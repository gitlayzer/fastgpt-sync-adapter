package uploader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client 封装 FastGPT 数据集上传 API。
type Client struct {
	BaseURL   string // 例: https://aanshixbkhrk.sealoshzh.site
	APIKey    string // fastgpt-xxx
	DatasetID string // 知识库 id
	HTTP      *http.Client

	// 解析参数（对应 FastGPT web 界面的上传选项）
	ImageIndex     bool // 图片索引（图片向量化）
	AutoIndexes    bool // 自动生成索引
	CustomPdfParse bool // 自定义 PDF 解析（需服务端配置解析服务）
	IndexPrefixTitle bool // 索引前缀标题
	// Tags 上传时打的标签（FastGPT 源码确认 create/localFile 支持；元素为字符串或 {"tag","value"} 对象）
	Tags []any
}

func NewClient(baseURL, apiKey, datasetID string) *Client {
	return &Client{
		BaseURL:   baseURL,
		APIKey:    apiKey,
		DatasetID: datasetID,
		HTTP:      &http.Client{Timeout: 120 * time.Second},
		// 默认开启图片索引，便于解析含图片的文档
		ImageIndex: true,
	}
}

// apiResp FastGPT 通用响应
type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Collection 知识库中的一个文件条目
type Collection struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ListCollections 列出知识库当前所有文件（用于增量判断）。
func (c *Client) ListCollections() ([]Collection, error) {
	var all []Collection
	page := 1
	for {
		body := map[string]any{
			"datasetId": c.DatasetID,
			"pageNum":   page,
			"pageSize":  100,
		}
		var data struct {
			Total int          `json:"total"`
			List  []Collection `json:"list"`
		}
		if err := c.post("/api/core/dataset/collection/listV2", body, &data); err != nil {
			return nil, err
		}
		all = append(all, data.List...)
		if len(all) >= data.Total || len(data.List) == 0 {
			break
		}
		page++
	}
	return all, nil
}

// UploadFile 上传单个文件到知识库，返回 collectionId。
func (c *Client) UploadFile(localPath, name string) (string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}
	return c.UploadBytes(name, filepath.Base(localPath), data)
}

// UploadBytes 直接把字节内容上传到知识库（不经本地文件），返回 collectionId。
// name 为知识库内显示名，filename 为上传表单的文件名（S3 落盘名，之后用 RenameCollection 修正）。
func (c *Client) UploadBytes(name, filename string, fileData []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// 文件字段：中文文件名必须走 RFC 5987（filename*=UTF-8''...），
	// 否则服务端 busboy/multer 会把 raw UTF-8 按 latin1 解码成乱码（实测：合同.docx → å å .docx）。
	// 注意：FastGPT localFile 接口用表单文件名作为知识库内的显示名（data.name 会被忽略）。
	fw, err := createFormFileUTF8(w, "file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(fileData); err != nil {
		return "", err
	}

	// data 字段（JSON 字符串），带解析参数
	data := map[string]any{
		"datasetId":        c.DatasetID,
		"parentId":         nil,
		"trainingType":     "chunk",
		"chunkSize":        512,
		"name":             name,
		"imageIndex":       c.ImageIndex,
		"autoIndexes":      c.AutoIndexes,
		"indexPrefixTitle": c.IndexPrefixTitle,
	}
	if c.CustomPdfParse {
		data["customPdfParse"] = true
	}
	// 标签：源码确认 create/localFile 支持（ApiCreateCollectionRequestBaseSchema.tags）。
	// 两种格式：纯字符串 ["合同"]；或键值对象 [{"tag":"类型","value":"合同"}]。
	if len(c.Tags) > 0 {
		data["tags"] = c.Tags
	}
	dataJSON, _ := json.Marshal(data)
	if err := w.WriteField("data", string(dataJSON)); err != nil {
		return "", err
	}
	w.Close()

	req, err := http.NewRequest("POST", c.BaseURL+"/api/core/dataset/collection/create/localFile", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var ar struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			CollectionID string `json:"collectionId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return "", fmt.Errorf("parse response: %w, body: %s", err, truncate(string(respBody), 300))
	}
	if ar.Code != 200 {
		return "", fmt.Errorf("upload failed code=%d msg=%s", ar.Code, ar.Message)
	}
	return ar.Data.CollectionID, nil
}

// RenameCollection 重命名知识库中的文件（上传到 S3 后文件名随机化，需二次调用修正）。
func (c *Client) RenameCollection(collectionID, newName string) error {
	body := map[string]any{
		"id":   collectionID,
		"name": newName,
	}
	return c.post("/api/core/dataset/collection/update", body, nil)
}

// UpdateTags 事后修改 collection 的标签（update 接口源码确认支持 tags 字段）。
func (c *Client) UpdateTags(collectionID string, tags []any) error {
	body := map[string]any{
		"id":   collectionID,
		"tags": tags,
	}
	return c.post("/api/core/dataset/collection/update", body, nil)
}

// DeleteCollection 删除知识库中的文件（用于处理本地已删除的文件）。
// FastGPT delete 接口 body 为 {collectionIds: [...]}。
func (c *Client) DeleteCollection(collectionID string) error {
	body := map[string]any{"collectionIds": []string{collectionID}}
	return c.post("/api/core/dataset/collection/delete", body, nil)
}

// post 发送 JSON POST 请求。
func (c *Client) post(path string, payload any, out any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	if len(respBody) > 0 && respBody[0] == '<' {
		return fmt.Errorf("got HTML instead of JSON (endpoint %s may not exist), ct=%s", path, ct)
	}

	var ar apiResp
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return fmt.Errorf("parse json: %w, body: %s", err, truncate(string(respBody), 200))
	}
	if ar.Code != 200 {
		return fmt.Errorf("api code=%d msg=%s", ar.Code, ar.Message)
	}
	if out != nil && len(ar.Data) > 0 {
		if err := json.Unmarshal(ar.Data, out); err != nil {
			return fmt.Errorf("parse data: %w", err)
		}
	}
	return nil
}

// createFormFileUTF8 手工构造 multipart 文件头：
// filename 给 ASCII 兜底（非 ASCII 替换为 _），filename* 按 RFC 5987 携带 UTF-8 原名。
// Go 标准库 CreateFormFile 直接写 raw UTF-8 字节，部分服务端解析器会按 latin1 误读。
func createFormFileUTF8(w *multipart.Writer, fieldname, filename string) (io.Writer, error) {
	ascii := strings.Map(func(r rune) rune {
		if r < 128 && r != '"' && r != '\\' {
			return r
		}
		return '_'
	}, filename)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"; filename="%s"; filename*=UTF-8''%s`,
			fieldname, ascii, url.PathEscape(filename)))
	h.Set("Content-Type", "application/octet-stream")
	return w.CreatePart(h)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
