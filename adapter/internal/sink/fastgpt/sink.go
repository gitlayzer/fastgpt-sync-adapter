// Package fastgpt Sink：把连接器拉到的【文件附件】直传 FastGPT 知识库。
// 只进附件，不进消息体 JSON（聊天原文归档走 NAS Sink，二选一）。
// 不扫描任何本地目录——字节由连接器经 read 回调直供。
package fastgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"fastgpt-sync-adapter/adapter/internal/model"
	"fastgpt-sync-adapter/adapter/internal/sink"
	"fastgpt-sync-adapter/adapter/internal/uploader"
)

type Config struct {
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"` // 知识库级 key 即可上传；如需按用户建 NAS 目录需账号级
	DatasetID string `yaml:"dataset_id"`
	// 上传解析参数（对应 FastGPT WebUI）
	ImageIndex     bool `yaml:"image_index"`
	AutoIndexes    bool `yaml:"auto_indexes"`
	CustomPdfParse bool `yaml:"custom_pdf_parse"`
	// StateFile 附件去重状态（sdkfileid → collectionID），防重启重复上传
	StateFile string `yaml:"state_file"`
}

type Sink struct {
	client    *uploader.Client
	statePath string
	mu        sync.Mutex
	uploaded  map[string]string // 附件 FileID（如企微 sdkfileid）→ collectionID
}

func New(cfg Config) *Sink {
	client := uploader.NewClient(cfg.BaseURL, cfg.APIKey, cfg.DatasetID)
	client.ImageIndex = cfg.ImageIndex
	client.AutoIndexes = cfg.AutoIndexes
	client.CustomPdfParse = cfg.CustomPdfParse
	s := &Sink{client: client, statePath: cfg.StateFile, uploaded: map[string]string{}}
	s.loadState()
	return s
}

func (s *Sink) Name() string { return "fastgpt" }

// WriteRecord 只处理附件：消息体 JSON 不进知识库。
func (s *Sink) WriteRecord(ctx context.Context, r model.Record, read sink.AttachmentReader) error {
	for i := range r.Attachments {
		a := &r.Attachments[i]
		if a.FileID == "" {
			continue
		}
		if _, dup := s.get(a.FileID); dup {
			continue // 已传过，跳过
		}

		rc, err := read(ctx, a)
		if err != nil {
			return fmt.Errorf("附件 %s 拉取失败: %w", a.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("附件 %s 读取失败: %w", a.Name, err)
		}

		// 上传（S3 侧文件名随机）→ 二次调用改回真实文件名
		collectionID, err := s.client.UploadBytes(a.Name, a.Name, data)
		if err != nil {
			return fmt.Errorf("附件 %s 上传失败: %w", a.Name, err)
		}
		if err := s.client.RenameCollection(collectionID, a.Name); err != nil {
			log.Printf("[fastgpt] 重命名失败 id=%s name=%s: %v", collectionID, a.Name, err)
		}
		s.set(a.FileID, collectionID)
		log.Printf("[fastgpt] 已入库 %s（%d 字节，来自 %s/%s）", a.Name, len(data), r.Source, r.ID)
	}
	return nil
}

// ===== 去重状态持久化 =====

func (s *Sink) get(fileID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.uploaded[fileID]
	return id, ok
}

func (s *Sink) set(fileID, collectionID string) {
	s.mu.Lock()
	s.uploaded[fileID] = collectionID
	data, _ := json.MarshalIndent(s.uploaded, "", "  ")
	s.mu.Unlock()
	if s.statePath != "" {
		if err := os.WriteFile(s.statePath, data, 0644); err != nil {
			log.Printf("[fastgpt] 状态写入失败 %s: %v", s.statePath, err)
		}
	}
}

func (s *Sink) loadState() {
	if s.statePath == "" {
		return
	}
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		return // 无状态文件=首次运行
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.Unmarshal(data, &s.uploaded); err != nil {
		log.Printf("[fastgpt] 状态文件解析失败（将重新去重）: %v", err)
	}
}
