// Package nasstore serve 模式的 NAS 后端：通过群晖 WebAPI 把 NAS 目录树暴露成
// FastGPT API 文件库（previewUrl 指向 NAS 文件字节流），不经任何本地挂载。
// R6 冻结：同目录下存在 <文件>.meta.json 且 frozen=true 时，该文件不出现在列表/搜索结果。
package nasstore

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/store"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
)

// Backend 基于群晖 WebAPI 的文件库后端。
type Backend struct {
	client *synology.Client
	root   string // NAS 上的根路径，如 /FastGPT-test
}

func New(client *synology.Client, root string) *Backend {
	return &Backend{client: client, root: strings.TrimRight(root, "/")}
}

// rootID 根节点 id。
func (b *Backend) rootID() string { return store.HashID("/") }

// relOf NAS 绝对路径 → 相对 root 的路径（用于稳定 id）。
func (b *Backend) relOf(p string) string {
	rel := strings.TrimPrefix(p, b.root)
	if rel == "" {
		return "/"
	}
	return rel
}

// nodeFrom 把 FileEntry 转成 store.Node（id = sha1(相对路径)，路径不变则 id 稳定）。
func (b *Backend) nodeFrom(e synology.FileEntry) *store.Node {
	rel := b.relOf(e.Path)
	typ := "file"
	if e.IsDir {
		typ = "folder"
	}
	var pid *string
	if rel != "/" {
		p := store.HashID(path.Dir(rel))
		pid = &p
	}
	return store.NewExternalNode(store.HashID(rel), pid, e.Name, typ, e.MTime, e.Path)
}

// Get 按 id 取节点：id 是 sha1(相对路径)，NAS 无法按 id 反查，只能逐层遍历匹配。
func (b *Backend) Get(id string) (*store.Node, error) {
	if id == "" || id == b.rootID() {
		return store.NewExternalNode(b.rootID(), nil, path.Base(b.root), "folder", time.Time{}, b.root), nil
	}
	found, err := b.findByID(b.root, id, 8)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("node not found: %s", id)
	}
	return found, nil
}

// findByID 深度受限的递归查找（NAS 每次 List 都是一次 HTTP，深度封顶防失控）。
func (b *Backend) findByID(dir, id string, depth int) (*store.Node, error) {
	if depth <= 0 {
		return nil, nil
	}
	entries, err := b.client.List(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name, ".meta.json") {
			continue
		}
		if store.HashID(b.relOf(e.Path)) == id {
			return b.nodeFrom(e), nil
		}
		if e.IsDir {
			if n, err := b.findByID(e.Path, id, depth-1); err == nil && n != nil {
				return n, nil
			}
		}
	}
	return nil, nil
}

// Children 列子节点。
// 如果是根目录（parentID == "" 或 rootID），返回员工目录列表（folder）。
// 如果是员工目录（或员工下的子目录），在 API 层做虚拟平铺展开：递归收集该员工名下的所有真实文件并直接返回（file），
// 彻底解决 FastGPT 无法联级勾选下载子目录文件的限制，而在 NAS 物理层依然完整保持规范的日期与会话目录结构。
func (b *Backend) Children(parentID string) ([]*store.Node, error) {
	dir := b.root
	isRoot := parentID == "" || parentID == b.rootID()
	if !isRoot {
		parent, err := b.Get(parentID)
		if err != nil {
			return nil, err
		}
		if parent.Type != "folder" {
			return nil, fmt.Errorf("parent is not a folder: %s", parentID)
		}
		dir = parent.AbsPath()
	}

	// 根目录：只返回第一层（员工目录 + 共享文件夹）
	if isRoot {
		entries, err := b.client.List(dir)
		if err != nil {
			return nil, err
		}
		frozen := b.frozenSet(entries)
		var out []*store.Node
		for _, e := range entries {
			if strings.HasSuffix(e.Name, ".meta.json") || frozen[e.Name] {
				continue
			}
			out = append(out, b.nodeFrom(e))
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Type != out[j].Type {
				return out[i].Type == "folder"
			}
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		})
		return out, nil
	}

	// 员工目录或更深层：API 层虚拟递归平铺，把深层的所有真实文件直接展现给 FastGPT，支持一键全选导入
	out, err := b.collectFilesRecursively(dir, parentID, "", 6)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// collectFilesRecursively 递归收集 dir 下的所有具体文件（隐藏 .meta.json 与被冻结文件）。
func (b *Backend) collectFilesRecursively(dir string, parentID string, basePrefix string, depth int) ([]*store.Node, error) {
	if depth <= 0 {
		return nil, nil
	}
	entries, err := b.client.List(dir)
	if err != nil {
		return nil, err
	}
	frozen := b.frozenSet(entries)
	var out []*store.Node
	for _, e := range entries {
		if strings.HasSuffix(e.Name, ".meta.json") || frozen[e.Name] {
			continue
		}
		if e.IsDir {
			subPrefix := e.Name
			if basePrefix != "" {
				subPrefix = basePrefix + "/" + e.Name
			}
			children, err := b.collectFilesRecursively(e.Path, parentID, subPrefix, depth-1)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		} else {
			displayName := e.Name
			if basePrefix != "" {
				displayName = basePrefix + "/" + e.Name
			}
			rel := b.relOf(e.Path)
			pid := parentID
			node := store.NewExternalNode(store.HashID(rel), &pid, displayName, "file", e.MTime, e.Path)
			out = append(out, node)
		}
	}
	return out, nil
}

// Search 全树按名称模糊匹配（深度 8；隐藏 meta 与冻结文件）。
func (b *Backend) Search(key string) ([]*store.Node, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return nil, nil
	}
	var out []*store.Node
	b.search(b.root, key, 8, &out)
	return out, nil
}

func (b *Backend) search(dir, key string, depth int, out *[]*store.Node) {
	if depth <= 0 {
		return
	}
	entries, err := b.client.List(dir)
	if err != nil {
		return
	}
	frozen := b.frozenSet(entries)
	for _, e := range entries {
		if strings.HasSuffix(e.Name, ".meta.json") || frozen[e.Name] {
			continue
		}
		if strings.Contains(strings.ToLower(e.Name), key) {
			*out = append(*out, b.nodeFrom(e))
		}
		if e.IsDir {
			b.search(e.Path, key, depth-1, out)
		}
	}
}

// frozenSet 读当前目录的 .meta.json，返回被冻结的文件名集合（R6）。
func (b *Backend) frozenSet(entries []synology.FileEntry) map[string]bool {
	frozen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir || !strings.HasSuffix(e.Name, ".meta.json") {
			continue
		}
		rc, err := b.client.Download(e.Path)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		var m struct {
			Frozen bool `json:"frozen"`
		}
		if json.Unmarshal(data, &m) == nil && m.Frozen {
			frozen[strings.TrimSuffix(e.Name, ".meta.json")] = true
		}
	}
	return frozen
}

// Open 下载文件字节流（previewUrl 的最终数据源）。
func (b *Backend) Open(n *store.Node) (io.ReadCloser, error) {
	return b.client.Download(n.AbsPath())
}

// OpenPath 按 NAS 绝对路径直接下载文件字节流（增加严格路径边界防跨目录校验）。
func (b *Backend) OpenPath(filePath string) (io.ReadCloser, error) {
	clean := path.Clean("/" + strings.TrimPrefix(filePath, "/"))
	rootPrefix := b.root + "/"
	inRoot := func(p string) bool {
		return b.root == "" || p == b.root || strings.HasPrefix(p, rootPrefix)
	}
	if !inRoot(clean) {
		clean = path.Join(b.root, clean)
	}
	if !inRoot(clean) || clean == b.root {
		return nil, fmt.Errorf("非法访问路径: %s", filePath)
	}
	return b.client.Download(clean)
}
