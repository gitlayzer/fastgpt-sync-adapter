package feishu

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MailMessage 邮件信息。
// 主题要 mail:user_mailbox.message.subject:read，发件人/收件人要 mail:user_mailbox.message.address:read，
// 正文和附件要 mail:user_mailbox.message.body:read；缺哪个权限，接口就不返回对应字段（实测）。
type MailMessage struct {
	MessageID   string           `json:"message_id"`
	SMTPID      string           `json:"smtp_message_id"` // RFC 邮件 ID：发给自己的邮件在收件箱和已发送里 message_id 不同、这个相同（实测）
	Subject     string           `json:"subject"`
	HeadFrom    string           `json:"head_from"` // 发件人邮箱地址
	FromName    string           `json:"from_name"` // 发件人显示名
	To          []MailAddress    `json:"to"`
	Cc          []MailAddress    `json:"cc"`
	Bcc         []MailAddress    `json:"bcc"`
	CreatedTime int64            `json:"created_time"` // 毫秒
	BodyHTML    string           `json:"body_html"`    // 已解码
	BodyPlain   string           `json:"body_plain"`   // 已解码
	Attachments []MailAttachment `json:"attachments"`
}

// MailAddress 邮件地址。
type MailAddress struct {
	Address string `json:"mail_address"`
	Name    string `json:"name"`
}

// MailAttachment 邮件附件元数据。
type MailAttachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsInline bool   `json:"is_inline"`
	CID      string `json:"cid"` // 内联图片：正文 HTML 里用 cid:<CID> 引用
}

// MailFolder 邮箱文件夹。Type：1 系统文件夹，2 用户自建文件夹。
type MailFolder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type int    `json:"folder_type"`
}

// ListMailFolders 列出用户邮箱的全部文件夹（需要 mail:user_mailbox.folder:read）。
func (c *Client) ListMailFolders(userToken string) ([]MailFolder, error) {
	u := fmt.Sprintf("%s/mail/v1/user_mailboxes/me/folders", baseURL)
	var out struct {
		apiResp
		Data struct {
			Items []MailFolder `json:"items"`
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		return nil, err
	}
	return out.Data.Items, nil
}

// ListMailboxMessages 列出某个文件夹的一页邮件 ID（每页 20 封，接口上限），返回下一页标记和是否还有下一页。
// folder_id 必传：不传时接口报 4039（实测）。用户未开通飞书邮箱时返回空列表、不报错。
func (c *Client) ListMailboxMessages(userToken, folderID, pageToken string) ([]string, string, bool, error) {
	q := url.Values{}
	q.Set("page_size", "20")
	q.Set("folder_id", folderID)
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	u := fmt.Sprintf("%s/mail/v1/user_mailboxes/me/messages?%s", baseURL, q.Encode())
	var out struct {
		apiResp
		Data struct {
			HasMore   bool            `json:"has_more"`
			PageToken string          `json:"page_token"`
			Items     json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		errStr := err.Error()
		// 15180001 / 4013：用户未开通企业邮箱，静默返回空
		if strings.Contains(errStr, "15180001") || strings.Contains(errStr, "4013") {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	next, more := out.Data.PageToken, out.Data.HasMore
	if len(out.Data.Items) == 0 {
		return nil, next, more, nil
	}
	// 兼容支持 []string 数组格式
	var strIDs []string
	if err := json.Unmarshal(out.Data.Items, &strIDs); err == nil {
		return strIDs, next, more, nil
	}
	// 兼容支持 []struct{ ID string `json:"id"` } 对象数组格式
	var objIDs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Data.Items, &objIDs); err == nil {
		var ids []string
		for _, o := range objIDs {
			if o.ID != "" {
				ids = append(ids, o.ID)
			}
		}
		return ids, next, more, nil
	}
	return nil, "", false, fmt.Errorf("解析邮件列表失败: 无法识别 items 格式")
}

// GetMailMessage 获取某封邮件的详情与附件清单。
func (c *Client) GetMailMessage(userToken, messageID string) (*MailMessage, error) {
	u := fmt.Sprintf("%s/mail/v1/user_mailboxes/me/messages/%s", baseURL, messageID)
	var out struct {
		apiResp
		Data struct {
			Message struct {
				MessageID    string `json:"message_id"` // 字段名以官方 SDK 为准：message_id，不是 id
				Subject      string `json:"subject"`
				SMTPID       string        `json:"smtp_message_id"`
				HeadFrom     MailAddress   `json:"head_from"` // 发件人是对象 {mail_address, name}，不是字符串
				To           []MailAddress `json:"to"`
				Cc           []MailAddress `json:"cc"`
				Bcc          []MailAddress `json:"bcc"`
				InternalDate string        `json:"internal_date"`
				BodyHTML     string        `json:"body_html"`       // base64url 编码
				BodyPlain    string        `json:"body_plain_text"` // base64url 编码
				Attachments  []struct {
					ID             string `json:"id"`
					Filename       string `json:"filename"`
					Size           int64  `json:"size"`
					AttachmentType int    `json:"attachment_type"`
					IsInline       bool   `json:"is_inline"`
					CID            string `json:"cid"`
				} `json:"attachments"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		return nil, err
	}
	m := out.Data.Message
	ts, _ := strconv.ParseInt(m.InternalDate, 10, 64)
	bodyHTML, err := decodeMailBody(m.BodyHTML)
	if err != nil {
		return nil, fmt.Errorf("解码邮件 %s 的 HTML 正文: %w", messageID, err)
	}
	bodyPlain, err := decodeMailBody(m.BodyPlain)
	if err != nil {
		return nil, fmt.Errorf("解码邮件 %s 的纯文本正文: %w", messageID, err)
	}
	res := &MailMessage{
		MessageID:   m.MessageID,
		SMTPID:      m.SMTPID,
		Subject:     m.Subject,
		HeadFrom:    m.HeadFrom.Address,
		FromName:    m.HeadFrom.Name,
		To:          m.To,
		Cc:          m.Cc,
		Bcc:         m.Bcc,
		CreatedTime: ts,
		BodyHTML:    bodyHTML,
		BodyPlain:   bodyPlain,
	}
	for _, a := range m.Attachments {
		res.Attachments = append(res.Attachments, MailAttachment{
			ID:       a.ID,
			Name:     a.Filename,
			Size:     a.Size,
			IsInline: a.IsInline,
			CID:      a.CID,
		})
	}
	return res, nil
}

// decodeMailBody 解码 base64url 正文（有无末尾 = 补位都能解）。
func decodeMailBody(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DownloadMailAttachment 从附件专属下载链接拉取文件流。
func (c *Client) DownloadMailAttachment(userToken, messageID, attachmentID string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("attachment_ids", attachmentID)
	u := fmt.Sprintf("%s/mail/v1/user_mailboxes/me/messages/%s/attachments/download_url?%s",
		baseURL, messageID, q.Encode())

	var out struct {
		apiResp
		Data struct {
			DownloadURLs []struct {
				AttachmentID string `json:"attachment_id"`
				DownloadURL  string `json:"download_url"`
			} `json:"download_urls"`
			FailedIDs     []string `json:"failed_ids"`
			FailedReasons []struct {
				AttachmentID string `json:"attachment_id"`
				Reason       string `json:"reason"`
			} `json:"failed_reasons"` // 官方 SDK 定义为数组，不是映射
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		return nil, fmt.Errorf("获取附件下载链接: %w", err)
	}

	for _, failedID := range out.Data.FailedIDs {
		if failedID == attachmentID {
			reason := ""
			for _, fr := range out.Data.FailedReasons {
				if fr.AttachmentID == attachmentID {
					reason = fr.Reason
					break
				}
			}
			return nil, fmt.Errorf("附件 %s 获取下载链接失败: %s", attachmentID, reason)
		}
	}

	dlURL := ""
	for _, item := range out.Data.DownloadURLs {
		if item.AttachmentID == attachmentID {
			dlURL = item.DownloadURL
			break
		}
	}
	if dlURL == "" {
		return nil, fmt.Errorf("未获取到附件 %s 的下载链接", attachmentID)
	}

	req, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("下载邮件附件失败 http=%d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return resp.Body, nil
}
