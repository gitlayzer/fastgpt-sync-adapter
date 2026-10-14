package synology

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Login 调用 webapi/auth.cgi 登录拿 sid。
func (c *Client) Login() error {
	u := fmt.Sprintf("%s/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=%s&passwd=%s&session=FileStation&format=sid",
		c.BaseURL, url.QueryEscape(c.Username), url.QueryEscape(c.Password))
	resp, err := c.HTTP.Get(u)
	if err != nil {
		return fmt.Errorf("nas 登录请求失败: %w", err)
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
		return fmt.Errorf("nas 登录响应解析失败: %w (body=%s)", err, truncate(string(body), 200))
	}
	if !r.Success {
		return fmt.Errorf("nas 登录失败, error code=%d (401=账号密码错误, 403=被锁定, 404=账号不存在)", r.Error.Code)
	}
	c.sid = r.Data.Sid
	return nil
}

// ensureLogin 确保已登录。
func (c *Client) ensureLogin() error {
	if c.sid == "" {
		return c.Login()
	}
	return nil
}

// apiCall 通用 File Station API 调用。
func (c *Client) apiCall(api, method string, params url.Values) (json.RawMessage, error) {
	if err := c.ensureLogin(); err != nil {
		return nil, err
	}
	params.Set("api", api)
	params.Set("method", method)
	params.Set("_sid", c.sid)
	u := fmt.Sprintf("%s/webapi/entry.cgi?%s", c.BaseURL, params.Encode())

	resp, err := c.HTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var r struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("nas api %s/%s 响应解析失败: %w", api, method, err)
	}
	if !r.Success {
		// 119 = sid 失效，重新登录重试一次
		if r.Error.Code == 119 {
			c.sid = ""
			if err := c.ensureLogin(); err != nil {
				return nil, err
			}
			return c.apiCall(api, method, params)
		}
		return nil, fmt.Errorf("nas api %s/%s 失败, error code=%d", api, method, r.Error.Code)
	}
	return r.Data, nil
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
		if err := c.CreateFolder(parent, p); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", parent+p, err)
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
func (c *Client) UploadBytes(filename, destDir string, data []byte) error {
	if err := c.ensureLogin(); err != nil {
		return err
	}
	// 确保目标目录存在
	if err := c.MkdirAll(destDir); err != nil {
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
