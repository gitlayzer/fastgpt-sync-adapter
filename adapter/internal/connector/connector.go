// Package connector 定义数据源连接器接口。每个数据源（企微/飞书/…）一个实现。
// 连接器只负责「从源端把数据原样拉出来」，落地去哪由 Sink 决定。
package connector

import (
	"context"
	"io"

	"fastgpt-sync-adapter/adapter/internal/model"
)

// Connector 数据源连接器。
type Connector interface {
	// Name 连接器标识，如 "wecom"。同时作为状态文件里 checkpoint 的 key。
	Name() string

	// Init 完成认证/资源初始化（SDK 加载、登录等）。在首次 Fetch 前调用一次。
	Init(ctx context.Context) error

	// Fetch 增量拉取：从 checkpoint 之后拉一批（最多 limit 条），返回记录与新游标。
	// checkpoint 为引擎持久化的游标字符串，格式由各连接器自定（企微是 seq 十进制）。
	// 首次启动 checkpoint 为空字符串。
	// 返回的 next 将在本批记录全部成功写入 Sink 后由引擎持久化。
	Fetch(ctx context.Context, checkpoint string, limit int) (records []model.Record, next string, err error)

	// FetchAttachment 按需拉取附件字节（附件可能大且多，懒加载避免内存膨胀）。
	FetchAttachment(ctx context.Context, a *model.Attachment) (io.ReadCloser, error)

	// Close 释放资源。
	Close() error
}
