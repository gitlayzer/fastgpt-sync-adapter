package feishu

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// MinuteItem 妙记搜索返回的条目。
type MinuteItem struct {
	MinuteToken string `json:"minute_token"`
	OwnerID     string `json:"owner_id"`
	CreateTime  int64  `json:"create_time"` // 毫秒时间戳
	Duration    int    `json:"duration"`    // 秒数
	Title       string `json:"title"`
	URL         string `json:"url"`
}

// SearchMinutes 列出调用者可见的妙记/智能纪要（支持自动翻页）。
func (c *Client) SearchMinutes(userToken string) ([]MinuteItem, error) {
	var all []MinuteItem
	pageToken := ""
	for {
		body := map[string]any{
			"query":     "",
			"page_size": 50,
		}
		if pageToken != "" {
			body["page_token"] = pageToken
		}
		var out struct {
			apiResp
			Data struct {
				HasMore   bool         `json:"has_more"`
				PageToken string       `json:"page_token"`
				Items     []MinuteItem `json:"items"`
			} `json:"data"`
		}
		if err := c.postJSON(baseURL+"/minutes/v1/minutes/search", body, userToken, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data.Items...)
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}
	return all, nil
}

// DownloadMinutesTranscript 下载妙记/智能纪要的逐字稿文字流。
func (c *Client) DownloadMinutesTranscript(userToken, minuteToken string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("need_speaker", "true")
	q.Set("need_timestamp", "true")
	u := fmt.Sprintf("%s/minutes/v1/minutes/%s/transcript?%s", baseURL, minuteToken, q.Encode())

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("下载妙记逐字稿失败 http=%d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return resp.Body, nil
}
