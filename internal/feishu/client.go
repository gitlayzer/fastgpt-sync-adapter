// Package feishu 飞书开放平台最小客户端：tenant_access_token、OAuth code 换 token、token 刷新。
// 只覆盖授权链路所需接口；云文档/多维表格的读取在 connector/feishu 里按需扩展。
package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	return &Client{AppID: appID, AppSecret: appSecret, HTTP: &http.Client{Timeout: 180 * time.Second}}
}

// apiResp 飞书通用响应。
type apiResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// apiAttempts 飞书 API 最大尝试次数。
const apiAttempts = 3

// retryableCode 判断飞书错误码是否值得重试。
// 只挑临时的：限流(99991400)、系统繁忙、内部错误。
// 权限类(99991679/99991663)、参数类(131002) 重试无益，还会拖慢失败。
func retryableCode(code int) bool {
	switch code {
	case 99991400, // 请求触发限流
		99991401, // 请求触发限流（部分接口）
		131001,   // rpc fail
		131007,   // internal err
		20050:    // server error
		return true
	}
	return false
}

func (c *Client) post(path string, payload any, out any) error {
	b, _ := json.Marshal(payload)
	var lastErr error
	for attempt := 0; attempt < apiAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
			log.Printf("[feishu] POST %s 重试 %d/%d（上次: %v）", path, attempt+1, apiAttempts, lastErr)
		}
		resp, err := c.HTTP.Post(baseURL+path, "application/json; charset=utf-8", bytes.NewReader(b))
		if err != nil {
			lastErr = err // 网络层，重试
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var ar apiResp
		if err := json.Unmarshal(body, &ar); err != nil {
			lastErr = fmt.Errorf("解析响应失败: %w, body: %s", err, truncate(string(body), 300))
			continue
		}
		if ar.Code != 0 {
			lastErr = fmt.Errorf("飞书错误 code=%d msg=%s", ar.Code, ar.Msg)
			if retryableCode(ar.Code) {
				continue
			}
			return lastErr
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("解析响应数据失败: %w", err)
			}
		}
		return nil
	}
	return lastErr
}

// postJSON 带显式 bearer token 的 POST（bitable 等需要指定身份的场景）。
// 与 post 的区别：不带 client_secret，token 由调用方给。
func (c *Client) postJSON(fullURL string, payload any, token string, out any) error {
	b, _ := json.Marshal(payload)
	var lastErr error
	for attempt := 0; attempt < apiAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
			log.Printf("[feishu] POST %s 重试 %d/%d（上次: %v）", shortURL(fullURL), attempt+1, apiAttempts, lastErr)
		}
		req, err := http.NewRequest("POST", fullURL, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var ar apiResp
		if err := json.Unmarshal(body, &ar); err != nil {
			lastErr = fmt.Errorf("解析响应失败: %w, body: %s", err, truncate(string(body), 300))
			continue
		}
		if ar.Code != 0 {
			lastErr = fmt.Errorf("飞书错误 code=%d msg=%s", ar.Code, ar.Msg)
			if retryableCode(ar.Code) {
				continue
			}
			return lastErr
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("解析响应数据失败: %w", err)
			}
		}
		return nil
	}
	return lastErr
}

// getWithToken 带显式 bearer token 的 GET（完整 URL）。
func (c *Client) getWithToken(fullURL, token string, out any) error {
	var lastErr error
	for attempt := 0; attempt < apiAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
			log.Printf("[feishu] GET %s 重试 %d/%d（上次: %v）", shortURL(fullURL), attempt+1, apiAttempts, lastErr)
		}
		req, err := http.NewRequest("GET", fullURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var ar apiResp
		if err := json.Unmarshal(body, &ar); err != nil {
			lastErr = fmt.Errorf("解析响应失败: %w, body: %s", err, truncate(string(body), 300))
			continue
		}
		if ar.Code != 0 {
			lastErr = fmt.Errorf("飞书错误 code=%d msg=%s", ar.Code, ar.Msg)
			if retryableCode(ar.Code) {
				continue
			}
			return lastErr
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("解析响应数据失败: %w", err)
			}
		}
		return nil
	}
	return lastErr
}

// shortURL 日志里只打 path 部分，避免刷屏。
func shortURL(u string) string {
	const marker = "/open-apis"
	if i := indexOf(u, marker); i >= 0 {
		return u[i+len(marker):]
	}
	return u
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
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

// RequiredScopes 授权链接要请求的权限集合。
//
// 重要：飞书只会授予这里显式列出的 scope。后台开通了但链接没请求，token 里就不会有，
// 排障时极易误判成「后台没开」。新增权限必须同步改这里。
//
// offline_access              只有它能让飞书返回 refresh_token（否则 2 小时后无法续期）
// drive:drive                 云空间元数据、下载文件
// wiki:wiki:readonly          知识库节点（「我的文档库」归属 wiki 体系）
// docx:document:readonly      新版文档正文（docx/v1/documents/{id}/raw_content）
// space:document:retrieve     用户云空间文件夹下的云文档清单
//                             —— drive/v1/files 在个人云空间上实测还额外要求它，
//                             仅有 drive:drive 时接口返回 code=0 但列表为空
// drive:export:readonly       导出云文档为源文件（docx/xlsx）
//                             —— 缺它时 export_tasks 报 99991679；
//                             drive:drive 不覆盖导出，必须单独申请
// contact:user.employee_id:readonly  user_id_type=user_id 时取 owner_id 需要
// search:docs:read            云文档搜索
// auth:user.id:read           识别授权人身份
// mail:user_mailbox.message:readonly  读取邮箱邮件列表
// mail:user_mailbox.message.body:read 获取邮件正文及附件下载链接
// minutes:minutes             妙记文件基础权限
// minutes:minutes.search:read 妙记全量检索列表
// minutes:minutes.transcript:export   导出妙记转写文字/逐字稿
// vc:note:read                获取视频会议智能纪要
const RequiredScopes = "offline_access drive:drive drive:export:readonly wiki:wiki:readonly " +
	"docx:document:readonly space:document:retrieve contact:user.employee_id:readonly " +
	"search:docs:read auth:user.id:read " +
	"mail:user_mailbox.message:readonly mail:user_mailbox.message.body:read " +
	"minutes:minutes minutes:minutes.search:read minutes:minutes.transcript:export " +
	"vc:note:read"

// AuthorizeURL 拼 OAuth 授权链接（用户浏览器打开并确认）。
// scope 必须显式带上 offline_access，否则飞书不返回 refresh_token。
func AuthorizeURL(appID, redirectURI, state string) string {
	q := "client_id=" + appID +
		"&response_type=code" +
		"&redirect_uri=" + urlQueryEscape(redirectURI) +
		"&scope=" + urlQueryEscape(RequiredScopes) +
		"&state=" + urlQueryEscape(state)
	return "https://accounts.feishu.cn/open-apis/authen/v1/authorize?" + q
}

// UserTokenResp code/refresh 换来的用户 token 响应。
//
// 注意字段名：飞书返回的是 refresh_token_expires_in（带 token_），
// 不是 refresh_expires_in。写错会导致解析出 0，进而把 refresh_token 的
// 过期时间算成「现在」，表现为授权完立刻提示「refresh_token 已过期」。
type UserTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // access_token 秒数（约 7200）

	// RefreshExpiresIn 对应 refresh_token_expires_in，仅在授予 offline_access 时返回
	RefreshExpiresIn int64 `json:"refresh_token_expires_in"`

	TokenType string `json:"token_type"`
	Scope     string `json:"scope"`
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
