# fastgpt-sync-adapter

多源数据归集中间件：**企业微信/飞书 → 群晖 NAS → FastGPT 知识库**。

后台驻留 daemon，定时执行三段流水线。

## 架构

```
企业微信(会话存档) ─┐
                    ├─► 群晖 NAS（按用户建目录，单向落盘）─► FastGPT 知识库（上传源文件）
飞书 ──────────────┘
```

## 三方配置（config.yaml）

| 模块 | 配置项 | 用途 |
|---|---|---|
| **FastGPT** | `base_url` / `api_key` / `dataset_id` | 连接、创建知识库、上传文件；拉用户列表在 NAS 建目录 |
| **群晖 NAS** | `url` / `username` / `password` / `root_path` | 企微/飞书文件落盘位置（File Station WebAPI） |
| **企业微信** | `corp_id` / `secret` / `rsa_private_key` | 会话内容存档数据拉取（含消息记录+附件） |

完整配置模板见 [config.yaml](config.yaml)。

## 各模块说明

### 1. FastGPT
- 上传源文件到知识库（`create/localFile`），上传后调用 `collection/update` 修正 S3 随机文件名
- 增量判断：按内容 SHA1，变更先删旧再传新
- **用户列表通过 API 自动获取**，在 NAS 按用户名自动建目录（新增用户自动补建）
- 上传解析参数：`image_index`（图片索引）、`custom_pdf_parse` 等可配
- **注意**：用户列表接口需账号级 API key；知识库级 key（fastgpt-xxx）无团队权限，会导致用户目录无法自动创建

### 2. 群晖 NAS
- WebAPI：`auth.cgi` 登录拿 sid、`entry.cgi` 建目录/上传
- `MkdirAll` 递归建目录，`UploadFile` 上传（自动建父目录、覆盖更新）
- 落盘结构：`root_path/<用户名>/...`（目录随 FastGPT 用户列表自动创建）

### 3. 企业微信会话存档
- **SDK 形态（重要）**：官方 SDK **只提供动态库 `.so`，无静态库 `.a`**，所以**无法静态打包进二进制**，运行时必须带对应架构的 `.so`。
  - SDK 已内置到 `lib/amd64/` 和 `lib/arm64/`（版本 20250205）。
  - **解密由 SDK 内置 `DecryptData` 完成**（RSA+AES 都在库里），不需要我们手写加解密；纯 Go 的 `crypto.go` 作为无 SDK 时的兜底。
- **开发环境是 arm64（你 Mac），运行环境是 amd64（Linux 服务器）**，cgo 无法简单交叉编译，需分别在对应架构上编译：
  - **amd64（服务器部署）**：在 amd64 机器上或用 amd64 容器编译，链接 `lib/amd64`
  - **arm64（本机开发）**：链接 `lib/arm64`
- 需要 `CGO_ENABLED=1`。运行时通过 `LD_LIBRARY_PATH` 指向对应架构的库目录。
- 已实测：cgo 编译通过、`NewSdk`/`Init` 真实调通（库能正常加载初始化）。

### 编译命令（分架构）

```bash
# --- 不含企微 SDK（企微功能不可用，其余正常，可自由交叉编译）---
CGO_ENABLED=0 go build -o bin/adapter .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/adapter-mac .   # Mac

# --- 含企微 SDK（cgo，须在与目标一致的架构上编译）---
# arm64（本机/ARM 服务器）
CGO_ENABLED=1 CGO_LDFLAGS="-L$(pwd)/lib/arm64" go build -o bin/adapter-cgo-arm64 .
# amd64（Linux 服务器，须在 amd64 环境执行）
CGO_ENABLED=1 CGO_LDFLAGS="-L$(pwd)/lib/amd64" go build -o bin/adapter-cgo-amd64 .
```

### 运行（含企微 SDK 时）

```bash
# 运行前指定库路径（按架构选一）
LD_LIBRARY_PATH=./lib/arm64 ./bin/adapter-cgo-arm64 --config config.yaml
LD_LIBRARY_PATH=./lib/amd64 ./bin/adapter-cgo-amd64 --config config.yaml
```

## 编译

```bash
# 基础（不含企微 SDK，企微功能不可用，其余正常）
CGO_ENABLED=0 go build -o bin/adapter .

# 含企微会话存档（需 SDK 动态库）
CGO_ENABLED=1 go build -o bin/adapter .

# Mac Apple Silicon
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/adapter-mac .
```

## 启动

```bash
./bin/adapter --config config.yaml
```

## 待你提供 / 待联调

1. **群晖 NAS**：真实 url/账号/密码 → 我可实测登录、建目录、上传
2. **企微**：corpid + secret + RSA 私钥（SDK 已集成在 `lib/`，无需再提供）→ 联调会话存档拉取（数据带消息记录+附件）
3. **FastGPT 账号级 key**：用户列表要自动获取，需要能访问团队/用户接口的账号级 token（当前知识库级 key 拿不到）
4. 飞书连接器（第五章）尚未实现，骨架已预留
