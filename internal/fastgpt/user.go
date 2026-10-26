package fastgpt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// User FastGPT 用户/团队成员。
type User struct {
	ID       string `json:"id"`       // tmbId / userId
	Username string `json:"username"` // 登录名
	Name     string `json:"name"`     // 昵称/姓名
}

// UserClient 获取 FastGPT 用户列表（用于在 NAS 按用户建目录）。
type UserClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewUserClient(baseURL, apiKey string) *UserClient {
	return &UserClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ListUsers 拉取 FastGPT 团队成员列表。
// 注意：团队成员接口需要账号级（session/token）权限，
// 知识库级 API key（fastgpt-xxx）通常无权访问，会返回 404/HTML。
// 此时应改用配置文件提供的用户清单（见 config 的 fallback）。
func (c *UserClient) ListUsers() ([]User, error) {
	endpoints := []struct {
		path string
		body map[string]any
	}{
		{"/api/support/user/team/member/list", map[string]any{}},
		{"/proApi/support/user/team/member/list", map[string]any{}},
	}
	var lastErr error
	for _, ep := range endpoints {
		users, err := c.tryList(ep.path, ep.body)
		if err == nil {
			return users, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("FastGPT 用户列表接口不可用（知识库 key 无团队权限）: %w", lastErr)
}

func (c *UserClient) tryList(path string, payload any) ([]User, error) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if len(body) > 0 && body[0] == '<' {
		return nil, fmt.Errorf("返回 HTML（接口不存在或无权限）")
	}
	var r struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	if r.Code != 200 {
		return nil, fmt.Errorf("code=%d msg=%s", r.Code, r.Message)
	}
	// data 可能是数组或 {list:[...]}
	var arr []User
	if err := json.Unmarshal(r.Data, &arr); err == nil {
		return arr, nil
	}
	var wrap struct {
		List []User `json:"list"`
	}
	if err := json.Unmarshal(r.Data, &wrap); err == nil {
		return wrap.List, nil
	}
	return nil, fmt.Errorf("无法识别的用户列表结构")
}
