package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
)

// ===== 企微会话存档数据结构 =====

// ChatDataItem get_chat_data 返回的单条加密记录。
type ChatDataItem struct {
	Seq              int64  `json:"seq"`
	MsgID            string `json:"msgid"`
	PublicKeyVer     int    `json:"publickey_ver"`
	EncryptRandomKey string `json:"encrypt_random_key"`
	EncryptChatMsg   string `json:"encrypt_chat_msg"`
}

// ChatDataResponse get_chat_data 解密前的响应。
type ChatDataResponse struct {
	ErrCode  int            `json:"errcode"`
	ErrMsg   string         `json:"errmsg"`
	ChatData []ChatDataItem `json:"chatdata"`
}

// Message 解密后的消息（结构随 msgtype 不同而不同，这里保留通用字段 + 原始 JSON）。
type Message struct {
	MsgID   string          `json:"msgid"`
	Action  string          `json:"action"` // send / recall / switch
	From    string          `json:"from"`
	ToList  []string        `json:"tolist"`
	RoomID  string          `json:"roomid"` // 群聊时非空
	MsgTime int64           `json:"msgtime"`
	MsgType string          `json:"msgtype"` // text/image/file/video/...
	Raw     json.RawMessage `json:"-"`       // 原始完整 JSON（原样落盘）
}

// SDK 抽象：真实拉取通过官方 C SDK（cgo），这里定义接口便于替换/测试。
//
// 两个拉取方法都接收 ctx：SDK 内部对 10001 做多次重试并带退避等待，
// 若不能在重试之间响应取消，进程停止时会把整批拉完才退出（实测可卡数分钟）。
type SDK interface {
	// GetChatData 拉取从 seq 开始的 limit 条加密记录。
	GetChatData(ctx context.Context, seq int64, limit int) (*ChatDataResponse, error)
	// GetMediaData 拉取媒体附件（sdkfileid），支持分片。返回文件字节。
	GetMediaData(ctx context.Context, sdkFileID, indexBuf string) (data []byte, nextIndexBuf string, isFinish bool, err error)
	// DecryptData SDK 内置解密：直接解 encrypt_random_key + encrypt_chat_msg。
	DecryptData(encryptRandomKey, encryptChatMsg string) (string, error)
}

// Archiver 会话存档拉取器：组合加解密与 SDK。
type Archiver struct {
	CorpID string
	Secret string
	Crypto *Crypto
	SDK    SDK // 由 cgo 实现注入
}

func NewArchiver(corpID, secret string, crypto *Crypto, sdk SDK) *Archiver {
	return &Archiver{CorpID: corpID, Secret: secret, Crypto: crypto, SDK: sdk}
}

// FetchAndDecrypt 从 seq 拉取并解密一批消息。
func (a *Archiver) FetchAndDecrypt(ctx context.Context, seq int64, limit int) ([]*Message, int64, error) {
	if a.SDK == nil {
		return nil, seq, fmt.Errorf("企微 SDK 未初始化（需要 libWeWorkFinanceSdk_C 动态库，见配置 wecom.sdk_lib_path）")
	}
	resp, err := a.SDK.GetChatData(ctx, seq, limit)
	if err != nil {
		return nil, seq, fmt.Errorf("get_chat_data: %w", err)
	}
	if resp.ErrCode != 0 {
		return nil, seq, fmt.Errorf("企微返回错误 errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	log.Printf("[wecom] GetChatData 返回 errcode=0，加密记录 %d 条", len(resp.ChatData))

	var out []*Message
	maxSeq := seq
	decryptFail := 0
	var firstErr error
	for _, item := range resp.ChatData {
		if item.Seq > maxSeq {
			maxSeq = item.Seq // 无论解密成败都推进游标，避免卡在同一批
		}
		// 正确的两步解密（这个 SDK 版本的 DecryptData 只做 AES）：
		// 1. 用我们的 RSA 私钥解 encrypt_random_key → 会话密钥（纯 Go，crypto.go）
		// 2. 会话密钥 + encrypt_chat_msg → SDK DecryptData 做 AES 解密
		//    若 SDK 解密失败，退回纯 Go 的 DecryptChatMsg 兜底
		var plain string
		sessionKey, kerr := a.Crypto.DecryptRandomKey(item.EncryptRandomKey)
		if kerr != nil {
			decryptFail++
			if firstErr == nil {
				firstErr = fmt.Errorf("RSA 解 random_key 失败: %w", kerr)
			}
			continue
		}
		if sdkWithDecrypt, ok := a.SDK.(interface {
			DecryptData(string, string) (string, error)
		}); ok {
			plain, kerr = sdkWithDecrypt.DecryptData(sessionKey, item.EncryptChatMsg)
		} else {
			plain, kerr = a.Crypto.DecryptChatMsg(sessionKey, item.EncryptChatMsg)
		}
		if kerr != nil {
			decryptFail++
			if firstErr == nil {
				firstErr = fmt.Errorf("AES 解消息体失败: %w", kerr)
			}
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(plain), &msg); err != nil {
			decryptFail++
			if firstErr == nil {
				firstErr = fmt.Errorf("消息 JSON 解析失败: %w", err)
			}
			continue
		}
		msg.Raw = json.RawMessage(plain)
		out = append(out, &msg)
	}
	if decryptFail > 0 {
		log.Printf("[wecom] 本批 %d 条中解密失败 %d 条，首个错误: %v", len(resp.ChatData), decryptFail, firstErr)
	}
	return out, maxSeq, nil
}
