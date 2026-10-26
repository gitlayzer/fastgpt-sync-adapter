# API 文件库 serve 模式（previewUrl 外链版）

> 本分支 `api-preview-url`：最初的 FastGPT API 文件库需求，改为 **previewUrl 外链**方式。
> 服务兼任文件服务器，FastGPT 通过外链在线下载原文件并自行解析。

## 与原方案的区别

| | 原方案（main 分支初版） | 本分支 |
|---|---|---|
| `/v1/file/content` | 服务端提取文本，返回 `content` | 返回 `previewUrl` 外链 |
| 文件解析 | 服务端做（docx/xlsx/pdf 提取） | FastGPT 自己做（下载原文件解析） |
| 支持格式 | 受服务端提取能力限制 | FastGPT 支持的所有格式 |
| 本服务职责 | 提取 + 暴露 | 纯文件服务器（原样提供字节流） |

## 接口

| 方法 | 路径 | 返回 |
|---|---|---|
| POST | `/v1/file/list` | 文件树（`{parentId, searchKey}`） |
| GET | `/v1/file/content?id=` | `{title, previewUrl}` |
| GET | `/v1/file/read?id=` | `{url}`（原文查看链接） |
| GET | `/v1/file/detail?id=` | 文件详情 |
| GET | `/v1/file/download?id=&token=` | 文件字节流（previewUrl 指向这里） |
| GET | `/healthz` | 健康检查 |

## 启动

```bash
# 本机直连（FastGPT 与本机同网）
./bin/adapter serve --root /path/to/dir --token mytoken --addr :8080

# 经隧道/反代（FastGPT 在外网，previewUrl 必须是公网地址）
./bin/adapter serve --root /path/to/dir --token mytoken \
  --external-url https://xxx.trycloudflare.com
```

## FastGPT 配置

知识库 → 新建 → API 文件库：

- **baseURL** = 服务的外部地址（与 `--external-url` 一致）
- **authorization** = `--token` 的值
- **basePath** = 留空

## 关键点

1. **previewUrl 必须外网可达**。FastGPT 服务端拉取 previewUrl 时不带 Authorization 头，所以下载链接把 token 放在 query 参数里（`?id=xx&token=xx`）。download 端点同时接受 Bearer 头和 query token。
2. **`--external-url`**：经 cloudflared 等隧道时必填，否则 previewUrl 会拼成内网地址，FastGPT 拉不到。
3. 中文文件名按 RFC 5987 编码（`filename*=UTF-8''`），FastGPT 能正确识别。

## 守护进程模式不受影响

不带子命令时仍是同步 daemon（企微/飞书→NAS→FastGPT 上传），两种模式共存：

```bash
./bin/adapter serve --root ...        # API 文件库模式（本分支新增）
./bin/adapter --config config.yaml    # 定时上传 daemon（原有）
```
