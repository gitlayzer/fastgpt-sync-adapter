// 客户群真实群名解析（groupchat/get），带内存 + SQLite 缓存。
// 供 NAS sink 给群聊会话文件命名：查得到用真实群名，查不到再由 sink 自行推断。
package wecomcontact

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// GroupResolver 客户群真实群名解析器。
type GroupResolver struct {
	store  *storage.Store
	client *Client
	now    func() time.Time

	mu  sync.Mutex
	mem map[string]storage.GroupChat
}

// NewGroupResolver 创建解析器；store 用于持久缓存，client 用于查接口。
func NewGroupResolver(store *storage.Store, client *Client) *GroupResolver {
	return &GroupResolver{
		store:  store,
		client: client,
		now:    time.Now,
		mem:    map[string]storage.GroupChat{},
	}
}

// GroupName 客户群真实群名；查不到、群没起名或出错时返回 ""。
func (r *GroupResolver) GroupName(ctx context.Context, roomID string) string {
	if strings.TrimSpace(roomID) == "" {
		return ""
	}
	return r.groupName(ctx, roomID)
}

func (r *GroupResolver) groupName(ctx context.Context, roomID string) string {
	// 1) 内存
	r.mu.Lock()
	mem, hasMem := r.mem[roomID]
	r.mu.Unlock()
	if hasMem && r.fresh(mem) {
		return groupNameOf(mem)
	}

	// 2) SQLite
	cached := storage.GroupChat{}
	if r.store != nil {
		g, ok, err := r.store.GetGroupChat(roomID)
		if err != nil {
			log.Printf("[wecomcontact] 读取客户群 %s 缓存失败: %v", logID(roomID), err)
		} else if ok {
			cached = g
			r.remember(g)
			if r.fresh(g) {
				return groupNameOf(g)
			}
		}
	}

	// 3) 接口
	if r.client == nil {
		return groupNameOf(cached)
	}
	name, err := r.client.GroupChatName(ctx, roomID)
	switch {
	case err == nil:
		g := storage.GroupChat{
			RoomID:    roomID,
			Name:      name,
			Status:    statusOK,
			FetchedAt: r.now().Unix(),
		}
		if r.save(g) {
			r.remember(g)
		}
		return stripPhone(strings.TrimSpace(name))
	case errors.Is(err, ErrGroupNotFound):
		// 查不到（无权限/非客户群）：正常结果，缓存 24 小时
		g := storage.GroupChat{
			RoomID:    roomID,
			Status:    statusNotFound,
			FetchedAt: r.now().Unix(),
		}
		if r.save(g) {
			r.remember(g)
		}
		return ""
	default:
		// 临时错误：有旧的 ok 记录就用旧名字，没有就当查不到，不写库
		log.Printf("[wecomcontact] 查询客户群 %s 失败: %v", logID(roomID), err)
		return groupNameOf(cached)
	}
}

// groupNameOf 缓存记录里可用的群名（只有 ok 且非空才算有名字）。
func groupNameOf(g storage.GroupChat) string {
	if g.Status != statusOK {
		return ""
	}
	return stripPhone(strings.TrimSpace(g.Name))
}

// fresh 缓存是否还在有效期内（查到的名字 7 天、查不到 24 小时）。
func (r *GroupResolver) fresh(g storage.GroupChat) bool {
	if g.FetchedAt <= 0 {
		return false
	}
	age := r.now().Sub(time.Unix(g.FetchedAt, 0))
	switch g.Status {
	case statusOK:
		return age < freshOK
	case statusNotFound:
		return age < freshNotFound
	default:
		return false
	}
}

// remember 写入内存缓存。
func (r *GroupResolver) remember(g storage.GroupChat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mem == nil {
		r.mem = map[string]storage.GroupChat{}
	}
	r.mem[g.RoomID] = g
}

// save 写库；失败只打日志（缓存不是关键路径）。
func (r *GroupResolver) save(g storage.GroupChat) bool {
	if r.store == nil {
		return true
	}
	if err := r.store.SaveGroupChat(g); err != nil {
		log.Printf("[wecomcontact] 写入客户群 %s 失败: %v", logID(g.RoomID), err)
		return false
	}
	return true
}
