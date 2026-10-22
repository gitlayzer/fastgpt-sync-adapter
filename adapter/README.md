# fastgpt-sync-adapter

多源数据**连接器平台**：连接器（Connector）从数据源拉取数据，经统一数据模型归档到群晖 NAS，
再由推送任务按人把文件推送到 FastGPT 对应知识库；同时保留 serve 模式作为备用的 previewUrl 供给方式。

```
企业微信(会话存档 SDK) ──┐
飞书云文档(tenant/user) ──┴─► Connector ──► 引擎(调度/游标/去重) ──► NAS 归档 Sink
                                                                    │  <人名>/<平台>/{messages,conversations,<分类>}
                                                                    ▼
                                                        FastGPT 推送 Sink（按人建库、幂等、标签）
                                                                    │
                                                                    ▼
                                                        FastGPT 知识库（一人一库）
```

## 运行模式

| 模式 | 命令 | 用途 |
|---|---|---|
| daemon（默认） | `./adapter --config config.yaml` | 连接器 → NAS 归档 + FastGPT 按人推送（后台常驻） |
| serve 子命令 | `./adapter serve --nas-url ... --nas-root ...` | 读 NAS 暴露 API 文件库给 FastGPT；`--root` 可退化为本地目录调试 |

## 快速开始

```bash
# 1. 准备配置（不要提交 config.yaml，里面是真实凭证）
cp config.example.yaml config.yaml
cp identity_map.example.json identity_map.json

# 2. 编辑 config.yaml，填入企微 / 飞书 / NAS / FastGPT 凭证

# 3. 编译
CGO_ENABLED=0 go build -o bin/adapter .          # 不含企微（纯 Go，可交叉编译）
CGO_ENABLED=1 go build -o bin/adapter .          # 含企微（cgo 链官方 .so，须在目标架构机器上编译）

# 4. 运行（企微需指定动态库路径）
LD_LIBRARY_PATH=./lib/arm64 ./bin/adapter --config config.yaml    # Mac/arm64
LD_LIBRARY_PATH=./lib/amd64 ./bin/adapter --config config.yaml    # Linux/amd64 生产
```

## 配置要点

配置项完整注释见 `config.example.yaml`。几个关键点：

**企微**需要 corpid、会话存档 secret、RSA 私钥三件套。附件在企微侧只保留约 3 天，
`interval` 必须远小于这个窗口，否则 `GetMediaData` 会因文件过期失败。

**飞书**支持 `mode: tenant`（应用身份遍历共享文件夹）、`user`（按已授权用户逐个拉个人云文档）、
`both`。user 模式依赖 `auth` 段的 OAuth webhook，授权链接会显式请求
`offline_access drive:drive search:docs:read auth:user.id:read`。
`offline_access` 是拿到 refresh_token 的前提，缺失时 user_access_token 两小时后就会失效且无法续期。

**NAS** 路径结构为 `<root>/<人名>/<企微|飞书>/`，下分 `messages/`（原始 JSON）、
`conversations/`（滚动追加的会话 Markdown）、`<分类>/`（附件 + `.meta.json` 侧边车）。
Docker 部署时可用环境变量 `NAS_URL` 覆盖配置里的地址。

**FastGPT 推送**按人名查找或创建知识库，用 SQLite 的 `file_index` 做 NAS 文件到 collection 的幂等映射。
标签使用字符串前缀形式（`分类:合同`、`来源:企微`）——对象式标签要求知识库内预先存在标签定义，
公开 API 没有对应的创建接口。`messages/*.json` 只做归档保真，不推入知识库。

## 授权入口

飞书 OAuth 的回调地址需在飞书开放平台后台配置为 `<外部地址>/oauth/feishu/callback`。
启动后访问：

```
http://127.0.0.1:8089/oauth/feishu/start     # 发起授权
http://127.0.0.1:8089/oauth/status           # 查看已授权用户与 token 状态
http://127.0.0.1:8089/oauth/feishu/refresh   # 手动触发刷新
```

授权记录存于 SQLite 的 `oauth_grants` 表，token 刷新由引擎周期任务执行（默认 1 小时）。

## serve 模式

```bash
# 读 NAS
./adapter serve --nas-url "https://nas.example.com:5001" --nas-user <user> --nas-pass <pass> \
  --nas-root "/FastGPT-test" --token <长随机串> --external-url <公网/内网地址>

# 调试：本地目录
./adapter serve --root /path/to/dir --token <token>
```

serve 模式特性：previewUrl 字节流直出 NAS；`.meta.json` 对 FastGPT 不可见；
meta 中 `frozen: true` 的文件被过滤。OCR/解析由 FastGPT 侧承担。

## 目录结构

```
adapter/
├── main.go                 daemon 入口（连接器 → Sink）
├── serve.go                serve 子命令入口
├── config.example.yaml     配置模板
├── internal/
│   ├── auth/               飞书 OAuth webhook + token 刷新
│   ├── classifier/         规则分类器
│   ├── config/             配置加载与校验
│   ├── connector/          wecom / feishu 连接器
│   ├── engine/             调度、游标、管线
│   ├── feishu/             飞书 API 客户端（token/drive/docx）
│   ├── identity/           企微 userid → 人员姓名解析
│   ├── render/             会话 Markdown 渲染
│   ├── sink/               nas 归档 / fastgpt 推送
│   ├── storage/            SQLite（授权/身份/台账/去重）
│   └── uploader/           FastGPT 上传与知识库管理
└── lib/                    企微官方动态库（amd64 / arm64）
```

## 注意事项

`lib/` 下的 `libWeWorkFinanceSdk_C.so` 是企业微信官方 SDK 动态库，受企微许可约束，
分发前请确认合规。仓库不包含任何真实凭证、私钥或运行时数据库。
