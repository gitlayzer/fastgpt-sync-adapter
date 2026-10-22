# fastgpt-sync-adapter

把**企业微信会话存档**和**飞书云文档**归集到群晖 NAS，并按人推送到 FastGPT 知识库的中间件。

它是整条 AI 应用链里的**数据供给层**：上游对接数据源，下游给 FastGPT Agent 应用层供料。
adapter 只负责拉取、解密、授权、身份映射、分类、归档和增量同步，不做问答，也不做工作流。

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

核心行为：

企业微信会话存档解密后转成 Markdown，按会话滚动追加；群聊内的每个成员都会在自己的目录下拿到一份完整记录和附件。飞书侧通过 OAuth 用户授权读取云文档，`user_access_token` 和 `refresh_token` 存 SQLite 并定时刷新。企微 userid 经身份映射解析成人员姓名，作为 NAS 第一层目录。每个人对应一个 FastGPT 知识库，文件带分类和来源平台标签。

## 目录

```
adapter/     Go 服务本体（daemon + serve 两种模式）
docs/        需求分析、架构设计、开发方案
```

详细用法见 [adapter/README.md](adapter/README.md)。

## 快速开始

```bash
cd adapter
cp config.example.yaml config.yaml
cp identity_map.example.json identity_map.json
# 填入企微 / 飞书 / NAS / FastGPT 凭证
CGO_ENABLED=1 go build -o bin/adapter .
LD_LIBRARY_PATH=./lib/arm64 ./bin/adapter --config config.yaml
```

## 构建说明

不含企微时可用 `CGO_ENABLED=0` 自由交叉编译。含企微时需要 cgo 链接企业微信官方动态库
（`lib/amd64` 或 `lib/arm64`），必须在目标架构的机器上构建。

## 安全

仓库不包含任何真实凭证、私钥或运行时数据库。`config.yaml`、`private.pem`、`adapter.db`、
`identity_map.json` 均被 `.gitignore` 忽略，请只提交 `*.example.*` 模板。

`lib/` 下的企业微信 SDK 动态库受企微许可约束，分发前请确认合规。
