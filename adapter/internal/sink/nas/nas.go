// Package nas Sink：把 Record 原样落盘到群晖 NAS。
// 目录结构（用户决策）：<root>/<平台>/<发送者>/messages/<msgid>.json（消息原文）
//                      <root>/<平台>/<发送者>/<分类>/<附件名>（附件，按分类器归档）
// 附件旁写 <附件名>.meta.json 侧边车（分类/来源/权限快照/冻结标记），原文永不改动。
package nas

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"fastgpt-sync-adapter/adapter/internal/classifier"
	"fastgpt-sync-adapter/adapter/internal/model"
	"fastgpt-sync-adapter/adapter/internal/sink"
	"fastgpt-sync-adapter/adapter/internal/synology"
)

type Config struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	RootPath string `yaml:"root_path"` // 落盘根路径，如 /FastGPT-test
}

// 平台显示名（目录第一层）。
var sourceLabel = map[string]string{
	"wecom":  "企业微信",
	"feishu": "飞书",
}

type Sink struct {
	cfg    Config
	client *synology.Client
	clf    *classifier.Classifier
}

func New(cfg Config, clf *classifier.Classifier) *Sink {
	return &Sink{cfg: cfg, clf: clf}
}

func (s *Sink) Name() string { return "nas" }

// Login 登录群晖（启动时调用）。
func (s *Sink) Login() error {
	s.client = synology.New(s.cfg.URL, s.cfg.Username, s.cfg.Password)
	return s.client.Login()
}

// Client 暴露底层群晖客户端（serve 模式与用户目录创建复用）。
func (s *Sink) Client() *synology.Client { return s.client }

// Meta 附件侧边车元数据（R4 分类 + R5/R6 权限快照与冻结）。
type Meta struct {
	RecordID   string   `json:"record_id"`         // 来源消息 ID
	Source     string   `json:"source"`            // wecom / feishu
	SenderID   string   `json:"sender_id"`         // 发送者（userid；姓名映射待通讯录 API）
	GroupID    string   `json:"group_id"`          // 群 ID（空=单聊）
	Category   string   `json:"category"`          // 分类标签（合同/方案/手册/其他…）
	OwnerIDs   []string `json:"owner_ids"`         // 权限快照：可见人集合
	MsgTime    int64    `json:"msg_time"`          // 源端时间（毫秒）
	ArchivedAt string   `json:"archived_at"`       // 归档时间
	Frozen     bool     `json:"frozen"`            // R6：冻结后 serve 不再返回、不再增量
	FrozenReason string `json:"frozen_reason,omitempty"`
}

// WriteRecord 原样落盘：消息体存 messages/，附件按分类归档 + 写 meta 侧边车。
func (s *Sink) WriteRecord(ctx context.Context, r model.Record, read sink.AttachmentReader) error {
	platform := sourceLabel[r.Source]
	if platform == "" {
		platform = r.Source
	}
	owner := sanitize(r.SenderID)
	if owner == "" {
		owner = "unknown"
	}
	base := path.Join(s.cfg.RootPath, platform, owner)

	// 1) 消息原文（含撤回等 action）原样落 messages/
	if err := s.client.UploadBytes(r.ID+".json", path.Join(base, "messages"), r.Body); err != nil {
		return fmt.Errorf("写入消息体: %w", err)
	}

	// 2) 附件：分类归档 + meta 侧边车
	for i := range r.Attachments {
		a := &r.Attachments[i]
		rc, err := read(ctx, a)
		if err != nil {
			log.Printf("[nas] 附件拉取失败 %s（%s）: %v", a.Name, a.FileID, err)
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			log.Printf("[nas] 附件读取失败 %s: %v", a.Name, err)
			continue
		}

		category := s.clf.Classify(a.Name, "")
		fname := fmt.Sprintf("%s_%s", r.ID, sanitize(a.Name))
		dir := path.Join(base, category)
		if err := s.client.UploadBytes(fname, dir, data); err != nil {
			log.Printf("[nas] 附件上传失败 %s: %v", a.Name, err)
			continue
		}

		meta := Meta{
			RecordID:   r.ID,
			Source:     r.Source,
			SenderID:   r.SenderID,
			GroupID:    r.GroupID,
			Category:   category,
			OwnerIDs:   r.OwnerIDs,
			MsgTime:    r.Timestamp,
			ArchivedAt: time.Now().Format(time.RFC3339),
		}
		if mdata, err := json.MarshalIndent(meta, "", "  "); err == nil {
			if err := s.client.UploadBytes(fname+".meta.json", dir, mdata); err != nil {
				log.Printf("[nas] meta 写入失败 %s: %v", a.Name, err)
			}
		}
	}
	return nil
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return strings.TrimSpace(s)
}
