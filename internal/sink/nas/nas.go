// Package nas Sink：把 Record 落盘到群晖 NAS，按人归档。
// 目录结构（用户决策，第一层人名以身份解析器查出的姓名为准）：
//
// 企微（消息）：
//   <root>/<人名>/企微/conversations/<会话>.md         会话正文（滚动追加，去重）
//   <root>/<人名>/企微/<msgid>_<附件名>                 附件原件（平铺）
//
// 飞书（文件）：
//   <root>/<人名>/飞书/<文件名>                        源文件平铺（.docx/.xlsx/.pdf/.md 等）
//
// 说明：
//   - 企微不再落 messages/*.json。原始 JSON 混有大量协议字段（encrypt_*、seq 等），
//     对归档和检索无价值；只保留会话正文与附件本体。
//   - 飞书只存源文件，不做格式转换（导出得到的就是完整源文件）。
//   - 群聊/多参与者的会话，每个参与者目录下各写一份完整记录（含附件）。
package nas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/classifier"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/identity"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/render"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/sink"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
)

type Config struct {
	URL           string            `yaml:"url"`
	Username      string            `yaml:"username"`
	Password      string            `yaml:"password"`
	RootPath      string            `yaml:"root_path"`         // 落盘根路径，如 /FastGPT-test
	GroupNames    map[string]string `yaml:"group_names"`       // 显式配置的群聊 ID -> 群名称映射
	MaxFileSizeMB int64             `yaml:"max_file_size_mb"`  // 单附件上限（MB），默认 200MB，防大文件打爆内存 OOM
}

const defaultMaxFileSizeMB = 200

// ErrOversized 表示文件大小超出上限，跳过以保护内存防 OOM。
var ErrOversized = errors.New("文件大小超出上限，跳过下载以保护内存")

func (s *Sink) maxFileSize() int64 {
	if s.cfg.MaxFileSizeMB > 0 {
		return s.cfg.MaxFileSizeMB * 1024 * 1024
	}
	return defaultMaxFileSizeMB * 1024 * 1024
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
	Delete(filePath string) error
}

type Sink struct {
	cfg    Config
	client *synology.Client
	fs     FS
	clf    *classifier.Classifier
	ids    identity.Resolver
	store  *storage.Store

	mu           sync.Mutex
	batchMode    bool
	convCache    map[string][]byte
	pendingConvs map[string]*pendingConv
}

type pendingConv struct {
	dir      string
	filename string
	content  []byte
	logged   []msgLoggedRecord
}

type msgLoggedRecord struct {
	msgID   string
	person  string
	msgTime int64
}

func New(cfg Config, clf *classifier.Classifier, ids identity.Resolver, store *storage.Store) *Sink {
	return &Sink{
		cfg:          cfg,
		clf:          clf,
		ids:          ids,
		store:        store,
		convCache:    make(map[string][]byte),
		pendingConvs: make(map[string]*pendingConv),
	}
}

func (s *Sink) Name() string { return "nas" }

// SetBatchMode 开启/关闭批次缓冲模式。
// 开启后，对会话 Markdown 的追加会缓存在内存中并去重合并，避免频繁读写群晖 WebAPI。
func (s *Sink) SetBatchMode(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchMode = enabled
}

// Flush 刷写批次缓冲中所有的会话文件到 NAS。
func (s *Sink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.pendingConvs) == 0 {
		return nil
	}

	for full, p := range s.pendingConvs {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.fs.UploadBytes(p.filename, p.dir, p.content); err != nil {
			return fmt.Errorf("刷写会话文件 %s: %w", full, err)
		}
		// 上传成功后才打标 SQLite，保证数据落地才记状态
		if s.store != nil {
			for _, m := range p.logged {
				_ = s.store.LogMessage(m.msgID, m.person, m.msgTime)
			}
		}
	}

	// 刷写完毕后重置待写队列与缓存
	s.pendingConvs = make(map[string]*pendingConv)
	s.convCache = make(map[string][]byte)
	return nil
}

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
			maxSize := s.maxFileSize()
			if a.Size > maxSize {
				log.Printf("[nas] 附件 %s 大小超出限制（%.1f MB > %d MB），跳过读取以保护内存",
					a.Name, float64(a.Size)/(1024*1024), maxSize/(1024*1024))
				return nil, ErrOversized
			}
			rc, err := read(ctx, a)
			if err != nil {
				return nil, err
			}
			lr := io.LimitReader(rc, maxSize+1)
			data, err := io.ReadAll(lr)
			rc.Close()
			if err != nil {
				return nil, err
			}
			if int64(len(data)) > maxSize {
				log.Printf("[nas] 附件 %s 实际内容超出限制（> %d MB），已跳过归档以保护内存",
					a.Name, maxSize/(1024*1024))
				return nil, ErrOversized
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
			if u == "" || seen[u] {
				return
			}
			// 企微外部联系人/客户（以 wm/wo 开头）不建独立的员工归档根目录，
			// 避免在 NAS 产生海量客户空目录与冗余附件上传，但内部员工目录内依然完整保留与客户的对话
			if r.Source == "wecom" && (strings.HasPrefix(u, "wm") || strings.HasPrefix(u, "wo")) {
				return
			}
			seen[u] = true
			users = append(users, u)
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

		// 飞书云端删除事件：同步清理 NAS 物理文件
		if r.Kind == "delete" {
			for _, uid := range users {
				person := sanitize(nameOf(uid))
				if person == "" {
					continue
				}
				base := path.Join(s.cfg.RootPath, person, platform)
				for _, a := range r.Attachments {
					for _, sub := range []string{"云文档", "邮件", ""} {
						targetFile := path.Join(base, sub, sanitize(a.Name))
						// 删除失败要返回错误：不 Ack，下一轮重试（文件不存在已在 Delete 内视为成功）
						if err := s.fs.Delete(targetFile); err != nil {
							return err
						}
					}
					log.Printf("[nas] 飞书端已删除，同步清理 NAS: %s/%s", base, sanitize(a.Name))
				}
			}
			return nil
		}

		var firstErr error
	for _, uid := range users {
		if ctx.Err() != nil {
			return ctx.Err() // 停止信号：剩余用户目录由下轮（消息去重保证幂等）补齐
		}
		person := sanitize(nameOf(uid))
		if person == "" {
			person = "unknown"
		}
		if err := s.writeForPerson(ctx, r, m, parseErr == nil, person, uid, platform, cachedRead, nameOf); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 内层（附件拉取/上传）已经打过具体原因，这里只标出是谁的目录失败，
			// 否则同一错误会在 nas/engine 两层各刷一遍
			log.Printf("[nas] %s 的目录写入未完成（id=%s）", person, r.ID)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// writeForPerson 把一条消息写到单个参与者的目录（会话 md + 附件）。
func (s *Sink) writeForPerson(ctx context.Context, r model.Record, m *render.Msg, msgOK bool,
	person, uid, platform string, read sink.AttachmentReader, nameOf func(string) string) error {

	base := path.Join(s.cfg.RootPath, person, platform)

	// 飞书文档：只存源文件本体，平铺在 <人名>/飞书/ 下，不写 messages JSON、不按分类建子目录。
	// 理由：飞书这边同步的是「文件」而非「消息」，导出的 .docx/.xlsx/.pdf 即完整源文件，
	// 再存一份元数据 JSON 和分类目录只会让目录变乱。
	if r.Source == "feishu" {
		return s.writeFeishuFiles(ctx, r, base, read)
	}

	// 企微：不落 messages/*.json 原文。
	// 原始 JSON 里混着大量协议字段（encrypt_*、publickey_ver、seq 等），
	// 对下游（NAS 归档 / FastGPT 检索）没有价值，反而占空间、干扰阅读。
	// 只保留：会话正文 Markdown（滚动追加）+ 附件原件。

		// 1) 会话正文 Markdown 滚动追加（按 消息×人 去重）
		if msgOK && r.Source == "wecom" {
			if logged, err := s.store.IsMessageLogged(r.ID, person); err == nil && !logged {
				if line, skip := render.RenderLine(m, nameOf); !skip {
					convFile := s.convFileName(m, uid, nameOf)
					if err := s.appendFileWithMeta(path.Join(base, "群聊记录"), convFile, line, r.ID, person, r.Timestamp); err != nil {
						return fmt.Errorf("追加会话 markdown: %w", err)
					}
				} else {
					_ = s.store.LogMessage(r.ID, person, r.Timestamp)
				}
			}
		}

		// 2) 附件：普通文件按发送日期组织在 <人名>/企微/<YYYY-MM-DD>/ 下（方案 A）；
		// 图片消息则归档至 <人名>/企微/群聊记录/images/<YYYY-MM-DD>/ 下，供会话 Markdown 相对路径出图。
		dateFolder := time.UnixMilli(r.Timestamp).Format("2006-01-02")
		if dateFolder == "" || r.Timestamp <= 0 {
			dateFolder = time.Now().Format("2006-01-02")
		}
		targetDir := path.Join(base, dateFolder)
		if r.Kind == "image" {
			targetDir = path.Join(base, "群聊记录", "images", dateFolder)
		}

			for i := range r.Attachments {
				if ctx.Err() != nil {
					return ctx.Err() // 停止信号：附件下载是耗时大头，必须能中断
				}
				a := &r.Attachments[i]
				maxSize := s.maxFileSize()
				if a.Size > maxSize {
					log.Printf("[nas] 附件 %s 超出大小限制（%.1f MB > %d MB），跳过归档以保护内存",
						a.Name, float64(a.Size)/(1024*1024), maxSize/(1024*1024))
					continue
				}
					rc, err := read(ctx, a)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						if errors.Is(err, ErrOversized) {
							continue
						}
						log.Printf("[nas] 附件拉取失败 %s（%s）: %v", a.Name, a.FileID, err)
						continue
					}
				lr := io.LimitReader(rc, maxSize+1)
				data, err := io.ReadAll(lr)
				rc.Close()
				if err != nil {
					log.Printf("[nas] 附件读取失败 %s: %v", a.Name, err)
					continue
				}
				if int64(len(data)) > maxSize {
					log.Printf("[nas] 附件 %s 实际内容超出大小限制（> %d MB），已跳过归档以保护内存",
						a.Name, maxSize/(1024*1024))
					continue
				}

				cleanName := sanitize(a.Name)
			if cleanName == "" {
				cleanName = a.FileID
			}
			if err := s.fs.UploadBytes(cleanName, targetDir, data); err != nil {
				log.Printf("[nas] 附件上传失败 %s: %v", a.Name, err)
				continue
			}
			log.Printf("[nas] 已归档 %s/%s（%.1f KB）", targetDir, cleanName, float64(len(data))/1024)
		}
		return nil
}

// convFileName 计算查看者视角的会话文件名。
// 单聊：以对方姓名命名（与李四的单聊.md）；
// 群聊：优先使用配置或数据库中的群名；若无则智能提取群内实名员工姓名（如 群聊_罗静怡、王永国等人的群聊.md）；未解析出员工时兜底使用 roomid。
func (s *Sink) convFileName(m *render.Msg, forUser string, nameOf func(string) string) string {
	if m.RoomID == "" {
		counterpart := ""
		for _, u := range m.Participants() {
			if u != forUser {
				counterpart = u
				break
			}
		}
		if counterpart == "" {
			counterpart = m.From
		}
		name := counterpart
		if nameOf != nil {
			name = nameOf(counterpart)
		}
		return fmt.Sprintf("与%s的单聊.md", sanitize(name))
	}

	// 1. 先查配置里的群名映射
	if len(s.cfg.GroupNames) > 0 {
		if gn, ok := s.cfg.GroupNames[m.RoomID]; ok && gn != "" {
			return fmt.Sprintf("群聊_%s.md", sanitize(gn))
		}
	}

	// 2. 查数据库记录的群名
	if s.store != nil {
		if gn := s.store.GetRoomName(m.RoomID); gn != "" {
			return fmt.Sprintf("群聊_%s.md", sanitize(gn))
		}
	}

	// 3. 智能推断：根据已知群成员姓名生成自然语言群名（如 "群聊_罗静怡、王永国、张建丰等人的群聊.md"）
	if s.store != nil {
		members, err := s.store.ListRoomParticipants(m.RoomID)
		if err == nil && len(members) > 0 {
			var memberNames []string
			seenNames := map[string]bool{}
			for _, uid := range members {
				if strings.HasPrefix(uid, "wm") || strings.HasPrefix(uid, "wo") {
					continue
				}
				name := uid
				if nameOf != nil {
					name = nameOf(uid)
				}
				if strings.HasPrefix(name, "wm") || strings.HasPrefix(name, "wo") || name == "" {
					continue
				}
				if !seenNames[name] {
					seenNames[name] = true
					memberNames = append(memberNames, name)
				}
			}
			if len(memberNames) > 0 {
				limit := 3
				if len(memberNames) < limit {
					limit = len(memberNames)
				}
				joined := strings.Join(memberNames[:limit], "、")
				if len(memberNames) > limit {
					return fmt.Sprintf("群聊_%s等人的群聊.md", sanitize(joined))
				}
				return fmt.Sprintf("群聊_%s的群聊.md", sanitize(joined))
			}
		}
	}

	// 4. 最终兜底使用 roomid
	return fmt.Sprintf("群聊_%s.md", sanitize(m.RoomID))
}

// appendFile 滚动追加（保持旧签名兼容测试）。
func (s *Sink) appendFile(dir, filename, line string) error {
	return s.appendFileWithMeta(dir, filename, line, "", "", 0)
}

// appendFileWithMeta 滚动追加：读旧内容（优先读内存缓存，避免频繁下载）→ 拼新行 → 写入（批次模式下入队统一刷写）。
func (s *Sink) appendFileWithMeta(dir, filename, line, msgID, person string, msgTime int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	full := path.Join(dir, filename)
	var existing []byte
	if cached, ok := s.convCache[full]; ok {
		existing = cached
	} else {
		if rc, err := s.fs.Download(full); err == nil {
			data, readErr := io.ReadAll(rc)
			closeErr := rc.Close()
			if readErr != nil {
				return fmt.Errorf("读取已有会话文件 %s: %w", full, readErr)
			}
			if closeErr != nil {
				return fmt.Errorf("关闭已有会话文件 %s: %w", full, closeErr)
			}
			existing = stripSynologyHTMLPrefix(data)
		} else if !errors.Is(err, synology.ErrFileNotFound) {
			return fmt.Errorf("下载已有会话文件 %s: %w", full, err)
		}

		if len(existing) == 0 {
			title := strings.TrimSuffix(filename, ".md")
			existing = []byte("# " + title + "\n\n")
		} else {
			// 旧版可能把群晖返回的 HTML 错误页误当成已有 Markdown。
			// 清掉 HTML 前缀后补回标题，保留其后已追加的会话行。
			if !strings.HasPrefix(strings.TrimSpace(string(existing)), "# ") {
				title := strings.TrimSuffix(filename, ".md")
				existing = append([]byte("# "+title+"\n\n"), existing...)
			}
		}
	}

	// RAG 优化：会话按日分块（若该日期在当前文件中尚未出现，则插入二级标题）
	if date := extractDateFromLine(line); date != "" {
		dateHeader := "## " + date
		if !strings.Contains(string(existing), dateHeader) {
			if !strings.HasSuffix(string(existing), "\n\n") {
				if strings.HasSuffix(string(existing), "\n") {
					existing = append(existing, '\n')
				} else {
					existing = append(existing, "\n\n"...)
				}
			}
			existing = append(existing, []byte(dateHeader+"\n\n")...)
		}
	}

	existing = append(existing, []byte(line)...)
	s.convCache[full] = existing

	if !s.batchMode {
		// 非批次模式：直接单条上传覆盖
		if err := s.fs.UploadBytes(filename, dir, existing); err != nil {
			return err
		}
		if s.store != nil && msgID != "" {
			_ = s.store.LogMessage(msgID, person, msgTime)
		}
		return nil
	}

	// 批次模式：加入待刷写队列
	p, found := s.pendingConvs[full]
	if !found {
		p = &pendingConv{
			dir:      dir,
			filename: filename,
		}
		s.pendingConvs[full] = p
	}
	p.content = existing
	if msgID != "" {
		p.logged = append(p.logged, msgLoggedRecord{msgID: msgID, person: person, msgTime: msgTime})
	}
	return nil
}

// extractDateFromLine 从会话行中提取 YYYY-MM-DD 日期。
func extractDateFromLine(line string) string {
	idx := strings.Index(line, "[")
	if idx == -1 {
		return ""
	}
	sub := line[idx+1:]
	spaceIdx := strings.Index(sub, " ")
	if spaceIdx == 10 && len(sub) >= 10 {
		date := sub[:10]
		if date[4] == '-' && date[7] == '-' {
			return date
		}
	}
	return ""
}

// stripSynologyHTMLPrefix 清理旧版追加逻辑误存进 .md 的群晖 HTML 页面。
// 如果页面后面已有会话 Markdown 行，则保留这些行；否则按空文件处理。
func stripSynologyHTMLPrefix(data []byte) []byte {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "<!doctype html") && !strings.Contains(lower, "<html") && !strings.Contains(lower, "synology inc.") {
		return []byte(text)
	}
	lines := strings.SplitAfter(text, "\n")
	offset := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "- **[") || strings.HasPrefix(trimmed, "- *[") {
			return []byte(text[offset:])
		}
		offset += len(line)
	}
	return nil
}

// writeFeishuFiles 飞书文档归档：把源文件平铺写到 base 目录。
//
// 不写 messages/*.json、不按分类建子目录——飞书侧同步的是「文件」而非「消息」，
// 导出的 .docx/.xlsx/.pdf 本身就是完整内容，再加元数据和分类目录只会让结构变乱。
func (s *Sink) writeFeishuFiles(ctx context.Context, r model.Record, base string,
	read sink.AttachmentReader) error {

	if len(r.Attachments) == 0 {
		return nil
	}
	var firstErr error
	for i := range r.Attachments {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a := &r.Attachments[i]
		maxSize := s.maxFileSize()
		if a.Size > maxSize {
			log.Printf("[nas] 飞书文件 %s 超出大小限制（%.1f MB > %d MB），跳过归档以保护内存",
				a.Name, float64(a.Size)/(1024*1024), maxSize/(1024*1024))
			continue
		}
			rc, err := read(ctx, a)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(err, ErrOversized) {
					continue
				}
				log.Printf("[nas] 飞书文件拉取失败 %s: %v", a.Name, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		lr := io.LimitReader(rc, maxSize+1)
		data, err := io.ReadAll(lr)
		rc.Close()
		if err != nil {
			log.Printf("[nas] 飞书文件读取失败 %s: %v", a.Name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if int64(len(data)) > maxSize {
			log.Printf("[nas] 飞书文件 %s 实际内容超出大小限制（> %d MB），已跳过归档以保护内存",
				a.Name, maxSize/(1024*1024))
			continue
		}

		// 飞书统一两大归档目录：所有云文档/表格/知识库/纪要全部归于 云文档/，邮件附件归于 邮件/
			destDir := path.Join(base, "云文档")
			if r.Kind == "mail" {
				destDir = path.Join(base, "邮件")
			}
			if err := s.fs.UploadBytes(sanitize(a.Name), destDir, data); err != nil {
				log.Printf("[nas] 飞书文件上传失败 %s: %v", a.Name, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			log.Printf("[nas] 已归档 %s/%s（%.1f KB）", destDir, a.Name, float64(len(data))/1024)
			// 文档改名：新文件写成功后再删旧文件，中途失败也不会丢文件
			if a.PrevName != "" && a.PrevName != a.Name {
				if err := s.fs.Delete(path.Join(destDir, sanitize(a.PrevName))); err != nil {
					log.Printf("[nas] 删除改名前的旧文件失败 %s: %v", a.PrevName, err)
					if firstErr == nil {
						firstErr = err
					}
				}
			}
	}
	return firstErr
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return strings.TrimSpace(s)
}
