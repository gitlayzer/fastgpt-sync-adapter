# 连接器（Connector）架构设计

> 定位：fastgpt-sync-adapter 重新定义为**多源数据连接器平台**。
> 核心是一个可插拔的连接器框架，每个连接器负责从一个数据源拉取数据；
> 当前首个连接器为**企业微信（会话存档）**，飞书为规划中的第二个连接器。
> 数据出口（NAS、FastGPT）与连接器解耦，所有连接器复用同一套出口。

## 1. 总体分层

```
┌─────────────────────────────────────────────────────────────┐
│                      数据源（外部平台）                        │
│   企业微信会话存档        飞书(TODO)        未来：钉钉/邮件/…   │
└──────────────┬──────────────────┬───────────────────────────┘
               │                  │
┌──────────────▼──────────────────▼───────────────────────────┐
│                  连接器层 Connectors（可插拔）                 │
│  ┌──────────────────────┐  ┌──────────────────────┐          │
│  │ wecom connector      │  │ feishu connector     │  …       │
│  │ 会话存档SDK/解密/附件 │  │ 开放平台API/文档      │          │
│  └──────────┬───────────┘  └──────────┬───────────┘          │
└─────────────┼─────────────────────────┼───────────────────────┘
              │      统一数据模型（Record / Attachment / Meta）
┌─────────────▼─────────────────────────▼───────────────────────┐
│                     核心引擎 Core Engine                       │
│  调度器(ticker) · 增量状态(seq/sha1游标) · 重试退避 · 日志指标  │
└─────────────┬─────────────────────────┬───────────────────────┘
              │                         │
┌─────────────▼──────────────┐  ┌───────▼───────────────────────┐
│   Sink: NAS（群晖WebAPI）   │  │  Sink: FastGPT（知识库API）    │
│   原样落盘 / 按用户建目录    │  │  上传+重命名 / API文件库外链    │
└────────────────────────────┘  └───────────────────────────────┘
```

一句话：**连接器只负责"从源端把数据原样拉出来"，落地去哪里、怎么落地由 Sink 决定**。

## 2. 核心接口（Go）

### 2.1 统一数据模型

```go
// 所有连接器产出的统一数据结构
type Record struct {
    Source    string          // 来源标识: "wecom" / "feishu" / ...
    Kind      string          // 数据种类: "chat_message" / "file" / "doc" / ...
    ID        string          // 源端唯一ID（msgid / 文档token / 文件路径）
    Timestamp int64           // 源端时间（毫秒）
    OwnerIDs  []string        // 归属人ID列表（权限元数据，发送者/协作者等）
    GroupID   string          // 群/空间标识（企微 roomid / 飞书文档空间）
    Body      json.RawMessage // 原始内容，原样保留不加工
    Attachments []Attachment  // 附件引用
}

type Attachment struct {
    Name     string // 原始文件名
    FileID   string // 源端文件标识（企微 sdkfileid / 飞书 file_token）
    Size     int64
    MD5      string
    // 实际字节由连接器按需拉取（懒加载，避免消息多附件时内存膨胀）
}
```

### 2.2 连接器接口

```go
// Connector 数据源连接器：每个源一个实现，注册进引擎。
type Connector interface {
    Name() string                                  // "wecom"
    Init(cfg ConnectorConfig) error                // 各自的认证初始化
    // 增量拉取：从 checkpoint 之后拉一批，返回新游标。
    // 附件字节不在这里取，见 FetchAttachment。
    Fetch(ctx context.Context, checkpoint string, limit int) (records []Record, next string, err error)
    FetchAttachment(ctx context.Context, a *Attachment) (io.ReadCloser, error) // 按需拉附件字节
    Close() error
}
```

### 2.3 Sink 接口

```go
// Sink 数据出口：NAS、FastGPT 都是 Sink，可多挂。
type Sink interface {
    Name() string
    WriteRecord(ctx context.Context, r Record, read func(a *Attachment) (io.ReadCloser, error)) error
}
```

### 2.4 引擎职责

引擎不感知任何具体平台，只做四件事：

1. **调度**：按配置间隔驱动每个连接器的 `Fetch`（各连接器独立间隔）。
2. **状态**：每个连接器一条 checkpoint（企微是 seq，飞书是 edit_time/游标），落盘 JSON，崩溃续跑。
3. **管线**：`Fetch → 逐条 Record → 所有已启用 Sink 的 WriteRecord`，单条失败隔离重试，不阻塞整轮。
4. **观测**：结构化日志 + 每轮统计（拉取数/落盘数/失败清单）。

## 3. 企业微信连接器（内部结构）

```
connector/wecom/
├── connector.go     // 实现 Connector 接口：组装下列组件，Fetch = 拉+解+组装 Record
├── sdk_cgo.go       // cgo 封装 libWeWorkFinanceSdk_C（已有）
├── sdk_nocgo.go     // 无 cgo 环境的降级 stub（已有）
├── crypto.go        // RSA+AES-GCM 解密兜底（已有）
├── types.go         // ChatDataItem/Message 等结构（已有 archiver.go，改名归位）
└── media.go         // GetMediaData 分片拉附件（is_finish 续传循环）
```

映射关系：企微 `msgid→Record.ID`，`from+tolist→OwnerIDs`，`roomid→GroupID`，`msgtime→Timestamp`，解密后的完整 JSON→`Body`（原样保留），`sdkfileid→Attachment.FileID`。checkpoint 就是 `seq`。

## 4. Sink 现状

| Sink | 模块 | 行为 |
|---|---|---|
| NAS（归档） | `internal/sink/nas` | Record.Body 落 `<root>/<source>/<发送者>/<群ID\|direct>/<ID>.json`；附件经连接器回调拉字节后原样落 `attachments/` |
| FastGPT（直传） | `internal/sink/fastgpt` | 只上传文件附件（消息体不进知识库）：上传 + 二次调 API 改回真实文件名；按 sdkfileid 去重 |

方案 B（FastGPT API 文件库 serve 模式，`internal/server` + `store`）保留为独立子命令，不参与 daemon 管线。

## 5. 配置结构（按连接器组织）

```yaml
engine:
  state_file: ./connector_state.json   # 各连接器 checkpoint 统一存这里
  run_on_start: true
connectors:
  wecom:
    enabled: true
    interval: 60s                      # 该连接器的拉取间隔
    batch_limit: 100
    corp_id: ""
    secret: ""
    rsa_private_key_file: ""
    sdk_lib_path: ./lib/arm64
  feishu:                              # TODO，占位
    enabled: false
sinks:                                 # 出口二选一，同时只能启用一个
  nas:                                 # 归档出口：消息体+附件原样落盘
    enabled: true
    url: https://nas.example.com:5001
    username: FastGPT
    password: ""
    root_path: /FastGPT-test
    user_dirs:                         # 按 FastGPT 用户列表自动建目录（人工上传区）
      enabled: false
      fastgpt_base_url: ""
      fastgpt_api_key: ""              # 需账号级 key
  fastgpt:                             # 直传出口：仅文件附件进知识库（字节直供，不经本地目录）
    enabled: false
    base_url: ""
    api_key: ""
    dataset_id: ""
    image_index: true
    state_file: ./fastgpt_upload_state.json  # 附件按 sdkfileid 去重
```

**关键决策（已确认）**：
- FastGPT 知识库只进**文件附件**，聊天消息 JSON 只归档到 NAS，不进知识库。
- NAS 与 FastGPT 两个出口**二选一**（归档 vs 直传），配置校验强制互斥。
- NAS 落盘目录**按发送者归属**：`<root>/<来源>/<发送者>/<群ID|direct>/`。
- **无本地目录依赖**：附件字节由连接器经回调直供 Sink，全链路不扫盘、不挂载。
- serve 子命令（方案 B，FastGPT 经 previewUrl 来拉）保留，与 daemon 互不影响。

## 6. 目录结构规划（目标态 vs 现状）

```
adapter/
├── main.go                        // 启动：加载配置 → 注册连接器 → 注册Sink → 引擎Run
├── internal/
│   ├── engine/                    // 【新增】调度/状态/管线/重试
│   ├── model/                     // 【新增】Record/Attachment 统一模型
│   ├── connector/
│   │   ├── connector.go           // 【新增】Connector 接口 + 注册表
│   │   └── wecom/                 // 【迁移】现 internal/wecom 整体迁入并实现接口
│   ├── sink/
│   │   ├── nas/                   // 【迁移】现 internal/synology + 落盘编排
│   │   └── fastgpt/               // 【迁移】现 internal/uploader + syncer
│   └── config/                    // 配置结构按第 5 章重组
└── lib/{amd64,arm64}/             // 企微官方动态库（不变）
```

现有代码全部复用，主要是**搬迁归位 + 抽出接口**，不重写逻辑。

## 7. 扩展新数据源的成本

以飞书为例：新建 `connector/feishu/`，实现 `Connector` 接口三个方法（Init/Fetch/FetchAttachment），配置加一节，注册一行——引擎、状态、Sink、部署全部零改动。这就是连接器化的目的：**数据源增加 = 加一个插件**。
