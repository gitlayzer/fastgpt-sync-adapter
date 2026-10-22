// Package nas Sink：把 Record 落盘到群晖 NAS，按人归档。
// 目录结构（用户决策，第一层人名以身份解析器查出的姓名为准）：
//   <root>/<人名>/<平台>/messages/<msgid>.json            消息原文（幂等覆盖，不清洗红线）
//   <root>/<人名>/<平台>/conversations/<会话>.md         会话 Markdown（滚动追加，去重）
//   <root>/<人名>/<平台>/<分类>/<msgid>_<附件名>          附件 + .meta.json 侧边车
// 群聊/多参与者的会话，每个参与者目录下各写一份完整记录（含附件）。
package nas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"fastgpt-sync-adapter/adapter/internal/classifier"
	"fastgpt-sync-adapter/adapter/internal/identity"
	"fastgpt-sync-adapter/adapter/internal/model"
	"fastgpt-sync-adapter/adapter/internal/render"
	"fastgpt-sync-adapter/adapter/internal/sink"
	"fastgpt-sync-adapter/adapter/internal/storage"
	"fastgpt-sync-adapter/adapter/internal/synology"
)

type Config struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	RootPath string `yaml:"root_path"` // 落盘根路径，如 /FastGPT-test
}

// 平台显示名（目录第二层）。
var sourceLabel = map[string]string{
	"wecom":  "企微",
	"feishu": "飞书",
}

// FS sink 依赖的文件操作子集（synology.Client 天然满足；测试可替换）。
type FS interface {
	UploadBytes(filename, destDir string, data []byte) error
	Download(filePath string) (io.ReadCloser, error)
}

type Sink struct {
	cfg    Config
	client *synology.Client
	fs     FS
	clf    *classifier.Classifier
	ids    identity.Resolver
	store  *storage.Store
}

func New(cfg Config, clf *classifier.Classifier, ids identity.Resolver, store *storage.Store) *Sink {
	return &Sink{cfg: cfg, clf: clf, ids: ids, store: store}
}

func (s *Sink) Name() string { return "nas" }

// Login 登录群晖（启动时调用）。
func (s *Sink) Login() error {
	s.client = synology.New(s.cfg.URL, s.cfg.Username, s.cfg.Password)
	if err := s.client.Login(); err != nil {
		return err
	}
	s.fs = s.client
	return nil
}

// Client 暴露底层群晖客户端（serve 模式复用）。
func (s *Sink) Client() *synology.Client { return s.client }

// Meta 附件侧边车元数据（R4 分类 + R5/R6 权限快照与冻结）。
type Meta struct {
	RecordID     string   `json:"record_id"`
	Source       string   `json:"source"`
	SenderID     string   `json:"sender_id"`
	GroupID      string   `json:"group_id"`
	Category     string   `json:"category"`
	OwnerIDs     []string `json:"owner_ids"`           // 权限快照：可见人（原始 id）
	Persons      []string `json:"persons,omitempty"`   // 解析后的人名
	MsgTime      int64    `json:"msg_time"`
	ArchivedAt   string   `json:"archived_at"`
	Frozen       bool     `json:"frozen"`
	FrozenReason string   `json:"frozen_reason,omitempty"`
}

// WriteRecord 把一条消息写到所有参与者的个人目录。
func (s *Sink) WriteRecord(ctx context.Context, r model.Record, read sink.AttachmentReader) error {
	platform := sourceLabel[r.Source]
	if platform == "" {
		platform = r.Source
	}

	m, parseErr := render.Parse(r.Body)

	// 附件字节缓存：一条记录 fan-out 到多人时，同一附件只拉一次（飞书文档 owner+可见人场景省一半下载）
	attCache := map[string][]byte{}
	cachedRead := func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
		if data, ok := attCache[a.FileID]; ok {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		rc, err := read(ctx, a)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		attCache[a.FileID] = data
		return io.NopCloser(bytes.NewReader(data)), nil
	}

	// 群成员学习：企微群聊 tolist 为空，靠发言记录逐步积累成员
	if parseErr == nil && m.RoomID != "" {
		if err := s.store.AddRoomParticipant(m.RoomID, m.From); err != nil {
			log.Printf("[nas] 群成员学习失败 %s/%s: %v", m.RoomID, m.From, err)
		}
	}

	// 参与者集合：OwnerIDs（可见人集合：企微=from+tolist / 飞书=owner+可见授权用户）
	// + 消息解析出的参与者 + 已知群成员
	var users []string
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	for _, u := range r.OwnerIDs {
		add(u)
	}
	if parseErr == nil {
		for _, u := range m.Participants() {
			add(u)
		}
		if m.RoomID != "" {
			if members, err := s.store.ListRoomParticipants(m.RoomID); err == nil {
				for _, u := range members {
					add(u)
				}
			}
		}
	}
	if len(users) == 0 {
		add(r.SenderID) // 解析失败兜底：至少写发送者目录
	}

	nameOf := func(uid string) string {
		if s.ids == nil {
			return uid
		}
		return s.ids.PersonName(r.Source, uid)
	}

	var firstErr error
	for _, uid := range users {
		person := sanitize(nameOf(uid))
		if person == "" {
			person = "unknown"
		}
		if err := s.writeForPerson(ctx, r, m, parseErr == nil, person, uid, platform, cachedRead, users, nameOf); err != nil {
			log.Printf("[nas] 写入失败 person=%s id=%s: %v", person, r.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// writeForPerson 把一条消息写到单个参与者的目录（原文 + 会话 md + 附件）。
func (s *Sink) writeForPerson(ctx context.Context, r model.Record, m *render.Msg, msgOK bool,
	person, uid, platform string, read sink.AttachmentReader, allUsers []string, nameOf func(string) string) error {

	base := path.Join(s.cfg.RootPath, person, platform)

	// 1) 消息原文 JSON（幂等覆盖，重试无害）
	if err := s.fs.UploadBytes(r.ID+".json", path.Join(base, "messages"), r.Body); err != nil {
		return fmt.Errorf("写入消息体: %w", err)
	}

	// 2) 会话 Markdown 滚动追加（仅企微对话；飞书文档无会话概念，按 消息×人 去重）
	if msgOK && r.Source == "wecom" {
		if logged, err := s.store.IsMessageLogged(r.ID, person); err == nil && !logged {
			if line, skip := render.RenderLine(m, nameOf); !skip {
				convFile := m.ConvFileName(uid, nameOf)
				if err := s.appendFile(path.Join(base, "conversations"), convFile, line); err != nil {
					return fmt.Errorf("追加会话 markdown: %w", err)
				}
			}
			if err := s.store.LogMessage(r.ID, person, r.Timestamp); err != nil {
				log.Printf("[nas] 消息去重记录失败 %s/%s: %v", r.ID, person, err)
			}
		}
	}

	// 3) 附件：分类归档 + meta 侧边车（幂等覆盖）
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
		if err := s.fs.UploadBytes(fname, dir, data); err != nil {
			log.Printf("[nas] 附件上传失败 %s: %v", a.Name, err)
			continue
		}

		persons := make([]string, 0, len(allUsers))
		for _, u := range allUsers {
			persons = append(persons, nameOf(u))
		}
		meta := Meta{
			RecordID: r.ID, Source: r.Source, SenderID: r.SenderID, GroupID: r.GroupID,
			Category: category, OwnerIDs: r.OwnerIDs, Persons: persons,
			MsgTime: r.Timestamp, ArchivedAt: time.Now().Format(time.RFC3339),
		}
		if mdata, err := json.MarshalIndent(meta, "", "  "); err == nil {
			if err := s.fs.UploadBytes(fname+".meta.json", dir, mdata); err != nil {
				log.Printf("[nas] meta 写入失败 %s: %v", a.Name, err)
			}
		}
	}
	return nil
}

// appendFile 滚动追加：读旧内容（不存在视为空，首行加标题）→ 拼新行 → 覆盖上传。
func (s *Sink) appendFile(dir, filename, line string) error {
	full := path.Join(dir, filename)
	var existing []byte
	if rc, err := s.fs.Download(full); err == nil {
		existing, _ = io.ReadAll(rc)
		rc.Close()
	}
	var buf strings.Builder
	if len(existing) == 0 {
		title := strings.TrimSuffix(filename, ".md")
		buf.WriteString("# " + title + "\n\n")
	} else {
		buf.Write(existing)
	}
	buf.WriteString(line)
	return s.fs.UploadBytes(filename, dir, []byte(buf.String()))
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return strings.TrimSpace(s)
}
