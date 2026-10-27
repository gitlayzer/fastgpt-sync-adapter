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
	"path"
	"strconv"
	"strings"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
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

	msgs, maxSeq, err := c.arch.FetchAndDecrypt(ctx, seq, limit)
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
//
// 重试只在这里做一层：GetMediaData 本身已在 SDK 层对 10001 做了多次重试，
// 这里再套一层会让最坏情况变成 10×N 次调用（实测大文件能卡 3 分钟以上）。
// 因此这里只负责分片续传与取消，不再叠加退避重试。
//
// ctx 在分片之间检查：单次 GetMediaData 是同步 C 调用无法中断，
// 但至少能在多分片的大文件上及时响应退出信号。
func (c *Connector) FetchAttachment(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
	if c.sdk == nil {
		return nil, fmt.Errorf("企微 SDK 未初始化")
	}
	var buf bytes.Buffer
	index := ""
	const maxChunks = 5000 // 防止异常时无限循环（5000 片 × 512KB ≈ 2.5GB，远超任何企微附件）
	for part := 0; part < maxChunks; part++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		chunk, next, finish, err := c.sdk.GetMediaData(ctx, a.FileID, index)
		if err != nil {
			return nil, fmt.Errorf("GetMediaData(%s) 分片 %d 失败: %w", a.FileID, part+1, err)
		}
		buf.Write(chunk)
		if finish {
			return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
		}
		if next == "" {
			// 未完成却不给续传点，继续请求只会重复拿同一片
			return nil, fmt.Errorf("GetMediaData(%s) 分片 %d 未完成但无续传点", a.FileID, part+1)
		}
		index = next
	}
	return nil, fmt.Errorf("GetMediaData(%s) 分片数超过上限 %d，疑似续传点异常", a.FileID, maxChunks)
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

// 仅 file 类型会下载归档；图片/语音/视频/表情只在会话 Markdown 里记一笔。
// 混合消息、聊天记录等嵌套类型暂不处理（TODO）。

// extractAttachments 从原始消息 JSON 提取附件引用（sdkfileid 等）。
// 支持 file 直接文件、chatrecord 合并转发中的嵌套文件以及 image 图片。
func extractAttachments(m *Message) []model.Attachment {
	// 语音、视频、表情一律不下载，只下载办公文档与聊天原图。
	if m.MsgType != "file" && m.MsgType != "chatrecord" && m.MsgType != "image" {
		return nil
	}

	if m.MsgType == "image" {
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
			FileSize  int64  `json:"filesize"`
			MD5Sum    string `json:"md5sum"`
		}
		if err := json.Unmarshal(content, &body); err != nil || body.SDKFileID == "" {
			return nil
		}
		imgName := fmt.Sprintf("image_%s.jpg", m.MsgID)
		return []model.Attachment{{
			Name:   imgName,
			FileID: body.SDKFileID,
			Size:   body.FileSize,
			MD5:    body.MD5Sum,
		}}
	}

	if m.MsgType == "file" {
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
		if body.FileName == "" || !isWantedDoc(body.FileName) {
			log.Printf("[wecom] 跳过非文档附件 %q（类型不在白名单）", body.FileName)
			return nil
		}

		return []model.Attachment{{
			Name:   body.FileName,
			FileID: body.SDKFileID,
			Size:   body.FileSize,
			MD5:    body.MD5Sum,
		}}
	}

	if m.MsgType == "chatrecord" {
		var raw struct {
			ChatRecord struct {
				Item []struct {
					Type    string `json:"type"`
					Content string `json:"content"`
				} `json:"item"`
			} `json:"chatrecord"`
		}
		if err := json.Unmarshal(m.Raw, &raw); err != nil {
			return nil
		}
		var atts []model.Attachment
		for _, it := range raw.ChatRecord.Item {
			if it.Type == "ChatRecordFile" {
				var body struct {
					SDKFileID string `json:"sdkfileid"`
					FileName  string `json:"filename"`
					FileSize  int64  `json:"filesize"`
					MD5Sum    string `json:"md5sum"`
				}
				if err := json.Unmarshal([]byte(it.Content), &body); err == nil && body.SDKFileID != "" {
					if body.FileName != "" && isWantedDoc(body.FileName) {
						atts = append(atts, model.Attachment{
							Name:   body.FileName,
							FileID: body.SDKFileID,
							Size:   body.FileSize,
							MD5:    body.MD5Sum,
						})
					} else {
						log.Printf("[wecom] 跳过合并转发非文档附件 %q", body.FileName)
					}
				}
			}
		}
		return atts
	}

	return nil
}

// docExts 需要归档的文档扩展名白名单。
// 只收办公文档与 PDF；压缩包、可执行文件、图片音视频等一律不拉。
var docExts = map[string]bool{
	".pdf":  true,
	".doc":  true,
	".docx": true,
	".xls":  true,
	".xlsx": true,
	".csv":  true,
	".ppt":  true,
	".pptx": true,
	".txt":  true,
	".md":   true,
	".rtf":  true,
	".wps":  true, // WPS 文字
	".et":   true, // WPS 表格
	".dps":  true, // WPS 演示
}

// isWantedDoc 判断文件名是否属于要归档的文档类型。
func isWantedDoc(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return docExts[ext]
}
