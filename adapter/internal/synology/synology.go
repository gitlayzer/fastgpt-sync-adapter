package synology

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// Client 群晖 NAS WebAPI 客户端（File Station）。
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client

	sid string // 登录会话
}

func New(baseURL, username, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: username,
		Password: password,
		// 群晖在内网用自签名证书（IP 无 SAN），跳过证书校验；仅建议内网使用
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// loginAttempts 登录尝试次数。群晖有「自动封锁」机制：短时间内多次失败会把来源 IP
// 拉黑一段时间，表现为 TCP 能连上、TLS 刚起手就被对端断开（EOF）。这类故障是
// 间歇性的，单次失败不代表配置有问题，重试通常能恢复。
const loginAttempts = 4

// uploadAttempts 上传重试次数。上传失败意味着数据永久丢失
// （企微附件在服务端只保留约 3 天），所以给得比其他环节宽。
const uploadAttempts = 4

// Login 调用 webapi/auth.cgi 登录拿 sid。
// 对网络层错误（连不上、被断开）做指数退避重试；对明确的 API 层错误
// （账号密码错、被锁定）立即返回，重试没有意义。
func (c *Client) Login() error {
	var lastErr error
	for i := 0; i < loginAttempts; i++ {
		if i > 0 {
			// 1s → 2s → 4s，给 NAS 的封锁窗口一点时间
			wait := time.Duration(1<<uint(i-1)) * time.Second
			log.Printf("[nas] 登录重试 %d/%d，等待 %s（上次错误: %v）",
				i+1, loginAttempts, wait, lastErr)
			time.Sleep(wait)
		}
		retryable, err := c.loginOnce()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("nas 登录失败（已重试 %d 次）: %w", loginAttempts, lastErr)
}

// loginOnce 单次登录。retryable 表示失败时是否值得重试；成功时恒为 false。
func (c *Client) loginOnce() (bool, error) {
	u := fmt.Sprintf("%s/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=%s&passwd=%s&session=FileStation&format=sid",
		c.BaseURL, url.QueryEscape(c.Username), url.QueryEscape(c.Password))
	resp, err := c.HTTP.Get(u)
	if err != nil {
		// 网络层：EOF / 超时 / 连接重置 —— 多为间歇性，值得重试
		return true, fmt.Errorf("nas 登录请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var r struct {
		Success bool `json:"success"`
		Data    struct {
			Sid string `json:"sid"`
		} `json:"data"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		// 响应体不是 JSON：可能是被拦截页或半截响应，值得重试一次
		return true, fmt.Errorf("nas 登录响应解析失败: %w (body=%s)", err, truncate(string(body), 200))
	}
	if !r.Success {
		// 明确的业务错误：账号密码错(400/401)、被锁定(403/407) 等，重试无益
		return false, fmt.Errorf("nas 登录失败, error code=%d (400/401=账号密码错误, 403=无权限, 407=IP 被封锁, 404=账号不存在)", r.Error.Code)
	}
	c.sid = r.Data.Sid
	return false, nil
}

// ensureLogin 确保已登录。
func (c *Client) ensureLogin() error {
	if c.sid == "" {
		return c.Login()
	}
	return nil
}

// apiAttempts 单个 NAS API 调用的最大尝试次数（网络层错误）。
// NAS 走 VPN，连接偶发被重置/EOF 是常态（实测登录阶段就会遇到）。
const apiAttempts = 4

// apiCall 通用 File Station API 调用。
// 网络层错误重试；sid 失效（119）重新登录后重试，但有次数上限，
// 避免 sid 反复失效时无限递归。
func (c *Client) apiCall(api, method string, params url.Values) (json.RawMessage, error) {
	var lastErr error
	relogin := 0
	for attempt := 0; attempt < apiAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		if err := c.ensureLogin(); err != nil {
			lastErr = err
			continue
		}
		params.Set("api", api)
		params.Set("method", method)
		params.Set("_sid", c.sid)
		u := fmt.Sprintf("%s/webapi/entry.cgi?%s", c.BaseURL, params.Encode())

		resp, err := c.HTTP.Get(u)
		if err != nil {
			lastErr = err // 网络层，重试
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var r struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
			Error   struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			lastErr = fmt.Errorf("nas api %s/%s 响应解析失败: %w", api, method, err)
			continue // 半截响应多为网络问题，重试
		}
		if r.Success {
			return r.Data, nil
		}
		// 119 = sid 失效：清掉后重新登录再试，最多两次
		if r.Error.Code == 119 && relogin < 2 {
			relogin++
			c.sid = ""
			lastErr = fmt.Errorf("nas api %s/%s sid 失效", api, method)
			continue
		}
		// 业务错误（如 408 已存在）重试无益，直接返回
		return nil, fmt.Errorf("nas api %s/%s 失败, error code=%d", api, method, r.Error.Code)
	}
	return nil, fmt.Errorf("nas api %s/%s 重试耗尽: %w", api, method, lastErr)
}

// FileEntry NAS 上的一个文件/目录项。
type FileEntry struct {
	Name  string
	Path  string // 完整路径
	IsDir bool
	Size  int64
	MTime time.Time
}

// List 列目录（SYNO.FileStation.List）。folderPath 须以共享文件夹开头（如 /FastGPT-test/企业微信）。
func (c *Client) List(folderPath string) ([]FileEntry, error) {
	params := url.Values{}
	params.Set("version", "2")
	params.Set("folder_path", folderPath)
	params.Set("additional", `["size","time"]`)
	data, err := c.apiCall("SYNO.FileStation.List", "list", params)
	if err != nil {
		return nil, err
	}
	var d struct {
		Files []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			IsDir bool   `json:"isdir"`
			Add   struct {
				Size int64 `json:"size"`
				Time struct {
					MTime int64 `json:"mtime"`
				} `json:"time"`
			} `json:"additional"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("list 响应解析失败: %w", err)
	}
	out := make([]FileEntry, 0, len(d.Files))
	for _, f := range d.Files {
		out = append(out, FileEntry{
			Name:  f.Name,
			Path:  f.Path,
			IsDir: f.IsDir,
			Size:  f.Add.Size,
			MTime: time.Unix(f.Add.Time.MTime, 0),
		})
	}
	return out, nil
}

// Download 下载文件字节流（SYNO.FileStation.Download）。调用方负责关闭。
func (c *Client) Download(filePath string) (io.ReadCloser, error) {
	if err := c.ensureLogin(); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("api", "SYNO.FileStation.Download")
	params.Set("version", "2")
	params.Set("method", "download")
	params.Set("path", filePath)
	params.Set("mode", "download")
	params.Set("_sid", c.sid)
	u := fmt.Sprintf("%s/webapi/entry.cgi?%s", c.BaseURL, params.Encode())

	resp, err := c.HTTP.Get(u)
	if err != nil {
		return nil, err
	}
	// 下载失败时 NAS 返回 JSON 错误而非文件流
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("下载失败 %s: %s", filePath, truncate(string(body), 200))
	}
	return resp.Body, nil
}

// CreateFolder 在 parentPath 下创建名为 name 的文件夹（已存在则忽略错误）。
func (c *Client) CreateFolder(parentPath, name string) error {
	params := url.Values{}
	params.Set("version", "2")
	params.Set("folder_path", parentPath)
	params.Set("name", name)
	_, err := c.apiCall("SYNO.FileStation.CreateFolder", "create", params)
	if err != nil {
		// 408 = 文件/文件夹已存在，视为成功
		if strings.Contains(err.Error(), "code=408") {
			return nil
		}
		return err
	}
	return nil
}

// MkdirAll 递归创建路径（逐层创建，已存在则跳过）。
func (c *Client) MkdirAll(fullPath string) error {
	fullPath = path.Clean(fullPath)
	parts := strings.Split(strings.Trim(fullPath, "/"), "/")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		parent := "/" + cur
		if cur == "" {
			parent = "/"
		}
		target := "/" + p
		if cur != "" {
			target = "/" + cur + "/" + p
		}
		if err := c.CreateFolder(parent, p); err != nil {
			// 共享文件夹根等场景 CreateFolder 会报 1100（无权限创建），
			// 但目录可能已存在——用 List 探测，能列出来就当存在继续
			if _, lerr := c.List(target); lerr != nil {
				return fmt.Errorf("创建目录 %s 失败: %w", target, err)
			}
		}
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
	}
	return nil
}

// UploadFile 上传本地文件到 NAS 的 destDir 目录。
func (c *Client) UploadFile(localPath, destDir string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	return c.UploadBytes(path.Base(localPath), destDir, data)
}

// UploadBytes 直接把字节内容以 filename 命名上传到 NAS 的 destDir 目录。
// 上传是数据落地的最后一环，失败即永久丢失（企微附件只留 3 天），因此做多层重试。
func (c *Client) UploadBytes(filename, destDir string, data []byte) error {
	if err := c.MkdirAll(destDir); err != nil {
		return fmt.Errorf("准备目录 %s: %w", destDir, err)
	}
	var lastErr error
	for attempt := 0; attempt < uploadAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
			log.Printf("[nas] 上传重试 %d/%d: %s（上次: %v）",
				attempt+1, uploadAttempts, filename, lastErr)
		}
		err := c.uploadOnce(filename, destDir, data)
		if err == nil {
			return nil
		}
		lastErr = err
		// sid 失效先重登，下一轮会拿到新 sid
		if strings.Contains(err.Error(), "code=119") {
			c.sid = ""
		}
	}
	return fmt.Errorf("上传 %s 失败（已重试 %d 次）: %w", filename, uploadAttempts, lastErr)
}

// uploadOnce 单次上传（不含目录创建与重试）。
func (c *Client) uploadOnce(filename, destDir string, data []byte) error {
	if err := c.ensureLogin(); err != nil {
		return err
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// File Station upload 参数
	_ = w.WriteField("api", "SYNO.FileStation.Upload")
	_ = w.WriteField("version", "2")
	_ = w.WriteField("method", "upload")
	_ = w.WriteField("path", destDir)
	_ = w.WriteField("create_parents", "true")
	_ = w.WriteField("overwrite", "true")
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	w.Close()

	u := fmt.Sprintf("%s/webapi/entry.cgi?_sid=%s", c.BaseURL, url.QueryEscape(c.sid))
	req, err := http.NewRequest("POST", u, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var r struct {
		Success bool `json:"success"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("上传响应解析失败: %w (body=%s)", err, truncate(string(body), 200))
	}
	if !r.Success {
		return fmt.Errorf("上传失败, error code=%d", r.Error.Code)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
