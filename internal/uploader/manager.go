package uploader

import (
	"fmt"
	"net/http"
	"time"
)

// Manager 知识库管理（按名查找/创建），与按库上传的 Client 配合。
type Manager struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	// 建库模型（缺省用实例默认）
	VectorModel string // text-embedding-v4
	AgentModel  string // qwen3.5-plus
	VlmModel    string // qwen3.8-flash
}

func NewManager(baseURL, apiKey string) *Manager {
	return &Manager{
		BaseURL: baseURL, APIKey: apiKey,
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		VectorModel: "text-embedding-v4",
		AgentModel:  "qwen3.5-plus",
		VlmModel:    "qwen3.8-flash",
	}
}

// Dataset 知识库条目。
type Dataset struct {
	ID         string `json:"_id"`
	Name       string `json:"name"`
	CreateTime string `json:"createTime"`
}

// postJSON 复用 Client 的 post（同包）。
func (m *Manager) post(path string, payload any, out any) error {
	c := &Client{BaseURL: m.BaseURL, APIKey: m.APIKey, HTTP: m.HTTP}
	return c.post(path, payload, out)
}

// ListDatasets 列出知识库（可按名搜索）。
func (m *Manager) ListDatasets(searchKey string) ([]Dataset, error) {
	body := map[string]any{"parentId": nil, "searchKey": searchKey}
	var data []Dataset
	if err := m.post("/api/core/dataset/list", body, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// FindDatasetByName 按名精确匹配；名称不唯一时取最新创建者。找不到返回空串。
func (m *Manager) FindDatasetByName(name string) (string, error) {
	list, err := m.ListDatasets(name)
	if err != nil {
		return "", err
	}
	var best *Dataset
	for i := range list {
		if list[i].Name != name {
			continue
		}
		if best == nil || list[i].CreateTime > best.CreateTime {
			best = &list[i]
		}
	}
	if best == nil {
		return "", nil
	}
	return best.ID, nil
}

// CreateDataset 创建知识库，返回 id。
func (m *Manager) CreateDataset(name, intro string) (string, error) {
	body := map[string]any{
		"parentId":    nil,
		"type":        "dataset",
		"name":        name,
		"intro":       intro,
		"vectorModel": m.VectorModel,
		"agentModel":  m.AgentModel,
	}
	if m.VlmModel != "" {
		body["vlmModel"] = m.VlmModel
	}
	var id string
	if err := m.post("/api/core/dataset/create", body, &id); err != nil {
		return "", fmt.Errorf("建库失败: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("建库返回空 id")
	}
	return id, nil
}

// EnsureDatasetByName 按名找知识库，没有则创建。返回 (id, created, error)。
func (m *Manager) EnsureDatasetByName(name string) (string, bool, error) {
	id, err := m.FindDatasetByName(name)
	if err != nil {
		return "", false, err
	}
	if id != "" {
		return id, false, nil
	}
	id, err = m.CreateDataset(name, "adapter 按人自动创建")
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// ClientFor 得到一个绑定指定知识库的上传客户端。
func (m *Manager) ClientFor(datasetID string) *Client {
	return NewClient(m.BaseURL, m.APIKey, datasetID)
}
