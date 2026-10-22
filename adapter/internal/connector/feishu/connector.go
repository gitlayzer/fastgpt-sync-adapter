// 飞书云文档连接器：tenant（共享文件夹遍历）+ user（已授权用户云空间）双模式。
// 变更检测：feishu_doc_state(token → edit_time)，只产出变化的文档。
// 归属：文档 owner + 可见的已授权用户 都进 OwnerIDs，NAS sink fan-out 到各自目录（含"归属别人但我可见"副本）。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"time"

	feishuapi "fastgpt-sync-adapter/adapter/internal/feishu"
	"fastgpt-sync-adapter/adapter/internal/model"
	"fastgpt-sync-adapter/adapter/internal/storage"
)

// Config 飞书连接器配置。
type Config struct {
	AppID     string   `yaml:"app_id"`
	AppSecret string   `yaml:"app_secret"`
	Mode      string   `yaml:"mode"`    // tenant / user / both
	Folders   []string `yaml:"folders"` // tenant 模式遍历的共享文件夹 token 列表
}

// API 抽象（便于测试替换；feishuapi.Client 适配实现）。
type API interface {
	ListFolderChildren(token, folderToken string) ([]feishuapi.DriveFile, error)
	WalkFolder(token, folderToken string, depth int, fn func(feishuapi.DriveFile)) error
	RootFolderMeta(token string) (string, error)
	DownloadFile(token, fileToken string) (io.ReadCloser, error)
	DocxRawContent(token, documentID string) (string, error)
	DocRawContent(token, docToken string) (string, error)
	TenantAccessToken() (string, error)
}

// 限流：drive 下载约 5 QPS，FetchAttachment 前统一减速。
const downloadMinInterval = 220 * time.Millisecond

// Connector 飞书云文档连接器。
type Connector struct {
	cfg   Config
	api   API
	store *storage.Store

	tenantToken string
	pending     []model.Record // 本轮已扫描出的待产出行列（增量）
	lastDl      atomic.Int64   // 上次下载时间（纳秒），限流用
}

func New(cfg Config, store *storage.Store) *Connector {
	return &Connector{cfg: cfg, store: store}
}

// NewWithAPI 注入自定义 API（测试用）。
func NewWithAPI(cfg Config, store *storage.Store, api API) *Connector {
	return &Connector{cfg: cfg, store: store, api: api}
}

func (c *Connector) Name() string { return "feishu" }

func (c *Connector) Init(ctx context.Context) error {
	if c.api == nil {
		c.api = feishuapi.New(c.cfg.AppID, c.cfg.AppSecret)
	}
	if c.mode() != "user" {
		tat, err := c.api.TenantAccessToken()
		if err != nil {
			return fmt.Errorf("tenant_access_token: %w", err)
		}
		c.tenantToken = tat
	}
	return nil
}

func (c *Connector) mode() string {
	if c.cfg.Mode == "" {
		return "tenant"
	}
	return c.cfg.Mode
}

// docToRecord 文档 → 统一 Record。owner + viewer 都进 OwnerIDs（各存一份）。
func docToRecord(f feishuapi.DriveFile, viewerID string) model.Record {
	owners := []string{}
	if f.OwnerID != "" {
		owners = append(owners, f.OwnerID)
	}
	if viewerID != "" && viewerID != f.OwnerID {
		owners = append(owners, viewerID)
	}
	meta, _ := json.Marshal(map[string]any{
		"token": f.Token, "name": f.Name, "type": f.Type,
		"owner_id": f.OwnerID, "url": f.URL, "modified_time": f.ModifiedTime,
	})
	return model.Record{
		Source:    "feishu",
		Kind:      f.Type,
		ID:        f.Token,
		Timestamp: feishuapi.UnixSeconds(f.ModifiedTime) * 1000,
		OwnerIDs:  owners,
		SenderID:  f.OwnerID, // 归属以所有者为主
		Body:      meta,
		Attachments: []model.Attachment{{
			Name:   attachmentName(f),
			FileID: attachmentFileID(f),
		}},
	}
}

// attachmentFileID 附件标识：file 类型走 drive 下载；docx/doc 走 raw_content。
func attachmentFileID(f feishuapi.DriveFile) string {
	switch f.Type {
	case "file":
		return "file:" + f.Token
	case "docx":
		return "docx:" + f.Token
	case "doc":
		return "doc:" + f.Token
	}
	return f.Type + ":" + f.Token
}

// attachmentName 附件名（文档补 .md 后缀；上传类文件用原名）。
func attachmentName(f feishuapi.DriveFile) string {
	name := f.Name
	if name == "" {
		name = f.Token
	}
	switch f.Type {
	case "docx", "doc":
		if !strings.HasSuffix(name, ".md") {
			name += ".md"
		}
	}
	return name
}

// Fetch 产出变化的文档（pending 为空时先全量扫描一遍）。
func (c *Connector) Fetch(ctx context.Context, checkpoint string, limit int) ([]model.Record, string, error) {
	if len(c.pending) == 0 {
		if err := c.scan(ctx); err != nil {
			return nil, checkpoint, err
		}
	}
	n := limit
	if n <= 0 || n > len(c.pending) {
		n = len(c.pending)
	}
	batch := c.pending[:n]
	c.pending = c.pending[n:]
	next := "done"
	if len(c.pending) > 0 {
		next = "cont"
	}
	log.Printf("[feishu] 本批产出 %d 篇文档（剩余待产出 %d）", len(batch), len(c.pending))
	return batch, next, nil
}

// scan 全量扫描（tenant 共享文件夹 + 各已授权用户云空间），筛出变化的文档进 pending。
func (c *Connector) scan(ctx context.Context) error {
	mode := c.mode()
	seen := map[string]bool{}

	// tenant 模式：遍历配置的共享文件夹
	if (mode == "tenant" || mode == "both") && c.tenantToken != "" {
		for _, folder := range c.cfg.Folders {
			err := c.api.WalkFolder(c.tenantToken, folder, 10, func(f feishuapi.DriveFile) {
				c.collect(f, "", seen)
			})
			if err != nil {
				log.Printf("[feishu] 遍历共享文件夹 %s 失败: %v", folder, err)
			}
		}
	}

	// user 模式：每个已授权用户
	if mode == "user" || mode == "both" {
		grants, err := c.store.ListOAuthGrants("feishu")
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, g := range grants {
			if g.Status != "authorized" || g.AccessToken == "" || g.ExpireAt <= now {
				log.Printf("[feishu] 用户 %s 授权无效（status=%s），跳过", g.ExternalUserID, g.Status)
				continue
			}
			root, err := c.api.RootFolderMeta(g.AccessToken)
			if err != nil {
				log.Printf("[feishu] 拿 %s 根目录失败: %v", g.ExternalUserID, err)
				continue
			}
			err = c.api.WalkFolder(g.AccessToken, root, 10, func(f feishuapi.DriveFile) {
				c.collect(f, g.ExternalUserID, seen)
			})
			if err != nil {
				log.Printf("[feishu] 遍历 %s 云空间失败: %v", g.ExternalUserID, err)
			}
		}
	}
	log.Printf("[feishu] 扫描完成，待产出 %d 篇", len(c.pending))
	return nil
}

// collect 变更检测 + 入队（同 token 只入一次）。
func (c *Connector) collect(f feishuapi.DriveFile, viewerID string, seen map[string]bool) {
	if seen[f.Token] {
		return
	}
	seen[f.Token] = true
	editTime := feishuapi.UnixSeconds(f.ModifiedTime)
	if editTime > 0 && editTime <= c.store.GetDocEditTime(f.Token) {
		return // 未变化
	}
	rec := docToRecord(f, viewerID)
	c.pending = append(c.pending, rec)
	// 产出即记录状态（附件拉取失败下轮还会重试，因为 checkpoint 由引擎管）
	c.store.UpsertDocState(f.Token, f.Name, f.OwnerID, editTime)
}

// FetchAttachment 拉取文档内容（file 原始字节 / docx·doc 纯文本），带限流。
func (c *Connector) FetchAttachment(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
	c.throttle()
	scheme, token, _ := strings.Cut(a.FileID, ":")
	switch scheme {
	case "file":
		return c.api.DownloadFile(c.tenantToken, token)
	case "docx":
		text, err := c.api.DocxRawContent(c.tenantToken, token)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(text)), nil
	case "doc":
		text, err := c.api.DocRawContent(c.tenantToken, token)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(text)), nil
	}
	return nil, fmt.Errorf("暂不支持的文档类型: %s（sheet/bitable/slides 待做）", scheme)
}

// throttle 下载限速（≥220ms 间隔 ≈ 4.5 QPS）。
func (c *Connector) throttle() {
	now := time.Now().UnixNano()
	last := c.lastDl.Load()
	if wait := downloadMinInterval - time.Duration(now-last); wait > 0 && last > 0 {
		time.Sleep(wait)
	}
	c.lastDl.Store(time.Now().UnixNano())
}

func (c *Connector) Close() error { return nil }
