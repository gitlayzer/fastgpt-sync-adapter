// Package feishu 飞书开放平台最小客户端：tenant_access_token、OAuth code 换 token、token 刷新。
// 只覆盖授权链路所需接口；云文档/多维表格的读取在 connector/feishu 里按需扩展。
package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const baseURL = "https://open.feishu.cn/open-apis"

// Client 飞书应用客户端。
type Client struct {
	AppID     string
	AppSecret string
	HTTP      *http.Client
}

func New(appID, appSecret string) *Client {
	return &Client{AppID: appID, AppSecret: appSecret, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// apiResp 飞书通用响应。
type apiResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (c *Client) post(path string, payload any, out any) error {
	b, _ := json.Marshal(payload)
	resp, err := c.HTTP.Post(baseURL+path, "application/json; charset=utf-8", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ar apiResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return fmt.Errorf("解析响应失败: %w, body: %s", err, truncate(string(body), 300))
	}
	if ar.Code != 0 {
		return fmt.Errorf("飞书错误 code=%d msg=%s", ar.Code, ar.Msg)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("解析响应数据失败: %w", err)
		}
	}
	return nil
}

// TenantAccessToken 应用身份 token（tenant 模式用）。
func (c *Client) TenantAccessToken() (string, error) {
	var out struct {
		apiResp
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := c.post("/auth/v3/tenant_access_token/internal", map[string]string{
		"app_id": c.AppID, "app_secret": c.AppSecret,
	}, &out); err != nil {
		return "", err
	}
	return out.TenantAccessToken, nil
}

// AuthorizeURL 拼 OAuth 授权链接（用户浏览器打开并确认）。
func AuthorizeURL(appID, redirectURI, state string) string {
	return fmt.Sprintf("https://accounts.feishu.cn/open-apis/authen/v1/authorize?app_id=%s&redirect_uri=%s&state=%s",
		appID, urlQueryEscape(redirectURI), urlQueryEscape(state))
}

// UserTokenResp code/refresh 换来的用户 token 响应。
type UserTokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`         // access_token 秒数（约 7200）
	RefreshExpiresIn int64  `json:"refresh_expires_in"` // refresh_token 秒数（约 30 天）
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
}

// ExchangeCode 授权 code → user_access_token。
func (c *Client) ExchangeCode(code, redirectURI string) (*UserTokenResp, error) {
	var out struct {
		apiResp
		UserTokenResp
	}
	err := c.post("/authen/v2/oauth/token", map[string]string{
		"grant_type":   "authorization_code",
		"client_id":    c.AppID,
		"client_secret": c.AppSecret,
		"code":         code,
		"redirect_uri": redirectURI,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out.UserTokenResp, nil
}

// RefreshUserToken refresh_token → 新的 user_access_token（滚动续期）。
func (c *Client) RefreshUserToken(refreshToken string) (*UserTokenResp, error) {
	var out struct {
		apiResp
		UserTokenResp
	}
	err := c.post("/authen/v2/oauth/token", map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     c.AppID,
		"client_secret": c.AppSecret,
		"refresh_token": refreshToken,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out.UserTokenResp, nil
}

// UserInfo 用 user_access_token 拿用户信息（确认是谁授权的）。
func (c *Client) UserInfo(userAccessToken string) (openID, unionID, name string, err error) {
	req, _ := http.NewRequest("GET", baseURL+"/authen/v1/user_info", nil)
	req.Header.Set("Authorization", "Bearer "+userAccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		apiResp
		Data struct {
			OpenID  string `json:"open_id"`
			UnionID string `json:"union_id"`
			Name    string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", "", fmt.Errorf("解析用户信息失败: %w", err)
	}
	if out.Code != 0 {
		return "", "", "", fmt.Errorf("飞书错误 code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Data.OpenID, out.Data.UnionID, out.Data.Name, nil
}

func urlQueryEscape(s string) string {
	var b bytes.Buffer
	for _, c := range []byte(s) {
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
