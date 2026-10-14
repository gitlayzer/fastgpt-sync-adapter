# fastgpt-sync-adapter

多源数据**连接器平台**：连接器（Connector）从数据源拉数据，经统一数据模型交给出口（Sink）。
当前连接器：企业微信（会话存档）；出口：群晖 NAS 归档 / FastGPT 知识库直传（**二选一**）。
全程**无本地目录依赖**——附件字节由连接器直供 Sink，不扫盘、不挂载。

```
企业微信(会话存档SDK) ──► Connector ──► 引擎(调度/游标/管线) ──► Sink
飞书(TODO)            ──┘                                ├─► NAS 归档（消息+附件原样落盘）
                                                       └─► FastGPT 直传（仅附件进知识库）
```

## 两种运行模式

| 模式 | 命令 | 用途 |
|---|---|---|
| daemon（默认） | `./adapter --config config.yaml` | 连接器 → Sink 后台常驻管线 |
| serve 子命令 | `./adapter serve --root DIR --token T` 或 `bash start_serve_public.sh` | 方案 B：FastGPT API 文件库（previewUrl 文件服务器），独立模式 |

## 配置（config.yaml）

三段：`engine`（游标/启动行为）、`connectors`（数据源）、`sinks`（出口，二选一，校验强制互斥）。

```yaml
engine:
  state_file: "./connector_state.json"   # 连接器 checkpoint（企微 seq）
  run_on_start: true

connectors:
  wecom:                    # 企业微信会话存档
    enabled: false          # 配好凭证后改 true
    interval: 60s           # 拉取间隔（附件企微侧只留 ~3 天，务必远小于它）
    batch_limit: 100
    corp_id: ""             # 企业 corpid
    secret: ""              # 会话存档 secret
    rsa_private_key_file: "/path/to/private.pem"   # 或 rsa_private_key 直接贴 PEM
    sdk_lib_path: "./lib/arm64"      # Mac 开发 arm64；生产 amd64 改 ./lib/amd64

sinks:
  nas:                      # 出口一：归档（消息 JSON + 附件原样落盘）
    enabled: true
    url: "https://nas.example.com:5001"
    username: "FastGPT"
    password: "CHANGE_ME"
    root_path: "/FastGPT-test"       # 落 <root>/<来源>/<发送者>/<群ID|direct>/<msgid>.json
    user_dirs:                       # 按 FastGPT 用户列表自动建目录（人工上传区）
      enabled: false                 # 需账号级 api_key
      fastgpt_base_url: "https://aanshixbkhrk.sealoshzh.site"
      fastgpt_api_key: ""

  fastgpt:                  # 出口二：直传知识库（仅文件附件，消息体不进）
    enabled: false
    base_url: "https://aanshixbkhrk.sealoshzh.site"
    api_key: "fastgpt-xxx"           # 知识库级即可
    dataset_id: "..."
    image_index: true                # 解析参数（对应 WebUI）
    state_file: "./fastgpt_upload_state.json"  # 附件按 sdkfileid 去重
```

## 编译与运行

```bash
# 不含企微（纯 Go，可自由交叉编译）
CGO_ENABLED=0 go build -o bin/adapter .

# 含企微（cgo 链官方 .so，须在目标架构机器上编译）
CGO_ENABLED=1 go build -o bin/adapter .          # 本机架构；cgo 按 GOARCH 自动选 lib/amd64 或 lib/arm64

# 运行（企微需指定动态库路径）
LD_LIBRARY_PATH=./lib/arm64 ./bin/adapter --config config.yaml    # Mac/arm64
LD_LIBRARY_PATH=./lib/amd64 ./bin/adapter --config config.yaml    # Linux/amd64 生产
```

企微 SDK 说明：官方只发动态库（版本 20250205，已内置 `lib/amd64` + `lib/arm64`），无法静态打包；解密由 SDK 内置 `DecryptData` 完成，纯 Go 实现作兜底。

## 运行流程（daemon）

1. 启动：加载配置（只校验 enabled 模块）→ 登录 NAS / 注册 FastGPT Sink → 可选按 FastGPT 用户列表建 NAS 目录 → 初始化企微 SDK → 引擎读游标启动。
2. 每轮：连接器按 `interval` 增量拉（seq 游标）→ 解密 → 映射为 Record（原始 JSON 不动）→ 逐条写入 Sink：
   - NAS：消息落 JSON，附件按 sdkfileid 分片拉取原样落盘；
   - FastGPT：仅附件直传知识库（上传 + 重命名修正 S3 随机名），按 sdkfileid 去重。
3. 整批写完才推进游标落盘——崩溃最多重拉一批，不丢数据。单条失败隔离，不阻塞整批。

## 待联调

1. **企微**：corpid + secret + RSA 私钥 → 真实会话存档拉取联调
2. **FastGPT 账号级 key**：`user_dirs` 自动建目录需要（知识库级 key 无用户列表权限）
3. 飞书连接器：TODO，骨架已预留（实现 `Connector` 接口三方法即可接入）
