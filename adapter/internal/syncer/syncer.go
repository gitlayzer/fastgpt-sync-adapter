package syncer

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fastgpt-sync-adapter/adapter/internal/uploader"
)

// 支持上传的文件扩展名（FastGPT 可解析的）
var supportedExts = map[string]bool{
	".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".txt": true, ".md": true, ".markdown": true,
	".csv": true, ".html": true, ".htm": true,
}

// stateEntry 记录已上传文件的状态
type stateEntry struct {
	RelPath      string `json:"relPath"`
	Hash         string `json:"hash"`       // 内容 sha1
	CollectionID string `json:"collectionId"`
}

// Syncer 负责扫描目录并增量上传。
type Syncer struct {
	Root      string
	Client    *uploader.Client
	StatePath string

	state map[string]*stateEntry // key: relPath
}

func New(root string, client *uploader.Client, statePath string) *Syncer {
	return &Syncer{
		Root:      root,
		Client:    client,
		StatePath: statePath,
		state:     map[string]*stateEntry{},
	}
}

// fileHash 计算文件内容 sha1。
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadState 读取状态文件。
func (s *Syncer) loadState() error {
	data, err := os.ReadFile(s.StatePath)
	if os.IsNotExist(err) {
		s.state = map[string]*stateEntry{}
		return nil
	}
	if err != nil {
		return err
	}
	var list []*stateEntry
	if err := json.Unmarshal(data, &list); err != nil {
		// 状态文件损坏则重置
		s.state = map[string]*stateEntry{}
		return nil
	}
	s.state = map[string]*stateEntry{}
	for _, e := range list {
		s.state[e.RelPath] = e
	}
	return nil
}

// saveState 持久化状态文件。
func (s *Syncer) saveState() error {
	var list []*stateEntry
	for _, e := range s.state {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].RelPath < list[j].RelPath })
	data, _ := json.MarshalIndent(list, "", "  ")
	return os.WriteFile(s.StatePath, data, 0644)
}

// scanLocal 扫描本地目录，返回 relPath -> (hash, name)。
func (s *Syncer) scanLocal() (map[string]struct {
	Hash string
	Name string
}, error) {
	result := map[string]struct {
		Hash string
		Name string
	}{}
	err := filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过无权限的项
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !supportedExts[ext] {
			return nil
		}
		rel, err := filepath.Rel(s.Root, path)
		if err != nil {
			return nil
		}
		h, err := fileHash(path)
		if err != nil {
			log.Printf("  [跳过] %s 计算哈希失败: %v", rel, err)
			return nil
		}
		result[rel] = struct {
			Hash string
			Name string
		}{Hash: h, Name: d.Name()}
		return nil
	})
	return result, err
}

// Run 执行一轮同步：新增/变更上传，删除远端多余文件。
func (s *Syncer) Run() error {
	if err := s.loadState(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	local, err := s.scanLocal()
	if err != nil {
		return fmt.Errorf("scan local: %w", err)
	}

	// 拉取远端现有文件
	remote, err := s.Client.ListCollections()
	if err != nil {
		return fmt.Errorf("list remote: %w", err)
	}
	remoteByName := map[string]uploader.Collection{}
	for _, c := range remote {
		remoteByName[c.Name] = c
	}

	// 1) 本地有、需要上传的（新增 或 内容变更）
	for rel, meta := range local {
		prev, seen := s.state[rel]
		if seen && prev.Hash == meta.Hash {
			continue // 无变化
		}
		// 内容变更：先删远端旧的（若存在）
		if existing, ok := remoteByName[meta.Name]; ok {
			log.Printf("  [更新] %s 内容变化，先删除远端旧版本", rel)
			if err := s.Client.DeleteCollection(existing.ID); err != nil {
				log.Printf("    删除旧版本失败（继续上传）: %v", err)
			}
		}
		abs := filepath.Join(s.Root, rel)
		log.Printf("  [上传] %s", rel)
		cid, err := s.Client.UploadFile(abs, meta.Name)
		if err != nil {
			log.Printf("    上传失败: %v", err)
			continue
		}
		// 上传到 S3 后文件名被随机化，二次调用修正为正常文件名
		if err := s.Client.RenameCollection(cid, meta.Name); err != nil {
			log.Printf("    重命名失败（不影响内容）: %v", err)
		} else {
			log.Printf("    已重命名为 %s", meta.Name)
		}
		s.state[rel] = &stateEntry{RelPath: rel, Hash: meta.Hash, CollectionID: cid}
		log.Printf("    成功 collectionId=%s", cid)
	}

	// 2) 本地已删除、远端仍存在的 → 删除远端（可选，默认开启）
	localNames := map[string]bool{}
	for _, meta := range local {
		localNames[meta.Name] = true
	}
	for rel, prev := range s.state {
		if _, stillLocal := local[rel]; !stillLocal {
			// 本地删除了这个文件
			name := filepath.Base(rel)
			if existing, ok := remoteByName[name]; ok {
				log.Printf("  [删除] 本地已移除 %s，删除远端", rel)
				if err := s.Client.DeleteCollection(existing.ID); err != nil {
					log.Printf("    删除失败: %v", err)
					continue
				}
			}
			delete(s.state, rel)
			_ = prev
		}
	}

	if err := s.saveState(); err != nil {
		log.Printf("  [警告] 保存状态失败: %v", err)
	}
	return nil
}
