// Package wecomcontact 企微「客户联系」最小客户端 + 外部联系人（客户）名字解析。
// 会话存档里外部联系人只有 ID（wm=微信客户 / wo=企微客户），本包用客户联系接口把 ID 换成名字，
// 供 NAS sink 决定会话正文里的发送人显示名和单聊文件名。
package wecomcontact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// defaultBaseURL 企微 API 根地址。
const defaultBaseURL = "https://qyapi.weixin.qq.com/cgi-bin"

// ErrNotFound 对应 84061：查不到这个外部联系人（正常结果，可缓存）。
var ErrNotFound = errors.New("外部联系人不存在或不在可见范围内")

// 需要重新取 token 的错误码：token 无效 / 过期 / corpid 与 token 不匹配。
const (
	errcodeTokenInvalid   = 40014
	errcodeTokenExpired   = 42001
	errcodeTokenWrongCorp = 40001
)

// errcodeNotVisible 84061：不是可见范围内员工的外部联系人（对方已删除等）。
const errcodeNotVisible = 84061

// tokenExpiryMargin token 提前多久视为过期（秒），避免卡在临界点。
const tokenExpiryMargin = 300

// Contact 外部联系人信息。
type Contact struct {
	Name     string
	CorpName string // 仅企微客户（type=2）有
	Type     int    // 1=微信用户 2=企微用户
}

// Client 客户联系接口客户端（access_token 进程内缓存）。
type Client struct {
	baseURL string
	corpID  string
	secret  string
	http    *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// New 创建客户端（secret 为客户联系自建应用的 Secret）。
func New(corpID, secret string) *Client {
	return &Client{
		baseURL: defaultBaseURL,
		corpID:  corpID,
		secret:  secret,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// SetBaseURL 覆盖 API 根地址（测试用）。
func (c *Client) SetBaseURL(u string) {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	if u != "" {
		c.baseURL = u
	}
}

// tokenResp gettoken 响应。
type tokenResp struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// externalContactResp externalcontact/get 响应。
type externalContactResp struct {
	ErrCode         int    `json:"errcode"`
	ErrMsg          string `json:"errmsg"`
	ExternalContact struct {
		ExternalUserID string `json:"external_userid"`
		Name           string `json:"name"`
		Type           int    `json:"type"`
		CorpName       string `json:"corp_name"`
		CorpFullName   string `json:"corp_full_name"`
		Position       string `json:"position"`
	} `json:"external_contact"`
}

// getToken 取缓存 token，过期或为空时重新获取。
func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshTokenLocked(ctx)
}

// invalidateToken 清缓存（token 失效时调用）。
func (c *Client) invalidateToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.tokenExp = time.Time{}
}

// refreshTokenLocked 返回可用 token（调用方持有 mu）。
// 缓存命中且未过期时直接返回；否则调 gettoken 重新获取。
func (c *Client) refreshTokenLocked(ctx context.Context) (string, error) {
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	u := c.baseURL + "/gettoken?corpid=" + url.QueryEscape(c.corpID) + "&corpsecret=" + url.QueryEscape(c.secret)
	var tr tokenResp
	if err := c.getJSON(ctx, u, &tr); err != nil {
		return "", fmt.Errorf("gettoken 请求失败: %w", err)
	}
	if tr.ErrCode != 0 {
		return "", fmt.Errorf("gettoken errcode=%d: %s", tr.ErrCode, tr.ErrMsg)
	}
	if tr.AccessToken == "" {
		return "", errors.New("gettoken 未返回 access_token")
	}
	c.token = tr.AccessToken
	ttl := tr.ExpiresIn - tokenExpiryMargin
	if ttl <= 0 {
		ttl = 60
	}
	c.tokenExp = time.Now().Add(time.Duration(ttl) * time.Second)
	return c.token, nil
}

// Get 查询外部联系人名字。
// 查不到（84061）返回 ErrNotFound；token 失效（40014/42001/40001）清缓存后重试一次。
func (c *Client) Get(ctx context.Context, externalUserID string) (Contact, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return Contact{}, err
		}
		contact, code, msg, err := c.getOnce(ctx, token, externalUserID)
		if err != nil {
			return Contact{}, err
		}
		switch code {
		case 0:
			return contact, nil
		case errcodeNotVisible:
			return Contact{}, ErrNotFound
		case errcodeTokenInvalid, errcodeTokenExpired, errcodeTokenWrongCorp:
			c.invalidateToken()
			if attempt >= 1 {
				return Contact{}, fmt.Errorf("externalcontact/get errcode=%d: %s（access_token 失效，重取后仍失败）", code, msg)
			}
		default:
			// 错误信息只放 errcode/errmsg，不放 URL（URL 里有 access_token）
			return Contact{}, fmt.Errorf("externalcontact/get errcode=%d: %s", code, msg)
		}
	}
}

// getOnce 调一次 externalcontact/get。
func (c *Client) getOnce(ctx context.Context, token, externalUserID string) (Contact, int, string, error) {
	u := c.baseURL + "/externalcontact/get?access_token=" + url.QueryEscape(token) +
		"&external_userid=" + url.QueryEscape(externalUserID)
	var resp externalContactResp
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return Contact{}, 0, "", fmt.Errorf("externalcontact/get 请求失败: %w", err)
	}
	ec := resp.ExternalContact
	return Contact{Name: ec.Name, CorpName: ec.CorpName, Type: ec.Type}, resp.ErrCode, resp.ErrMsg, nil
}

// getJSON GET 并解析 JSON。错误只带状态码/原因，不含 URL（URL 里带 corpsecret / access_token）。
func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("构造请求失败")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error 的 Error() 带完整 URL，只留底层原因
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return errors.New("请求失败")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析响应失败: %v", err)
	}
	return nil
}
