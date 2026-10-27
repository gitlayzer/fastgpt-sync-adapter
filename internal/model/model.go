// Package model 定义所有连接器与 Sink 之间流转的统一数据模型。
package model

import "encoding/json"

// Record 一条从数据源拉取的数据记录（消息/文件/文档…），内容原样保留不加工。
type Record struct {
	Source    string          `json:"source"`    // 来源标识: "wecom" / "feishu" / ...
	Kind      string          `json:"kind"`      // 数据种类: "text" / "image" / "file" / "doc" / ...
	ID        string          `json:"id"`        // 源端唯一 ID（msgid / 文档 token / 文件路径）
	Timestamp int64           `json:"timestamp"` // 源端时间（毫秒）
	OwnerIDs  []string        `json:"owner_ids"` // 归属人 ID 列表（权限元数据：发送者/接收者/协作者）
	SenderID  string          `json:"sender_id"` // 发送者 ID（企微 from；目录归属按它）
	GroupID   string          `json:"group_id"`  // 群/空间标识（企微 roomid；空=单聊）
	Body      json.RawMessage `json:"body"`      // 原始内容（企微为解密后的完整消息 JSON，原样保留）

	Attachments []Attachment `json:"attachments,omitempty"` // 附件引用（字节按需经连接器拉取）
}

// Attachment 附件引用。字节不随 Record 走，由 Sink 通过连接器的 FetchAttachment 懒加载。
type Attachment struct {
	Name   string `json:"name"`              // 原始文件名（可能为空，如图片/语音）
	FileID string `json:"file_id"`           // 源端文件标识（企微 sdkfileid / 飞书 file_token）
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`
	UserID string `json:"user_id,omitempty"` // 扫描/持有权限的用户 ExternalUserID (open_id)，用于精确定向取 Token
}
