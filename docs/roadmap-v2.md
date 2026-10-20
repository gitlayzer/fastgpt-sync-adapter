# fastgpt-sync-adapter v2 — 整体架构与开发周期

> 基于 2026-10-20 需求（5 条功能 + 授权 webhook + 按人建库主动推 FastGPT）整理。
> 与旧版（方案 B / serve 拉取）的关键差异：**FastGPT 改为中间件主动推**、**按人建知识库**、**新增飞书 OAuth 授权**、**新增身份映射**、**企微转 Markdown 按人双份存储**。

## 一、需求到模块的映射

| # | 需求 | 对应模块 | 状态 |
|---|------|---------|------|
| 1 | 企微对话 → Markdown，会话内全部人员各存一份 + 附件 | wecom connector（转 md、会话归组）+ NAS sink（按人写） | 改造 |
| 2 | 飞书全员云文档（用户授权后，SQLite 判断授权与 token 有效性） | feishu connector + auth webhook + token refresher | 新建 |
| 3 | 对接 NAS，按"人/平台"目录存（FastGPT-test/zhangsan/企微·飞书） | NAS sink 目录结构调整 | 改造 |
| 4 | 企微 userid →（多维表格）→ 飞书人员 → 姓名 → FastGPT 按人建库 → 推 NAS 文件 | identity 解析器（Bitable）+ fastgpt push sink | 新建 |
| 5 | 对外暴露 FastGPT API 文件库接口（列目录 + previewUrl，备用） | serve 子命令（保留） | 已有 |

授权 webhook：adapter 常驻一个 HTTP 端，接收飞书 OAuth 回调拿 code → 换 token → 存 SQLite，每小时刷新一次 token。授权与文件同步同一个进程。

## 二、整体架构

```
                         ┌────────────────────────────────────────────┐
                         │              adapter (单进程)              │
                         │                                            │
  企业微信 ──SDK/cgo──►  │  wecom connector ─┐                        │
  (会话存档 GetChatData) │  feishu connector ─┤   ┌──────────┐         │
  飞书云文档 ──API────►  │  (tenant/user 两种) ├─► │  engine  │         │
                         │                   │   │ 调度/游标 │         │
                         │  auth webhook (HTTP)│   └────┬─────┘         │
   飞书OAuth回调 ─────►  │  /oauth/feishu/callback │      │ Record      │
                         │  token refresher (1h)  │      ▼             │
                         │  identity resolver ◄───┤  ┌─────────────┐   │
   飞书多维表格 ◄──────  │  (Bitable 查身份映射)   │  │  sink 管线   │   │
                         │                        │  │             │   │
                         │  storage (SQLite) ◄────┼──┤ NAS sink    │──►│─► NAS
                         │  oauth/identity/file   │  │ fastgpt sink│──►│─► FastGPT(按人建库)
                         │                        │  └─────────────┘   │
                         │  serve 子命令(备用 previewUrl) ────────────►│─► FastGPT API文件库
                         └────────────────────────────────────────────┘
```

## 三、各模块设计要点

### 1. storage（SQLite，新建）
用纯 Go 驱动 `modernc.org/sqlite`（**不依赖 cgo**，避免和企微 SDK 的 cgo 冲突、交叉编译也省事）。表：
- `oauth_grants`：platform / external_user_id / code / access_token / refresh_token / expire_at / refresh_expire_at / status（authorized/expired/revoked）
- `identities`：wecom_user_id / feishu_user_id / person_name / updated_at（身份映射缓存）
- `file_index`：person / source / nas_path / category / fastgpt_collection_id / frozen / checksum（幂等推送台账）
- `checkpoint`：连接器游标（可由现在的 JSON 迁入，可选）

### 2. auth webhook + token refresher（新建）
- `GET /oauth/feishu/start?user=<企微或姓名>` → 302 跳转飞书授权页（state 里带身份线索）
- `GET /oauth/feishu/callback` → code 换 user_access_token → 写 `oauth_grants`
- `GET /oauth/status` → 已授权/未授权名单（便于催收授权）
- refresher：每小时扫 `oauth_grants`，临近过期（剩 <10min）就用 refresh_token 换新并写回（滚动续期，30 天内不断授权）
- 复用 `tools/feishu_oauth_server.py` 的逻辑，只是改成 Go、常驻、写 SQLite

### 3. identity resolver（新建）
- Bitable 客户端：读你提供的多维表格，缓存进 `identities`
- 解析链：企微 userid →（表）→ 飞书 user → 姓名；企微/飞书两侧的记录都归一到同一个 `person_name`
- 同时决定 NAS 目录名 和 FastGPT 知识库名

### 4. wecom connector 改造
- 现有：GetChatData 拉取 + 两步解密 + 附件分片拉取（保留）
- 新增：**会话归组**（单聊按对方、群聊按 roomid，按天/时间窗切片）→ **渲染 Markdown**
- 新增：一条会话写入**会话内全部人员**各自的目录（owner_ids 里每个人一份）
- NAS 落盘从 `<平台>/<发送者>/...` 改为 `<人名>/企微/...`（见下）

### 5. feishu connector（新建）
- 两种模式：
  - **tenant 模式**：tenant_access_token 遍历共享文件夹/知识库（无需授权，先做）
  - **user 模式**：对每个 `oauth_grants.status=authorized` 的用户，用其 user_access_token 拉其云文档（含"归属别人但我可见"的副本，按需求 4 也要存）
- 拉取正文 + 附件，归档到 `<人名>/飞书/...`
- 注意 drive 下载限流（5 QPS、约 1 万次/天/应用），全量要排队+断点续传

### 6. NAS sink 改造（目录结构）
按你给的截图（`FastGPT-test/zhangsan/企微`、`FastGPT-test/zhangsan/飞书`）：
```
<root>/<人名>/企微/messages|<会话>/<msgid>.md + 附件
<root>/<人名>/企微/<分类>/<附件>
<root>/<人名>/飞书/<分类>/<文档/附件>
```
人名取自 identity resolver；`.meta.json` 侧边车（分类/权限快照/冻结）保留。

### 7. fastgpt push sink（复活方案 A）
- 对每个 person：按名在 FastGPT 查/建知识库（模型用 emb4 / qwen3.5-plus / qwen3.8-flash）
- 把该 person 在 NAS 的文件经 uploader 推上去（带分类标签；中文文件名走已修好的 RFC5987）
- `file_index` 记录 nas_path ↔ collection_id，保证幂等（改了才重传、删了按策略处理）
- 可选：配合登录态 token 调协作者 API 设文件权限（待你确认是否本期要）

### 8. serve 子命令（保留，需求 5）
现有 NAS→previewUrl 接口不动，作为备用通道。

## 四、运行形态

单二进制两子命令：
- `./adapter`（daemon）：engine + connectors + NAS/fastgpt sinks + **auth webhook** + **token refresher** + identity 解析
- `./adapter serve`（备用 previewUrl）

## 五、开发周期（约 11 人日，不含外部前置等待）

| 阶段 | 内容 | 工期 |
|------|------|------|
| P0 | config 扩展、SQLite 存储层与建表、迁移脚本 | 0.5 d |
| P1 | auth webhook + token refresher（含 status 页） | 1.5 d |
| P2 | identity resolver（Bitable 客户端 + identities 缓存） | 1.0 d |
| P3 | 企微：会话归组 + Markdown 渲染 + 按人多份 + NAS 新结构 | 2.0 d |
| P4 | 飞书 connector（tenant+user 双模式、文档/附件、限流/断点） | 2.5 d |
| P5 | fastgpt push sink（按人建库、上传、标签、幂等映射） | 1.5 d |
| P6 | serve 校验 + 端到端联调 | 1.0 d |
| P7 | 加固（重试/限流/日志）+ 部署 + 文档 | 1.0 d |

## 六、外部前置条件（不在工时内，需你提供）

1. 飞书自建应用 app_id/secret + 开通云文档读取、多维表格读取权限 + 配置 OAuth 回调地址
2. 多维表格：所在位置（app_token/table_id）+ 列结构（哪列企微 userid、哪列飞书 id、哪列姓名）
3. FastGPT：若本期要设文件权限，提供登录态 token 或先设账号密码（用于自动登录换 token）
4. NAS 新根目录结构确认（人名用中文还是拼音/工号）
5. 企业微信会话存档 corpid/secret/RSA（已有）

## 七、开放问题 → 已拍板（2026-10-20）

1. ✅ NAS 第一层人名：以**飞书多维表格通过企微 userid 查出的人员姓名**为准。多维表格暂未提供（TODO），identity resolver 做成接口，先用本地映射文件兜底，表格到位后切换。
2. ✅ FastGPT：**一个人一个知识库**；可以打标签（用户担心文件类型分不清 → 标签设计时把"分类"和"来源平台"分开打：如 `分类=合同`、`来源=企微`）。
3. ✅ 企微会话 Markdown：**滚动追加**（同一会话同一文件持续追加，不按天切片）；**群聊内每个成员目录都存一份**完整记录。
4. ✅ 文档级权限配置（协作者 API）：**本期不做**。
5. ⏸ 飞书"个人空间全量（归集账号批量转移所有权）"：本期只做**用户授权模式**（OAuth webhook），归集账号方案后续再议。
6. ⏸ 多维表格：未拿到，身份映射先 TODO。
