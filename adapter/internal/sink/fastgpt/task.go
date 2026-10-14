// Package fastgpt Sink（方案 A：主动上传）。
// FastGPT 上传是「扫描目录→增量上传」的旁路工作，不消费 Record，
// 因此实现为引擎的 Task（周期任务）而非 Sink。
package fastgpt

import (
	"context"
	"time"

	"fastgpt-sync-adapter/adapter/internal/syncer"
	"fastgpt-sync-adapter/adapter/internal/uploader"
)

type Config struct {
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"` // fastgpt-xxx（知识库级即可上传；账号级可调用户列表）
	DatasetID string `yaml:"dataset_id"`
	// 上传解析参数（对应 FastGPT WebUI）
	ImageIndex     bool `yaml:"image_index"`
	AutoIndexes    bool `yaml:"auto_indexes"`
	CustomPdfParse bool `yaml:"custom_pdf_parse"`

	// WatchPath 监控的本地目录（NAS 挂载点或同步落盘目录的本地镜像）
	WatchPath string `yaml:"watch_path"`
	// SyncDelete 本地删除后是否同步删除 FastGPT 对应文件（默认 false 保留远端副本）
	SyncDelete bool `yaml:"sync_delete"`
	// StateFile 上传增量状态文件（记录 文件路径+sha1 → collectionID）
	StateFile string `yaml:"state_file"`

	Interval time.Duration `yaml:"-"` // 由 config 解析后填入
}

// UploadTask 周期扫描 WatchPath，增量上传至 FastGPT 知识库。
type UploadTask struct {
	syncer   *syncer.Syncer
	interval time.Duration
}

func NewUploadTask(cfg Config) *UploadTask {
	client := uploader.NewClient(cfg.BaseURL, cfg.APIKey, cfg.DatasetID)
	client.ImageIndex = cfg.ImageIndex
	client.AutoIndexes = cfg.AutoIndexes
	client.CustomPdfParse = cfg.CustomPdfParse
	sync := syncer.New(cfg.WatchPath, client, cfg.StateFile)
	sync.SyncDelete = cfg.SyncDelete
	return &UploadTask{syncer: sync, interval: cfg.Interval}
}

func (t *UploadTask) Name() string             { return "fastgpt-upload" }
func (t *UploadTask) Interval() time.Duration  { return t.interval }
func (t *UploadTask) Run(ctx context.Context) error { return t.syncer.Run() }
