// Package nas Sink：把 Record 原样落盘到群晖 NAS。
// 目录结构：<root>/<source>/<groupID|direct>/<recordID>.json，附件放同级 attachments/ 子目录。
package nas

import (
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"strings"

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

type Sink struct {
	cfg    Config
	client *synology.Client
}

func New(cfg Config) *Sink { return &Sink{cfg: cfg} }

func (s *Sink) Name() string { return "nas" }

// Login 登录群晖（启动时调用）。
func (s *Sink) Login() error {
	s.client = synology.New(s.cfg.URL, s.cfg.Username, s.cfg.Password)
	return s.client.Login()
}

// Client 暴露底层群晖客户端（用户目录创建等场景用）。
func (s *Sink) Client() *synology.Client { return s.client }

// WriteRecord 原样落盘：Body 存 JSON 文件，附件经 read 拉字节后原样上传。
// 目录归属：按发送者归档 <root>/<source>/<发送者>/<群ID|direct>/<msgid>.json，
// 与「FastGPT 用户列表自动建目录」对应（sender userid = 目录名）。
func (s *Sink) WriteRecord(ctx context.Context, r model.Record, read sink.AttachmentReader) error {
	owner := r.SenderID
	if owner == "" {
		owner = "unknown"
	}
	group := r.GroupID
	if group == "" {
		group = "direct" // 单聊
	}
	// 清洗路径段，防止源端 ID 里的分隔符穿透目录
	dir := path.Join(s.cfg.RootPath, r.Source, sanitize(owner), sanitize(group))

	if err := s.client.UploadBytes(r.ID+".json", dir, r.Body); err != nil {
		return fmt.Errorf("写入消息体: %w", err)
	}

	for i := range r.Attachments {
		a := &r.Attachments[i]
		rc, err := read(ctx, a)
		if err != nil {
			log.Printf("[nas] 附件拉取失败 %s（%s）: %v", a.Name, a.FileID, err)
			continue // 单附件失败不影响消息体落盘
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			log.Printf("[nas] 附件读取失败 %s: %v", a.Name, err)
			continue
		}
		// 附件名带消息 ID 前缀防重名
		fname := fmt.Sprintf("%s_%s", r.ID, sanitize(a.Name))
		if err := s.client.UploadBytes(fname, path.Join(dir, "attachments"), data); err != nil {
			log.Printf("[nas] 附件上传失败 %s: %v", a.Name, err)
		}
	}
	return nil
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return strings.TrimSpace(s)
}
