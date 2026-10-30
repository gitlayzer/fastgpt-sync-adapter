// Package wecomcontact：外部联系人名字解析（ID → 显示名 / 文件名标签），带内存+SQLite 缓存。
package wecomcontact

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// 缓存有效期：查到的名字 7 天内不再请求接口；查不到 24 小时内不再请求。
const (
	freshOK       = 7 * 24 * time.Hour
	freshNotFound = 24 * time.Hour
)

// 状态取值（与 storage.ExternalContact.Status 一致）。
const (
	statusOK       = "ok"
	statusNotFound = "notfound"
)

// Resolver 外部联系人名字解析器。
type Resolver struct {
	store  *storage.Store
	client *Client
	now    func() time.Time

	mu  sync.Mutex
	mem map[string]storage.ExternalContact
}

// NewResolver 创建解析器；store 用于持久缓存，client 用于查接口。
func NewResolver(store *storage.Store, client *Client) *Resolver {
	return &Resolver{
		store:  store,
		client: client,
		now:    time.Now,
		mem:    map[string]storage.ExternalContact{},
	}
}

// IsExternal 是否企微外部联系人 ID（wm / wo 开头）。
func IsExternal(id string) bool {
	return strings.HasPrefix(id, "wm") || strings.HasPrefix(id, "wo")
}

// Display 消息里显示的名字（取最新名字）：
//
//	查到：type 有 corp_name → "外部:<name>@<corp_name>"，否则 "外部:<name>"
//	查不到 / 出错：         "外部:<ID后6位>"
func (r *Resolver) Display(ctx context.Context, id string) string {
	c, ok := r.lookup(ctx, id)
	if !ok || strings.TrimSpace(c.Name) == "" {
		return "外部:" + tail(id, 6)
	}
	if c.CorpName != "" {
		return fmt.Sprintf("外部:%s@%s", c.Name, c.CorpName)
	}
	return "外部:" + c.Name
}

// FileLabel 单聊文件名用的固定标签（首次查到时定下，之后名字变了也不变）：
//
//	查到：label = "<name>@<corp_name>_<ID后4位>"，无 corp_name 时 "<name>_<ID后4位>"
//	查不到 / 出错："外部联系人_<ID后6位>"（不写入 label，下次查到后再定）
func (r *Resolver) FileLabel(ctx context.Context, id string) string {
	c, ok := r.lookup(ctx, id)
	if !ok {
		return "外部联系人_" + tail(id, 6)
	}
	if c.Label != "" {
		return c.Label
	}
	// 库里有名字却算不出 label（理论上不会出现）：按规则临时算一个，不写库
	return buildLabel(c.Name, c.CorpName, id)
}

// phoneRe 中国大陆手机号：1 开头、第二位 3-9、共 11 位，前后不能紧挨着其他数字。
var phoneRe = regexp.MustCompile(`(^|[^0-9])1[3-9][0-9]{9}([^0-9]|$)`)

// stripPhone 去掉文本里的手机号，并清理去掉后多出来的首尾空白和连接符（- _ 空格 · ,）。
//
// 正则有前后定界字符（Go 的 regexp 不支持环视），一次替换会吃掉紧挨着的定界字符，
// 所以循环替换直到不再命中：每次把号码替换成它前后的定界字符（$1$2），
// 相邻的多个号码因此能在后续轮次继续被去掉。
func stripPhone(s string) string {
	for {
		next := phoneRe.ReplaceAllString(s, "$1$2")
		if next == s {
			break
		}
		s = next
	}
	return strings.Trim(s, " -_·,，")
}

// buildLabel 计算固定标签（文件名用，必须不含手机号）。
// 去掉手机号后名字为空时退化为 "外部联系人_<ID后4位>"
// （有 corp_name 时 "外部联系人@<corp_name>_<ID后4位>"）。
func buildLabel(name, corpName, id string) string {
	name = stripPhone(strings.TrimSpace(name))
	corpName = stripPhone(corpName)
	if name == "" {
		if corpName != "" {
			return fmt.Sprintf("外部联系人@%s_%s", corpName, tail(id, 4))
		}
		return "外部联系人_" + tail(id, 4)
	}
	if corpName != "" {
		return fmt.Sprintf("%s@%s_%s", name, corpName, tail(id, 4))
	}
	return fmt.Sprintf("%s_%s", name, tail(id, 4))
}

// tail 返回字符串末尾 n 个字符（不足取全部）。
func tail(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[len(rs)-n:])
}

// logID 日志里只打 ID 后 6 位，避免整串客户 ID 刷屏。
func logID(id string) string { return tail(id, 6) }

// lookup 解析外部联系人：内存 → SQLite → 接口，带回写缓存。
// 返回 ok=false 表示查不到（status=notfound 或临时错误且无旧记录）。
func (r *Resolver) lookup(ctx context.Context, id string) (storage.ExternalContact, bool) {
	if !IsExternal(id) {
		return storage.ExternalContact{}, false
	}
	// 1) 内存
	r.mu.Lock()
	mem, hasMem := r.mem[id]
	r.mu.Unlock()
	if hasMem && r.fresh(mem) {
		return mem, mem.Status == statusOK && strings.TrimSpace(mem.Name) != ""
	}

	// 2) SQLite
	cached := storage.ExternalContact{}
	hasCached := false
	if r.store != nil {
		c, ok, err := r.store.GetExternalContact(id)
		if err != nil {
			log.Printf("[wecomcontact] 读取外部联系人 %s 缓存失败: %v", logID(id), err)
		} else if ok {
			cached, hasCached = c, true
			r.remember(c)
			if r.fresh(c) {
				return c, c.Status == statusOK && strings.TrimSpace(c.Name) != ""
			}
		}
	}

	// 3) 接口
	if r.client == nil {
		return cached, hasCached && cached.Status == statusOK && strings.TrimSpace(cached.Name) != ""
	}
	contact, err := r.client.Get(ctx, id)
	if err == nil && strings.TrimSpace(contact.Name) == "" {
		err = errors.New("接口未返回联系人姓名")
	}
	switch {
	case err == nil:
		c := storage.ExternalContact{
			ExternalUserID: id,
			Name:           contact.Name,
			CorpName:       contact.CorpName,
			Label:          buildLabel(contact.Name, contact.CorpName, id),
			Status:         statusOK,
			FetchedAt:      r.now().Unix(),
		}
		// 库里 label 已有值时由 SaveExternalContact 保留旧值，回读一次拿到生效的 label
		c = r.saveAndReload(c)
		r.remember(c)
		return c, true
	case errors.Is(err, ErrNotFound):
		// 查不到：name 保留库里旧值，不清空；label 也不动（SaveExternalContact 保证）
		c := storage.ExternalContact{
			ExternalUserID: id,
			Name:           cached.Name,
			CorpName:       cached.CorpName,
			Status:         statusNotFound,
			FetchedAt:      r.now().Unix(),
		}
		if r.save(c) {
			r.remember(c)
		}
		return c, false
	default:
		// 临时错误：有旧的 ok 记录就用旧的，没有就当查不到，不写库
		log.Printf("[wecomcontact] 查询外部联系人 %s 失败: %v", logID(id), err)
		if hasCached && cached.Status == statusOK && strings.TrimSpace(cached.Name) != "" {
			return cached, true
		}
		return storage.ExternalContact{}, false
	}
}

// fresh 缓存是否还在有效期内。
func (r *Resolver) fresh(c storage.ExternalContact) bool {
	if c.FetchedAt <= 0 {
		return false
	}
	age := r.now().Sub(time.Unix(c.FetchedAt, 0))
	switch c.Status {
	case statusOK:
		return age < freshOK
	case statusNotFound:
		return age < freshNotFound
	default:
		return false
	}
}

// remember 写入内存缓存（notfound 记录不长期保留）。
func (r *Resolver) remember(c storage.ExternalContact) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mem == nil {
		r.mem = map[string]storage.ExternalContact{}
	}
	r.mem[c.ExternalUserID] = c
}

// save 写库；失败只打日志（缓存不是关键路径）。
func (r *Resolver) save(c storage.ExternalContact) bool {
	if r.store == nil {
		return true
	}
	if err := r.store.SaveExternalContact(c); err != nil {
		log.Printf("[wecomcontact] 写入外部联系人 %s 失败: %v", logID(c.ExternalUserID), err)
		return false
	}
	return true
}

// saveAndReload 写库后回读，拿到库里真正生效的记录（label 首次写入后不再变）。
// 写库失败时原样返回 c，调用方照常可用。
func (r *Resolver) saveAndReload(c storage.ExternalContact) storage.ExternalContact {
	if !r.save(c) {
		return c
	}
	saved, ok, err := r.store.GetExternalContact(c.ExternalUserID)
	if err != nil || !ok {
		return c
	}
	return saved
}
