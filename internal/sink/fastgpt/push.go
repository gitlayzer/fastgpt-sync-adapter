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

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/uploader"
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

// pushPlatform 处理 <root>/<人名>/<平台> 下的 conversations 与各日期/分类目录及平铺文件。
func (t *Task) pushPlatform(ctx context.Context, person, source, platLabel, platPath, datasetID string) error {
	entries, err := t.nas.List(platPath)
	if err != nil {
		return err
	}
	activePaths := map[string]bool{}
	listFailed := false // 有目录没列出来时，本轮不做反向对账，避免把没列到的文件当成已删除
	for _, e := range entries {
		if skipDirs[e.Name] {
			continue
		}
		if !e.IsDir {
			// 平铺在平台根目录下的文件：按文件名自动归类并推送
			category := classifyFilename(e.Name)
			activePaths[e.Path] = true
			if err := t.pushFile(ctx, person, source, category, e, datasetID); err != nil {
				log.Printf("[fastgpt] 推送失败 %s: %v", e.Path, err)
			}
			continue
		}
		if e.Name == "群聊记录" || e.Name == "conversations" {
			// 会话 markdown：分类记为 会话
			if err := t.pushDir(ctx, person, source, "会话", e.Path, datasetID, activePaths); err != nil {
				log.Printf("[fastgpt] %s/%s/%s 出错: %v", person, platLabel, e.Name, err)
				listFailed = true
			}
			continue
		}
		// 日期子目录（YYYY-MM-DD，方案 A）、云文档/、邮件/ 或传统分类目录（合同/方案等）
		isDateDir := len(e.Name) == 10 && e.Name[4] == '-' && e.Name[7] == '-'
		category := e.Name
		if isDateDir || e.Name == "云文档" || e.Name == "邮件" {
			category = "" // 由各文件按文件名关键词自动归类
		}
		if err := t.pushDir(ctx, person, source, category, e.Path, datasetID, activePaths); err != nil {
			log.Printf("[fastgpt] %s/%s/%s 出错: %v", person, platLabel, e.Name, err)
			listFailed = true
		}
	}
	if listFailed {
		log.Printf("[fastgpt] %s/%s 本轮有目录未列出，跳过反向对账", person, platLabel)
		return nil
	}

	// 反向对账：检测 NAS 上已删除但在 FastGPT 知识库与本地 SQLite 中仍有残留的集合，完成彻底清理闭环
	if t.store != nil {
		indexedFiles, err := t.store.ListFileIndexByPersonAndSource(person, source)
		if err == nil {
			for _, entry := range indexedFiles {
				if !activePaths[entry.NasPath] && entry.CollectionID != "" {
					log.Printf("[fastgpt] 检测到 NAS 文件已删除，同步清理 FastGPT 知识库集合: %s (collection: %s)", entry.NasPath, entry.CollectionID)
					c := t.manager.ClientFor(datasetID)
					if err := c.DeleteCollection(entry.CollectionID); err != nil {
						log.Printf("[fastgpt] 删除 FastGPT 集合失败 %s: %v", entry.CollectionID, err)
					}
					_ = t.store.DeleteFileIndex(person, source, entry.NasPath)
				}
			}
		}
	}
	return nil
}

// pushDir 推送一个目录下的文件（跳过 .meta.json），支持多层子目录递归。
// 当 defaultCategory 为空时，根据每个文件的名称关键词自动归类。
func (t *Task) pushDir(ctx context.Context, person, source, defaultCategory, dir, datasetID string, activePaths map[string]bool) error {
	files, err := t.nas.List(dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".meta.json") {
			continue
		}
		if f.IsDir {
			if err := t.pushDir(ctx, person, source, defaultCategory, f.Path, datasetID, activePaths); err != nil {
				log.Printf("[fastgpt] 遍历子目录失败 %s: %v", f.Path, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}
		if activePaths != nil {
			activePaths[f.Path] = true
		}
		category := defaultCategory
		if category == "" {
			category = classifyFilename(f.Name)
		}
		if err := t.pushFile(ctx, person, source, category, f, datasetID); err != nil {
			log.Printf("[fastgpt] 推送失败 %s: %v", f.Path, err)
		}
	}
	return firstErr
}

// classifyFilename 根据文件名关键词识别分类。
func classifyFilename(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "合同") || strings.Contains(lower, "协议") || strings.Contains(lower, "contract"):
		return "合同"
	case strings.Contains(lower, "方案") || strings.Contains(lower, "规划") || strings.Contains(lower, "proposal"):
		return "方案"
	case strings.Contains(lower, "手册") || strings.Contains(lower, "说明书") || strings.Contains(lower, "指南"):
		return "手册"
	case strings.Contains(lower, "报告") || strings.Contains(lower, "总结") || strings.Contains(lower, "report"):
		return "报告"
	case strings.Contains(lower, "报表") || strings.Contains(lower, "数据") || strings.HasSuffix(lower, ".xlsx") || strings.HasSuffix(lower, ".csv"):
		return "数据表"
	default:
		return "其他"
	}
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

		// 展示名：默认使用真实文件名；若带有 _<短Token> 后缀或历史遗留 <msgid>_ 机器前缀，则剥离以还原最纯净的中文标题
		display := f.Name
		if category != "会话" {
			ext := path.Ext(f.Name)
			base := strings.TrimSuffix(f.Name, ext)
			if idx := strings.LastIndex(base, "_"); idx > 0 {
				suffix := base[idx+1:]
				if len(suffix) >= 6 && len(suffix) <= 12 {
					display = base[:idx] + ext
				}
			} else if idx := strings.LastIndex(f.Name, "_"); idx > 0 {
				prefix := f.Name[:idx]
				if strings.ContainsAny(prefix, "0123456789") && len(prefix) > 10 {
					display = f.Name[idx+1:]
				}
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

		// 若此前已有该文件的旧版本 Collection，在上传新版本成功后清理旧集合，防止知识库产生同名重复数据
		if existing != nil && existing.CollectionID != "" && existing.CollectionID != collectionID {
			if err := c.DeleteCollection(existing.CollectionID); err != nil {
				log.Printf("[fastgpt] 清理旧版本集合 %s 失败 (新集合: %s): %v", existing.CollectionID, collectionID, err)
			} else {
				log.Printf("[fastgpt] 已清理旧版本集合 %s (文件更新: %s)", existing.CollectionID, f.Path)
			}
		}

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
