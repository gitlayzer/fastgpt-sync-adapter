package store

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Node 表示目录树中的一个文件或文件夹。
type Node struct {
	ID         string    `json:"id"`
	ParentID   *string   `json:"parentId"`
	Name       string    `json:"name"`
	Type       string    `json:"type"` // "file" 或 "folder"
	UpdateTime time.Time `json:"updateTime"`
	CreateTime time.Time `json:"createTime"`
	HasChild   bool      `json:"hasChild,omitempty"`

	relPath string // 相对 root 的路径，内部使用
	absPath string // 真实磁盘路径，内部使用
}

// Store 把本地目录映射成稳定的 id 树。
type Store struct {
	root string // 绝对路径的根目录
}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Store{root: abs}, nil
}

// RootID 返回根目录的固定 id。
func (s *Store) RootID() string {
	return s.idFor(".")
}

// idFor 用相对路径生成稳定 id（同一路径恒定）。
func (s *Store) idFor(rel string) string {
	h := sha1.Sum([]byte(rel))
	return hex.EncodeToString(h[:])
}

// Get 按 id 取节点；id 为空或根 id 时返回根节点。
func (s *Store) Get(id string) (*Node, error) {
	if id == "" || id == s.RootID() {
		return s.buildNode(".")
	}
	var found *Node
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != nil {
			return err
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return nil
		}
		if s.idFor(rel) == id {
			n, err := s.buildNode(rel)
			if err == nil {
				found = n
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("node not found: %s", id)
	}
	return found, nil
}

// Children 返回某个父节点下的直接子节点。parentID 为空或根 id 表示根。
func (s *Store) Children(parentID string) ([]*Node, error) {
	parentRel := "."
	if parentID != "" && parentID != s.RootID() {
		parent, err := s.Get(parentID)
		if err != nil {
			return nil, err
		}
		if parent.Type != "folder" {
			return nil, fmt.Errorf("parent is not a folder: %s", parentID)
		}
		parentRel = parent.relPath
	}
	dir := filepath.Join(s.root, parentRel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Node
	for _, e := range entries {
		rel := filepath.Join(parentRel, e.Name())
		n, err := s.buildNode(rel)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	// 稳定排序：文件夹在前，再按名称
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "folder"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Search 在整个树内按名称模糊匹配（忽略大小写）。
func (s *Store) Search(key string) ([]*Node, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return nil, nil
	}
	var out []*Node
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil || rel == "." {
			return nil
		}
		if strings.Contains(strings.ToLower(d.Name()), key) {
			n, err := s.buildNode(rel)
			if err == nil {
				out = append(out, n)
			}
		}
		return nil
	})
	return out, err
}

// buildNode 由相对路径构造节点。
func (s *Store) buildNode(rel string) (*Node, error) {
	abs := filepath.Join(s.root, rel)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	n := &Node{
		ID:         s.idFor(rel),
		Name:       info.Name(),
		UpdateTime: info.ModTime(),
		CreateTime: info.ModTime(), // 多数文件系统拿不到 btime，用 mtime 兜底
		relPath:    rel,
		absPath:    abs,
	}
	if rel != "." {
		pid := s.idFor(filepath.Dir(rel))
		n.ParentID = &pid
	}
	if info.IsDir() {
		n.Type = "folder"
		entries, _ := os.ReadDir(abs)
		n.HasChild = len(entries) > 0
	} else {
		n.Type = "file"
		n.HasChild = false
	}
	return n, nil
}

// AbsPath 返回节点真实磁盘路径（仅服务端内部使用）。
func (n *Node) AbsPath() string { return n.absPath }

// NewExternalNode 供非本地后端（如 NAS）构造节点：backendPath 是后端的定位键（NAS 路径）。
func NewExternalNode(id string, parentID *string, name, typ string, mtime time.Time, backendPath string) *Node {
	return &Node{
		ID:         id,
		ParentID:   parentID,
		Name:       name,
		Type:       typ,
		UpdateTime: mtime,
		CreateTime: mtime,
		absPath:    backendPath,
	}
}

// HashID 用任意路径字符串生成稳定 id（与本地 Store 同一套规则，供外部后端复用）。
func HashID(p string) string {
	h := sha1.Sum([]byte(p))
	return hex.EncodeToString(h[:])
}

// Open 打开本地节点的文件流（本地后端的下载实现）。
func (s *Store) Open(n *Node) (io.ReadCloser, error) {
	return os.Open(n.absPath)
}
