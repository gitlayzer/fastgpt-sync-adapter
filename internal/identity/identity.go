// Package identity 身份解析：企微 userid ↔ 飞书 user ↔ 人员姓名。
// 姓名是 NAS 第一层目录和 FastGPT 知识库命名的唯一依据（用户决策：以多维表格查出的人名为准）。
// 多维表格（Bitable）未到位前用本地 JSON 映射文件兜底；接口保持不变，届时只换数据源。
package identity

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// Resolver 身份解析接口。
type Resolver interface {
	// PersonName 返回规范人名；未命中映射时兜底返回 externalUserID 本身（不阻塞归档）。
	PersonName(source, externalUserID string) string
	// ResolveWeCom 按企微 userid 解析完整身份。
	ResolveWeCom(wecomUserID string) (*storage.Identity, error)
	// ResolveFeishu 按飞书 user_id 解析完整身份。
	ResolveFeishu(feishuUserID string) (*storage.Identity, error)
}

// Mapping 一条身份映射（本地文件格式）。
type Mapping struct {
	WeComUserID  string `json:"wecom_user_id"`
	FeishuUserID string `json:"feishu_user_id"`
	PersonName   string `json:"person_name"`
}

// LocalResolver 本地 JSON 文件兜底实现。文件变更（mtime 变化）自动重载。
type LocalResolver struct {
	file  string
	store *storage.Store

	mu       sync.Mutex
	loadedAt time.Time
	modTime  time.Time
}

// NewLocalResolver 构造本地兜底解析器。
// 构造时立即加载一次：其他模块（如飞书共享文件夹发现）会直接读 identities 表，不能等到第一次解析调用。
func NewLocalResolver(file string, store *storage.Store) *LocalResolver {
	r := &LocalResolver{file: file, store: store}
	r.maybeReload()
	return r
}

// maybeReload 文件存在且 mtime 变化时重载进 SQLite 缓存。
func (r *LocalResolver) maybeReload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Stat(r.file)
	if err != nil {
		return // 文件不存在：保持已有缓存（或空）
	}
	if !info.ModTime().After(r.modTime) {
		return
	}
	data, err := os.ReadFile(r.file)
	if err != nil {
		log.Printf("[identity] 读取映射文件失败: %v", err)
		return
	}
	var list []Mapping
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("[identity] 解析映射文件失败: %v", err)
		return
	}
	n := 0
	for _, m := range list {
		if m.WeComUserID == "" {
			continue
		}
		if err := r.store.UpsertIdentity(&storage.Identity{
			WeComUserID:  m.WeComUserID,
			FeishuUserID: m.FeishuUserID,
			PersonName:   m.PersonName,
		}); err == nil {
			n++
		}
	}
	r.modTime = info.ModTime()
	r.loadedAt = time.Now()
	log.Printf("[identity] 本地映射已加载 %d 条（%s）", n, r.file)
}

func (r *LocalResolver) ResolveWeCom(wecomUserID string) (*storage.Identity, error) {
	r.maybeReload()
	return r.store.GetIdentityByWeCom(wecomUserID)
}

func (r *LocalResolver) ResolveFeishu(feishuUserID string) (*storage.Identity, error) {
	r.maybeReload()
	return r.store.GetIdentityByFeishu(feishuUserID)
}

// PersonName 未命中时兜底返回 externalUserID（归档不阻塞，后续补映射后重跑即可归位）。
func (r *LocalResolver) PersonName(source, externalUserID string) string {
	var id *storage.Identity
	var err error
	switch source {
	case "wecom":
		id, err = r.ResolveWeCom(externalUserID)
	case "feishu":
		id, err = r.ResolveFeishu(externalUserID)
	}
	if err == nil && id != nil && id.PersonName != "" {
		return id.PersonName
	}
	return externalUserID
}

// ===== Bitable 数据源 =====

// BitableSource 多维表格读取能力（由 feishu.Client 实现，接口化便于测试）。
type BitableSource interface {
	TenantAccessToken() (string, error)
	SearchRecords(token, appToken, tableID string) ([]feishuapi.BitableRecord, error)
}

// BitableConfig 多维表格身份映射配置。
type BitableConfig struct {
	AppToken string        // 多维表格 app_token
	TableID  string        // 数据表 id
	Refresh  time.Duration // 同步间隔（0 表示默认 1 小时）
	// 字段名（可配，避免表格改列名后代码失效）
	FieldWeComID    string // 企微 userid 列
	FieldWeComName  string // 企微姓名列（含英文名前缀）
	FieldFeishuUser string // 飞书人员列（用户对象）
	FieldOpenID     string // 飞书 open_id 列（冗余列，人员列为空时兜底）
}

// ApplyDefaults 填默认字段名（与现网表格一致）。
func (c *BitableConfig) ApplyDefaults() {
	if c.Refresh <= 0 {
		c.Refresh = time.Hour
	}
	if c.FieldWeComID == "" {
		c.FieldWeComID = "企业微信UserID"
	}
	if c.FieldWeComName == "" {
		c.FieldWeComName = "企业微信姓名"
	}
	if c.FieldFeishuUser == "" {
		c.FieldFeishuUser = "飞书人员"
	}
	if c.FieldOpenID == "" {
		c.FieldOpenID = "飞书open_id"
	}
}

// BitableResolver 从飞书多维表格同步身份映射到 SQLite。
// 表格是权威数据源，本地只做缓存：查询走 SQLite，避免每条消息都打飞书 API。
type BitableResolver struct {
	cfg   BitableConfig
	src   BitableSource
	store *storage.Store

	mu       sync.Mutex
	lastSync time.Time
	lastErr  error
	rowCount int
}

func NewBitableResolver(cfg BitableConfig, src BitableSource, store *storage.Store) *BitableResolver {
	cfg.ApplyDefaults()
	return &BitableResolver{cfg: cfg, src: src, store: store}
}

// syncIfStale 到达间隔就拉一次表格。失败保留旧缓存，不阻塞归档。
func (r *BitableResolver) syncIfStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastSync) < r.cfg.Refresh {
		return
	}
	r.lastSync = time.Now() // 失败也更新时间，避免每轮都重试打爆接口

	n, err := r.syncLocked()
	if err != nil {
		r.lastErr = err
		log.Printf("[identity] 多维表格同步失败（沿用旧缓存 %d 条）: %v", r.rowCount, err)
		return
	}
	r.lastErr = nil
	r.rowCount = n
	log.Printf("[identity] 多维表格同步完成：%d 条映射", n)
}

func (r *BitableResolver) syncLocked() (int, error) {
	token, err := r.src.TenantAccessToken()
	if err != nil {
		return 0, fmt.Errorf("获取 tenant token: %w", err)
	}
	recs, err := r.src.SearchRecords(token, r.cfg.AppToken, r.cfg.TableID)
	if err != nil {
		return 0, fmt.Errorf("查询记录: %w", err)
	}
	n := 0
	for _, rec := range recs {
		f := rec.Fields
		wecomID := strings.TrimSpace(feishuapi.FieldText(f[r.cfg.FieldWeComID]))
		name := strings.TrimSpace(feishuapi.FieldText(f[r.cfg.FieldWeComName]))
		// 优先使用显式指定的飞书 open_id 文本字段（适配读表格与文档同步为不同应用的情况）
		openID := strings.TrimSpace(feishuapi.FieldText(f[r.cfg.FieldOpenID]))
		if openID == "" {
			openID, _ = feishuapi.FieldUser(f[r.cfg.FieldFeishuUser])
		}
		if wecomID == "" && openID == "" {
			continue
		}
		if wecomID == "" {
			wecomID = "feishu:" + openID
		}
		if name == "" {
			if !strings.HasPrefix(wecomID, "feishu:") {
				name = wecomID // 姓名为空时退回 userid，至少不产生空目录
			} else {
				name = openID
			}
		}
		if err := r.store.UpsertIdentity(&storage.Identity{
			WeComUserID:  wecomID,
			FeishuUserID: openID,
			PersonName:   name,
		}); err == nil {
			n++
		}
	}
	if n == 0 && len(recs) > 0 {
		return 0, fmt.Errorf("解析出 0 条映射（检查字段名配置是否正确）")
	}
	return n, nil
}

// Sync 立即同步一次（启动时调用，避免首轮用空缓存）。
func (r *BitableResolver) Sync() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.syncLocked()
	if err != nil {
		return err
	}
	r.lastSync = time.Now()
	r.rowCount = n
	log.Printf("[identity] 多维表格同步完成：%d 条映射", n)
	return nil
}

func (r *BitableResolver) ResolveWeCom(wecomUserID string) (*storage.Identity, error) {
	r.syncIfStale()
	return r.store.GetIdentityByWeCom(wecomUserID)
}

func (r *BitableResolver) ResolveFeishu(feishuUserID string) (*storage.Identity, error) {
	r.syncIfStale()
	return r.store.GetIdentityByFeishu(feishuUserID)
}

// PersonName 未命中时兜底返回 externalUserID。
func (r *BitableResolver) PersonName(source, externalUserID string) string {
	var id *storage.Identity
	var err error
	switch source {
	case "wecom":
		id, err = r.ResolveWeCom(externalUserID)
	case "feishu":
		id, err = r.ResolveFeishu(externalUserID)
	}
	if err == nil && id != nil && id.PersonName != "" {
		return id.PersonName
	}
	return externalUserID
}
