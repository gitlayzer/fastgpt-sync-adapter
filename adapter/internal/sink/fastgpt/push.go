// Package fastgpt Push sink：周期性扫 NAS 按人归档目录，把文件推进各人的 FastGPT 知识库。
// 推送内容：会话 Markdown（conversations/*.md）+ 分类附件（<分类>/*）。
// 原始消息 messages/*.json 是归档保真数据，不进知识库。
// 幂等靠 storage.file_index 台账（person+source+nas_path ↔ collection_id + checksum）。
package fastgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"time"

	"fastgpt-sync-adapter/adapter/internal/storage"
	"fastgpt-sync-adapter/adapter/internal/synology"
	"fastgpt-sync-adapter/adapter/internal/uploader"
)

// Config 推送配置。
type Config struct {
	BaseURL  string
	APIKey   string
	Interval time.Duration
}

// NASFS 推送任务依赖的 NAS 操作子集（synology.Client 满足；测试可替换）。
type NASFS interface {
	List(folderPath string) ([]synology.FileEntry, error)
	Download(filePath string) (io.ReadCloser, error)
}

// 平台目录名 → 来源标识。
var platformSource = map[string]string{"企微": "wecom", "飞书": "feishu"}

// 不推送的目录：messages=原始归档保真（不进知识库）。
var skipDirs = map[string]bool{"messages": true, "#recycle": true}

// Task FastGPT 按人推送任务（实现 engine.Task）。
type Task struct {
	cfg     Config
	nas     NASFS
	root    string
	manager *uploader.Manager
	store   *storage.Store
}

func New(cfg Config, nas NASFS, root string, store *storage.Store) *Task {
	return &Task{
		cfg: cfg, nas: nas, root: strings.TrimRight(root, "/"),
		manager: uploader.NewManager(cfg.BaseURL, cfg.APIKey), store: store,
	}
}

func (t *Task) Name() string            { return "fastgpt-push" }
func (t *Task) Interval() time.Duration { return t.cfg.Interval }

// Run 扫一遍 NAS 并推送增量。
func (t *Task) Run(ctx context.Context) error {
	persons, err := t.nas.List(t.root)
	if err != nil {
		return fmt.Errorf("列 NAS 根目录: %w", err)
	}
	for _, p := range persons {
		if !p.IsDir || strings.HasPrefix(p.Name, "#") || strings.HasPrefix(p.Name, ".") {
			continue
		}
		if err := t.pushPerson(ctx, p.Name); err != nil {
			log.Printf("[fastgpt] 推送 %s 出错: %v", p.Name, err)
		}
	}
	return nil
}

// pushPerson 处理一个人的目录（<root>/<人名>/<平台>/...）。
func (t *Task) pushPerson(ctx context.Context, person string) error {
	platDirs, err := t.nas.List(path.Join(t.root, person))
	if err != nil {
		return err
	}
	datasetID := ""
	for _, plat := range platDirs {
		if !plat.IsDir {
			continue
		}
		source, ok := platformSource[plat.Name]
		if !ok {
			continue // 非平台目录（如杂项），跳过
		}
		// 用到时才建库
		if datasetID == "" {
			id, created, err := t.manager.EnsureDatasetByName(person)
			if err != nil {
				return fmt.Errorf("确保知识库: %w", err)
			}
			if created {
				log.Printf("[fastgpt] 为 %s 创建知识库（id=%s）", person, id)
			}
			datasetID = id
		}
		if err := t.pushPlatform(ctx, person, source, plat.Name, plat.Path, datasetID); err != nil {
			log.Printf("[fastgpt] %s/%s 推送出错: %v", person, plat.Name, err)
		}
	}
	return nil
}

// pushPlatform 处理 <root>/<人名>/<平台> 下的 conversations 与各分类目录。
func (t *Task) pushPlatform(ctx context.Context, person, source, platLabel, platPath, datasetID string) error {
	entries, err := t.nas.List(platPath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		if skipDirs[e.Name] {
			continue
		}
		if e.Name == "conversations" {
			// 会话 markdown：分类记为 会话
			if err := t.pushDir(ctx, person, source, "会话", e.Path, datasetID); err != nil {
				log.Printf("[fastgpt] %s/%s/conversations 出错: %v", person, platLabel, err)
			}
			continue
		}
		// 其余目录即分类目录（合同/方案/手册/其他…）
		if err := t.pushDir(ctx, person, source, e.Name, e.Path, datasetID); err != nil {
			log.Printf("[fastgpt] %s/%s/%s 出错: %v", person, platLabel, e.Name, err)
		}
	}
	return nil
}

// pushDir 推送一个目录下的文件（跳过 .meta.json）。
func (t *Task) pushDir(ctx context.Context, person, source, category, dir, datasetID string) error {
	files, err := t.nas.List(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDir || strings.HasSuffix(f.Name, ".meta.json") {
			continue
		}
		if err := t.pushFile(ctx, person, source, category, f, datasetID); err != nil {
			log.Printf("[fastgpt] 推送失败 %s: %v", f.Path, err)
		}
	}
	return nil
}

// pushFile 推送单个文件（带幂等判断）。
func (t *Task) pushFile(ctx context.Context, person, source, category string, f synology.FileEntry, datasetID string) error {
	checksum := fmt.Sprintf("%d:%d", f.Size, f.MTime.Unix())

	// 冻结（R6）优先判断：有 sibling .meta.json 且 frozen=true → 不推；已推过的删除
	frozen := t.isFrozen(f.Path)
	existing, _ := t.store.GetFileIndex(person, source, f.Path)
	if frozen {
		if existing != nil && existing.CollectionID != "" {
			c := t.manager.ClientFor(datasetID)
			if err := c.DeleteCollection(existing.CollectionID); err != nil {
				log.Printf("[fastgpt] 冻结删除失败 %s: %v", f.Path, err)
			} else {
				log.Printf("[fastgpt] 冻结删除 %s", f.Path)
			}
			existing.Frozen = true
			existing.CollectionID = ""
			t.store.UpsertFileIndex(existing)
		}
		return nil
	}

	// 台账命中且未变 → 跳过
	if existing != nil && existing.CollectionID != "" && existing.Checksum == checksum {
		return nil
	}

	// 读内容
	rc, err := t.nas.Download(f.Path)
	if err != nil {
		return fmt.Errorf("下载: %w", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("读取: %w", err)
	}

	// 展示名：附件去掉 <msgid>_ 前缀；会话 md 直接用文件名
	display := f.Name
	if source == "wecom" || source == "feishu" {
		if i := strings.Index(f.Name, "_"); i > 0 && category != "会话" {
			display = f.Name[i+1:]
		}
	}

	// 标签：分类 + 来源平台（前缀字符串形式）。
	// 注：FastGPT 对象式 {tag,value} 标签要求库内预先创建标签定义（仅 Pro UI 可建，无公开 API，
	// 否则报 tagNotExist）；字符串标签自动归并进 default_tag 数组，前缀保留维度区分。
	tags := []any{
		"分类:" + category,
		"来源:" + platformLabel(source),
	}

	c := t.manager.ClientFor(datasetID)
	c.Tags = tags
	collectionID, err := c.UploadBytes(display, f.Name, data)
	if err != nil {
		return fmt.Errorf("上传: %w", err)
	}
	// localFile 接口用表单文件名做展示名，二次调用修正为我们想要的名字
	_ = c.RenameCollection(collectionID, display)

	// 更新台账
	t.store.UpsertFileIndex(&storage.FileIndexEntry{
		Person: person, Source: source, NasPath: f.Path, Category: category,
		CollectionID: collectionID, Checksum: checksum,
	})
	log.Printf("[fastgpt] 推送 %s → %s（%s）", f.Path, person, category)
	return nil
}

// isFrozen 读 sibling .meta.json 判断冻结。
func (t *Task) isFrozen(nasPath string) bool {
	rc, err := t.nas.Download(nasPath + ".meta.json")
	if err != nil {
		return false
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return false
	}
	var m struct {
		Frozen bool `json:"frozen"`
	}
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	return m.Frozen
}

func platformLabel(source string) string {
	switch source {
	case "wecom":
		return "企微"
	case "feishu":
		return "飞书"
	}
	return source
}
