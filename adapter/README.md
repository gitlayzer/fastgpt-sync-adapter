# fastgpt-sync-adapter

多源数据**连接器平台**：连接器（Connector）从数据源拉数据，经统一数据模型归档到群晖 NAS；
FastGPT 通过 serve 模式（API 文件库 + previewUrl）**直接从 NAS 拉取**文件入库（方案 B 已选定）。
全程**无本地目录依赖**——daemon 不扫盘，serve 直接走群晖 WebAPI 读 NAS。

```
企业微信(会话存档SDK) ──► Connector ──► 引擎(调度/游标/管线) ──► NAS 归档 Sink
飞书(暂不做)         ──┘                    │                    （消息+附件原样落盘，
                                         分类器(可配规则)         附件按 平台/发送者/分类 归档）
FastGPT ──► serve 模式读 NAS（previewUrl 外链，FastGPT 拉到后自行解析/OCR）
```

**已定决策**：FastGPT 只走 serve 拉取（不做主动上传）；人工上传 NAS 的文件只归档不上传；
OCR 由 FastGPT 解析时承担，中间件不做任何内容加工；飞书暂缓，先把企微跑通。

## 两种运行模式

| 模式 | 命令 | 用途 |
|---|---|---|
| daemon（默认） | `./adapter --config config.yaml` | 连接器 → NAS 归档（后台常驻） |
| serve 子命令 | `./adapter serve --nas-url ... --nas-root ...` | 读 NAS 暴露 API 文件库给 FastGPT；`--root` 可退化为本地目录调试 |

## 配置（config.yaml）

```yaml
engine:
  state_file: "./connector_state.json"   # 连接器 checkpoint（企微 seq）
  run_on_start: true

connectors:
  wecom:                    # 企业微信会话存档（飞书暂缓）
    enabled: false          # 配好凭证后改 true
    interval: 60s           # 拉取间隔（附件企微侧只留 ~3 天，务必远小于它）
    batch_limit: 100
    corp_id: ""             # 企业 corpid（待客户提供）
    secret: ""              # 会话存档 secret（待客户提供）
    rsa_private_key_file: "/path/to/private.pem"   # 或 rsa_private_key 直接贴 PEM
    sdk_lib_path: "./lib/arm64"      # Mac 开发 arm64；生产 amd64 改 ./lib/amd64

classifier:                 # R4 文件用途分类（可配规则，只影响归档子目录，不动原文）
  enabled: true
  default_label: "其他"
  rules:                    # 自由增改：label=分类目录名，keywords=关键词
    - label: "合同"
      keywords: ["合同", "协议", "contract"]
    # ……见 config.yaml 完整默认规则

sinks:
  nas:                      # 归档出口（唯一）
    enabled: true
    url: "https://nas.example.com:5001"
    username: "FastGPT"
    password: "CHANGE_ME"
    root_path: "/FastGPT-test"
    # 落盘结构：<root>/<平台>/<发送者>/messages/<msgid>.json（消息原文）
    #           <root>/<平台>/<发送者>/<分类>/<附件名>（附件按分类归档）
    #           附件旁写 <附件名>.meta.json（分类/来源/权限快照/冻结标记）
    user_dirs:              # 按 FastGPT 用户列表自动建目录（人工上传区，需账号级 key，暂无）
      enabled: false
      fastgpt_base_url: ""
      fastgpt_api_key: ""
```

## serve 模式（FastGPT 入口）

```bash
# 生产：读 NAS（用户决策 7：serve 直接连 NAS）
./adapter serve --nas-url "https://nas.example.com:5001" --nas-user FastGPT --nas-pass 'xxx' \
  --nas-root "/FastGPT-test" --token <长随机串> --external-url <公网/内网地址>

# 调试：本地目录
./adapter serve --root /path/to/dir --token <token>

# 一键公网（cloudflared 隧道）
bash start_serve_public.sh
```

serve 模式特性：previewUrl 字节流直出 NAS；`.meta.json` 对 FastGPT 不可见；
meta 中 `frozen: true` 的文件被过滤（R6：权限回收后不再可被拉取）。
OCR/解析全部由 FastGPT 侧承担（previewUrl 提供原文件字节）。

## 编译与运行

```bash
# 不含企微（纯 Go，可自由交叉编译）
CGO_ENABLED=0 go build -o bin/adapter .

# 含企微（cgo 链官方 .so，须在目标架构机器上编译，自动选 lib/amd64 或 lib/arm64）
CGO_ENABLED=1 go build -o bin/adapter .

# 运行（企微需指定动态库路径）
LD_LIBRARY_PATH=./lib/arm64 ./bin/adapter --config config.yaml    # Mac/arm64
LD_LIBRARY_PATH=./lib/amd64 ./bin/adapter --config config.yaml    # Linux/amd64 生产
```

## 待联调（硬前置）

1. **企微三件套**：corpid + 会话存档 secret + RSA 私钥 → 企微真实数据联调（唯一点）
2. 通讯录/客户联系 secret 暂无 → NAS 目录先用企微 userid 命名，拿到后升级汉字姓名
3. FastGPT 账号级 key 暂无 → `user_dirs` 保持关闭

