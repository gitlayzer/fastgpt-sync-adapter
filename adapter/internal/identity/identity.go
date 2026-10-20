// Package identity 身份解析：企微 userid ↔ 飞书 user ↔ 人员姓名。
// 姓名是 NAS 第一层目录和 FastGPT 知识库命名的唯一依据（用户决策：以多维表格查出的人名为准）。
// 多维表格（Bitable）未到位前用本地 JSON 映射文件兜底；接口保持不变，届时只换数据源。
package identity

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"fastgpt-sync-adapter/adapter/internal/storage"
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

func NewLocalResolver(file string, store *storage.Store) *LocalResolver {
	return &LocalResolver{file: file, store: store}
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

// ===== Bitable 数据源（TODO：多维表格到位后实现）=====
// type BitableResolver struct { ... }  // 查飞书多维表格：企微 userid → 飞书 id + 姓名
// 预留接口与 LocalResolver 一致，届时替换 NewLocalResolver 的构造即可。
