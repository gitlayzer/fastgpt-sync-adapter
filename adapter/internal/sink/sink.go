// Package sink 定义数据出口接口。NAS、FastGPT 都是 Sink，可同时挂多个。
package sink

import (
	"context"
	"io"

	"fastgpt-sync-adapter/adapter/internal/model"
)

// AttachmentReader 由引擎注入：调用即经连接器拉取附件字节（懒加载）。
type AttachmentReader func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error)

// Sink 数据出口。
type Sink interface {
	Name() string

	// WriteRecord 写入一条记录。read 用于按需拉附件字节（无附件时可忽略）。
	// 实现方必须「原样写入」，不做内容加工（单向同步红线）。
	// 单条失败返回 error，由引擎隔离记录，不影响同批其他记录。
	WriteRecord(ctx context.Context, r model.Record, read AttachmentReader) error
}
