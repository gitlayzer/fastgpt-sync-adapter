package wecom

import (
	"encoding/json"
	"fmt"
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
type SDK interface {
	// GetChatData 拉取从 seq 开始的 limit 条加密记录。
	GetChatData(seq int64, limit int) (*ChatDataResponse, error)
	// GetMediaData 拉取媒体附件（sdkfileid），支持分片。返回文件字节。
	GetMediaData(sdkFileID, indexBuf string) (data []byte, nextIndexBuf string, isFinish bool, err error)
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
func (a *Archiver) FetchAndDecrypt(seq int64, limit int) ([]*Message, int64, error) {
	if a.SDK == nil {
		return nil, seq, fmt.Errorf("企微 SDK 未初始化（需要 libWeWorkFinanceSdk_C 动态库，见配置 wecom.sdk_lib_path）")
	}
	resp, err := a.SDK.GetChatData(seq, limit)
	if err != nil {
		return nil, seq, fmt.Errorf("get_chat_data: %w", err)
	}
	if resp.ErrCode != 0 {
		return nil, seq, fmt.Errorf("企微返回错误 errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}

	var out []*Message
	maxSeq := seq
	for _, item := range resp.ChatData {
		// 优先用 SDK 内置解密（更可靠）；SDK 无 DecryptData 时用纯 Go Crypto 兜底
		var plain string
		var derr error
		if sdkWithDecrypt, ok := a.SDK.(interface {
			DecryptData(string, string) (string, error)
		}); ok {
			plain, derr = sdkWithDecrypt.DecryptData(item.EncryptRandomKey, item.EncryptChatMsg)
		} else {
			plain, derr = a.Crypto.DecryptMessage(item.EncryptRandomKey, item.EncryptChatMsg)
		}
		if derr != nil {
			// 单条解密失败不阻塞整批，记录后跳过
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(plain), &msg); err != nil {
			continue
		}
		msg.Raw = json.RawMessage(plain)
		out = append(out, &msg)
		if item.Seq > maxSeq {
			maxSeq = item.Seq
		}
	}
	return out, maxSeq, nil
}
