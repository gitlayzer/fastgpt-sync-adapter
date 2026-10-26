package feishu

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MailMessage 邮件摘要。
type MailMessage struct {
	MessageID      string `json:"message_id"`
	Subject        string `json:"subject"`
	HasAttachments bool   `json:"has_attachments"`
	CreatedTime    int64  `json:"created_time"` // 毫秒
}

// MailAttachment 邮件附件元数据。
type MailAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ListMailboxMessages 列出用户收件箱邮件（用户未开通飞书邮箱时自动平滑跳过，不报错）。
func (c *Client) ListMailboxMessages(userToken string, pageSize int) ([]MailMessage, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	u := fmt.Sprintf("%s/mail/v1/user_mailboxes/me/messages?page_size=%d", baseURL, pageSize)
	var out struct {
		apiResp
		Data struct {
			HasMore bool          `json:"has_more"`
			Items   []MailMessage `json:"items"`
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		errStr := err.Error()
		// 15180001 / 4013：用户未开通企业邮箱，静默返回空
		if strings.Contains(errStr, "15180001") || strings.Contains(errStr, "4013") {
			return nil, nil
		}
		return nil, err
	}
	return out.Data.Items, nil
}

// ListMailAttachments 获取某封邮件的所有附件清单。
func (c *Client) ListMailAttachments(userToken, messageID string) ([]MailAttachment, error) {
	u := fmt.Sprintf("%s/mail/v1/user_mailboxes/me/messages/%s", baseURL, messageID)
	var out struct {
		apiResp
		Data struct {
			Attachments []MailAttachment `json:"attachments"`
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		return nil, err
	}
	return out.Data.Attachments, nil
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
			DownloadURLs map[string]string `json:"download_urls"`
		} `json:"data"`
	}
	if err := c.getWithToken(u, userToken, &out); err != nil {
		return nil, fmt.Errorf("获取附件下载链接: %w", err)
	}

	dlURL, ok := out.Data.DownloadURLs[attachmentID]
	if !ok || dlURL == "" {
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
