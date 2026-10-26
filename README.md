# fastgpt-sync-adapter

把**企业微信会话存档**和**飞书云文档**归集到群晖 NAS，并按人推送到 FastGPT 知识库的中间件与连接器平台。

它是整条企业 AI 知识链路里的**数据供给层**：上游对接数据源，下游给 FastGPT Agent 应用层供料。本服务只负责数据拉取、解密、授权、身份映射、分类、归档和增量同步，不承担模型问答与工作流编排。

```
企业微信 (会话存档 SDK) ──┐
飞书云文档 (tenant/user) ──┴─► Connector ──► 调度引擎 (游标/去重/限流) ──► NAS 归档 Sink
                                                                          │  <人名>/<平台>/{messages,conversations,<分类>}
                                                                          ▼
                                                              FastGPT 推送 Sink (按人建库/幂等/打标)
                                                                          │
                                                                          ▼
                                                              FastGPT 知识库 (一人一库)
```

---

## 核心特性

- **多源连接器架构**：
  - **企业微信**：对接官方会话存档 C-SDK，自动解密消息与媒体附件，群聊按成员分发归档，Markdown 自动去噪与单行压实。
  - **飞书云文档**：支持应用身份（tenant）与用户 OAuth 身份（user）双模式；支持云空间 Drive、知识库 Wiki，以及多维表格 Bitable；云文档源文件（docx/sheet）无损导出归档。
- **群晖 NAS 归档 (保真与防重)**：
  - 路径结构：`<root>/<人名>/<企微|飞书>/`，细分为 `messages/`（原始 JSON）、`conversations/`（滚动追加 Markdown）和 `<分类>/`（附件与 `.meta.json` 侧边车）。
  - 支持多成员会话 Fan-out 扇出，附件多引用去重，大文件断点续传。
- **FastGPT 推送 (一人一库)**：
  - 自动按人名检索或创建 FastGPT 对应知识库。
  - 基于 SQLite 做 NAS 文件到 FastGPT Collection 的幂等索引映射，避免重复解析。
  - 自动打标：按分类（合同/方案/报告等）和数据源（企微/飞书）打标。
- **双模运行**：
  - **daemon 模式**（默认）：后台常驻，调度连接器同步、NAS 归档与 FastGPT 知识库推送。
  - **serve 模式**（子命令）：直接作为 FastGPT 的 API 文件库提供 `previewUrl` 字节流代理。

---

## 项目结构

```text
fastgpt-sync-adapter/
├── cmd/
│   └── adapter/                 # 主程序入口 (支持 daemon 模式与 serve 子命令)
├── internal/                    # 核心内部包
│   ├── auth/                    # 飞书 OAuth Webhook 回调与 Token 定时刷新
│   ├── classifier/              # 规则分类器 (根据文件名识别合同/方案/数据表等)
│   ├── config/                  # 配置加载与结构体校验
│   ├── connector/               # 连接器抽象与实现 (wecom / feishu)
│   ├── engine/                  # 同步调度引擎与游标持久化
│   ├── fastgpt/                 # FastGPT 接口封装
│   ├── feishu/                  # 飞书开放平台 Client (Drive / Wiki / Bitable / Export)
│   ├── identity/                # 身份解析器 (本地 JSON / 飞书多维表格动态同步)
│   ├── model/                   # 核心数据模型 (Record, Attachment)
│   ├── nasstore/                # serve 模式下的 NAS 文件存储抽象
│   ├── render/                  # 企微消息 Markdown 渲染引擎与清洗器
│   ├── server/                  # serve 模式下的 HTTP API 文件库服务
│   ├── sink/                    # 数据出口 (NAS 归档 / FastGPT 推送)
│   ├── storage/                 # SQLite 本地存储 (OAuth Token / 去重 / 台账 / 游标)
│   ├── store/                   # 本地目录存储抽象 (用于本地无 NAS 调试)
│   ├── synology/                # 群晖 FileStation WebAPI Client
│   ├── uploader/                # FastGPT OpenAPI 上传器
│   └── version/                 # 版本与构建信息注入
├── lib/                         # 企微会话存档 C-SDK 头文件与 Linux 动态库
│   ├── WeWorkFinanceSdk_C.h
│   ├── amd64/libWeWorkFinanceSdk_C.so
│   └── arm64/libWeWorkFinanceSdk_C.so
├── scripts/
│   └── docker-dev.sh            # Docker 容器开发、连通性检测与中继脚本
├── Dockerfile                   # 生产级多阶段构建 Dockerfile
├── docker-compose.yml           # Docker Compose 一键启动配置
├── Makefile                     # 标准化构建、测试与运维指令
├── config.example.yaml          # 配置文件模板
├── identity_map.example.json    # 身份映射模板
└── SERVE.md                     # FastGPT API 文件库模式详细接口契约
```

---

## 快速开始

### 1. 准备配置

```bash
cp config.example.yaml config.yaml
cp identity_map.example.json identity_map.json
# 编辑 config.yaml 填入企微 / 飞书 / NAS / FastGPT 真实凭证
```

> **安全提示**：`config.yaml`、`private.pem`、`adapter.db` 与 `identity_map.json` 包含敏感凭据，已被 `.gitignore` 忽略，请切勿提交。

### 2. 本地构建与运行

#### 方式 A：纯 Go 模式 (无 CGO，无需企微动态库，支持交叉编译)

适用于仅使用飞书同步，或在非 Linux 环境测试：

```bash
make build-nocgo
./bin/adapter --config config.yaml
```

#### 方式 B：全量模式 (含企微 CGO 动态库，需要在目标架构 Linux 下构建)

```bash
# 1. 编译
make build

# 2. 运行 (需指定对应架构的动态库路径)
LD_LIBRARY_PATH=./lib/arm64 ./bin/adapter --config config.yaml   # ARM64 (Linux aarch64)
LD_LIBRARY_PATH=./lib/amd64 ./bin/adapter --config config.yaml   # AMD64 (Linux x86_64)
```

---

## 容器化部署

### 1. Docker Compose (推荐生产部署)

```bash
# 启动服务
docker compose up -d

# 查看日志
docker compose logs -f adapter
```

### 2. 原生 Docker 镜像构建

```bash
make docker-build
```

---

## 常用 Makefile 指令

| 命令 | 说明 |
|---|---|
| `make build` | 使用 CGO 编译主程序（需对应架构 C 编译器与动态库） |
| `make build-nocgo` | 纯 Go 编译主程序（无 CGO，跨平台静态编译） |
| `make test` | 运行项目所有单元测试 |
| `make test-coverage`| 运行单元测试并导出 HTML 覆盖率报告 |
| `make vet` | 执行 Go 代码静态检查 |
| `make docker-build` | 使用多阶段 Dockerfile 制作生产容器镜像 |
| `make clean` | 清理二进制产物与测试覆盖率文件 |

---

## 运行模式与子命令

### 1. 常驻同步模式 (Daemon)

```bash
./bin/adapter --config config.yaml
```

- 启动飞书 OAuth 授权服务：
  - 发起授权：`http://127.0.0.1:8089/oauth/feishu/start`
  - 状态查看：`http://127.0.0.1:8089/oauth/status`
  - 刷新凭据：`http://127.0.0.1:8089/oauth/feishu/refresh`
- 周期调度连接器拉取数据落盘 NAS，并增量同步至 FastGPT 对应人员的知识库。

### 2. FastGPT API 文件库模式 (Serve)

```bash
# 读群晖 NAS 暴露文件
./bin/adapter serve \
  --nas-url "https://nas.example.com:5001" \
  --nas-user "<user>" \
  --nas-pass "<pass>" \
  --nas-root "/FastGPT-test" \
  --token "<AuthToken>" \
  --external-url "https://adapter.example.com"

# 本地目录调试模式
./bin/adapter serve --root ./local-data --token "dev-token"
```

详细请求契约与协议说明见 [SERVE.md](SERVE.md)。

---

## 许可证与安全说明

1. 本项目开源发布，但 `lib/` 目录下的企业微信官方 SDK (`libWeWorkFinanceSdk_C.so` / `WeWorkFinanceSdk_C.h`) 版权归腾讯企业微信所有，分发前请遵守企微相关许可协议。
2. 敏感数据安全：切勿将生产私钥（`private.pem`）、配置文件（`config.yaml`）或包含会话历史的 SQLite 数据文件（`adapter.db`）提交至公开代码仓库。
