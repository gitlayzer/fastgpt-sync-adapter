// 企业微信会话存档连接器：实现 connector.Connector 接口。
// 数据链路：官方 C SDK GetChatData → 解密（SDK 内置优先，纯 Go 兜底）→ 映射为 model.Record。
package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"fastgpt-sync-adapter/adapter/internal/model"
)

// Config 企微连接器配置（会话内容存档）。
type Config struct {
	CorpID string `yaml:"corp_id"` // 企业 corpid
	Secret string `yaml:"secret"`  // 会话存档 secret
	// RSA 私钥：可直接给 PEM 文本，或给文件路径（二选一）
	RSAPrivateKey     string `yaml:"rsa_private_key"`
	RSAPrivateKeyFile string `yaml:"rsa_private_key_file"`
	// 官方 SDK 动态库目录（libWeWorkFinanceSdk_C.so 所在目录，按运行架构选 lib/amd64 或 lib/arm64）
	SDKLibPath string `yaml:"sdk_lib_path"`
}

// Connector 企微会话存档连接器。
type Connector struct {
	cfg     Config
	arch    *Archiver
	sdk     SDK
	startSeq int64 // checkpoint 为空时的起始 seq（默认 0=从最早可得的拉）
}

func New(cfg Config) *Connector { return &Connector{cfg: cfg} }

func (c *Connector) Name() string { return "wecom" }

// Init 加载 RSA 私钥并初始化官方 SDK（cgo）。
func (c *Connector) Init(ctx context.Context) error {
	pemText, err := c.privateKeyPEM()
	if err != nil {
		return fmt.Errorf("读取 RSA 私钥: %w", err)
	}
	crypto, err := NewCrypto(pemText)
	if err != nil {
		return fmt.Errorf("解析 RSA 私钥: %w", err)
	}
	sdk, err := NewSDKFromLib(c.cfg.CorpID, c.cfg.Secret, c.cfg.SDKLibPath)
	if err != nil {
		return fmt.Errorf("企微 SDK 初始化: %w", err)
	}
	c.sdk = sdk
	c.arch = NewArchiver(c.cfg.CorpID, c.cfg.Secret, crypto, sdk)
	return nil
}

// Fetch 从 checkpoint（seq 十进制字符串，空=从头）拉一批并解密，映射为统一 Record。
func (c *Connector) Fetch(ctx context.Context, checkpoint string, limit int) ([]model.Record, string, error) {
	seq := c.startSeq
	if checkpoint != "" {
		v, err := strconv.ParseInt(checkpoint, 10, 64)
		if err != nil {
			return nil, checkpoint, fmt.Errorf("非法 checkpoint %q: %w", checkpoint, err)
		}
		seq = v
	}

	msgs, maxSeq, err := c.arch.FetchAndDecrypt(seq, limit)
	if err != nil {
		return nil, checkpoint, err
	}
	log.Printf("[wecom] GetChatData(seq=%d, limit=%d) → 解密成功 %d 条，maxSeq=%d", seq, limit, len(msgs), maxSeq)
	records := make([]model.Record, 0, len(msgs))
	for _, m := range msgs {
		records = append(records, messageToRecord(m))
	}
	return records, strconv.FormatInt(maxSeq, 10), nil
}

// FetchAttachment 按 sdkfileid 分片拉取附件完整字节（is_finish 前持续续传）。
// 网络类错误（ret=10001 等）做指数退避重试：附件在企微侧只留 ~3 天，丢不得。
func (c *Connector) FetchAttachment(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
	if c.sdk == nil {
		return nil, fmt.Errorf("企微 SDK 未初始化")
	}
	var buf bytes.Buffer
	index := ""
	for {
		var chunk []byte
		var next string
		var finish bool
		var err error
		// 单分片最多重试 4 次（1s/2s/4s/8s），续传点 index 保持不动
		for attempt := 0; ; attempt++ {
			chunk, next, finish, err = c.sdk.GetMediaData(a.FileID, index)
			if err == nil {
				break
			}
			if attempt >= 3 {
				return nil, fmt.Errorf("GetMediaData(%s) 重试耗尽: %w", a.FileID, err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		buf.Write(chunk)
		if finish {
			break
		}
		index = next
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func (c *Connector) Close() error { return nil }

func (c *Connector) privateKeyPEM() (string, error) {
	if c.cfg.RSAPrivateKey != "" {
		return c.cfg.RSAPrivateKey, nil
	}
	data, err := os.ReadFile(c.cfg.RSAPrivateKeyFile)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// messageToRecord 企微消息 → 统一 Record。Body 保留解密后的完整原始 JSON。
func messageToRecord(m *Message) model.Record {
	owners := make([]string, 0, len(m.ToList)+1)
	if m.From != "" {
		owners = append(owners, m.From)
	}
	owners = append(owners, m.ToList...)

	return model.Record{
		Source:      "wecom",
		Kind:        m.MsgType,
		ID:          m.MsgID,
		Timestamp:   m.MsgTime,
		OwnerIDs:    owners,
		SenderID:    m.From,
		GroupID:     m.RoomID, // 空=单聊
		Body:        m.Raw,
		Attachments: extractAttachments(m),
	}
}

// 带 sdkfileid 的顶层消息类型（混合消息/聊天记录等嵌套类型暂 TODO）。
var mediaMsgTypes = map[string]bool{
	"image": true, "file": true, "voice": true, "video": true, "emotion": true,
}

// extractAttachments 从原始消息 JSON 提取附件引用（sdkfileid 等）。
func extractAttachments(m *Message) []model.Attachment {
	if !mediaMsgTypes[m.MsgType] {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(m.Raw, &raw); err != nil {
		return nil
	}
	content, ok := raw[m.MsgType]
	if !ok {
		return nil
	}
	var body struct {
		SDKFileID string `json:"sdkfileid"`
		FileName  string `json:"filename"`
		FileSize  int64  `json:"filesize"`
		MD5Sum    string `json:"md5sum"`
	}
	if err := json.Unmarshal(content, &body); err != nil || body.SDKFileID == "" {
		return nil
	}
	name := body.FileName
	if name == "" {
		name = m.MsgID // 图片/语音无文件名时用 msgid
	}
	return []model.Attachment{{
		Name:   name,
		FileID: body.SDKFileID,
		Size:   body.FileSize,
		MD5:    body.MD5Sum,
	}}
}
