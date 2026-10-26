package fastgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
	"path/filepath"
)

// fakeNAS 内存版 NAS。
type fakeNAS struct {
	dirs  map[string][]synology.FileEntry
	files map[string][]byte
}

func newFakeNAS() *fakeNAS {
	return &fakeNAS{dirs: map[string][]synology.FileEntry{}, files: map[string][]byte{}}
}

func (f *fakeNAS) addDir(parent, name string) {
	p := path.Join(parent, name)
	f.dirs[parent] = append(f.dirs[parent], synology.FileEntry{Name: name, Path: p, IsDir: true})
	if _, ok := f.dirs[p]; !ok {
		f.dirs[p] = nil
	}
}

func (f *fakeNAS) addFile(dir, name string, data []byte) {
	p := path.Join(dir, name)
	f.dirs[dir] = append(f.dirs[dir], synology.FileEntry{Name: name, Path: p, Size: int64(len(data)), MTime: time.Unix(1760000000, 0)})
	f.files[p] = data
}

func (f *fakeNAS) List(dir string) ([]synology.FileEntry, error) {
	return f.dirs[dir], nil
}

func (f *fakeNAS) Download(p string) (io.ReadCloser, error) {
	data, ok := f.files[p]
	if !ok {
		return nil, fmt.Errorf("不存在: %s", p)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// fakeFastGPT 模拟 FastGPT 服务端，记录调用。
type fakeFastGPT struct {
	mu         sync.Mutex
	datasets   map[string]string // name -> id
	uploads    []string          // 收到的文件名
	collections map[string]string // collectionID -> name
	nextID     int
}

func newFakeFastGPT(t *testing.T) (*fakeFastGPT, string) {
	fg := &fakeFastGPT{datasets: map[string]string{}, collections: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fg.mu.Lock()
		defer fg.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/api/core/dataset/list":
			var req struct{ SearchKey string `json:"searchKey"` }
			json.Unmarshal(body, &req)
			var out []map[string]string
			for name, id := range fg.datasets {
				if req.SearchKey == "" || strings.Contains(name, req.SearchKey) {
					out = append(out, map[string]string{"_id": id, "name": name, "createTime": "2026-10-21T00:00:00Z"})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": out})
		case r.URL.Path == "/api/core/dataset/create":
			var req struct{ Name string `json:"name"` }
			json.Unmarshal(body, &req)
			fg.nextID++
			id := fmt.Sprintf("ds%d", fg.nextID)
			fg.datasets[req.Name] = id
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": id})
			case r.URL.Path == "/api/core/dataset/collection/create/localFile":
				// multipart：取 file 字段文件名
				_, params, _ := parseMultipart(r, body)
				fg.nextID++
				cid := fmt.Sprintf("col%d", fg.nextID)
				name := params["display_name"]
				if name == "" {
					name = params["filename"]
				}
				fg.uploads = append(fg.uploads, name)
				fg.collections[cid] = name
				json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]string{"collectionId": cid}})
			case r.URL.Path == "/api/core/dataset/collection/update":
				var req struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				json.Unmarshal(body, &req)
				if req.Name != "" && req.ID != "" {
					fg.collections[req.ID] = req.Name
				}
				json.NewEncoder(w).Encode(map[string]any{"code": 200})
		case r.URL.Path == "/api/core/dataset/collection/delete":
			var req struct{ CollectionIds []string `json:"collectionIds"` }
			json.Unmarshal(body, &req)
			for _, id := range req.CollectionIds {
				delete(fg.collections, id)
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"code": 404, "message": "not found: " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return fg, srv.URL
}

func parseMultipart(r *http.Request, body []byte) (string, map[string]string, error) {
	params := map[string]string{}
	ct := r.Header.Get("Content-Type")
	_, mediaParams, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", params, err
	}
	boundary, ok := mediaParams["boundary"]
	if !ok {
		return "", params, fmt.Errorf("no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "file" {
			cd := part.Header.Get("Content-Disposition")
			if idx := strings.Index(cd, "filename*=UTF-8''"); idx != -1 {
				raw := cd[idx+len("filename*=UTF-8''"):]
				if end := strings.Index(raw, ";"); end != -1 {
					raw = raw[:end]
				}
				if unescaped, err := url.PathUnescape(strings.Trim(raw, `"`)); err == nil && unescaped != "" {
					params["filename"] = unescaped
				}
			}
			if params["filename"] == "" {
				params["filename"] = part.FileName()
			}
		}
		if part.FormName() == "data" {
			var d struct {
				Name string `json:"name"`
			}
			b, _ := io.ReadAll(part)
			_ = json.Unmarshal(b, &d)
			if d.Name != "" {
				params["display_name"] = d.Name
			}
		}
		io.Copy(io.Discard, part)
	}
	return "", params, nil
}

func newPushTask(t *testing.T, baseURL string, fs *fakeNAS, st *storage.Store) *Task {
	return New(Config{BaseURL: baseURL, APIKey: "k", Interval: time.Minute}, fs, "/FastGPT-test", st)
}

func TestPushNewFilesAndIdempotent(t *testing.T) {
	fs := newFakeNAS()
	root := "/FastGPT-test"
	fs.dirs[root] = nil
	fs.addDir(root, "张三")
	fs.addDir(root+"/张三", "企微")
	fs.addDir(root+"/张三/企微", "conversations")
	fs.addDir(root+"/张三/企微", "合同")
	fs.addDir(root+"/张三/企微", "messages") // 应被跳过
	fs.addFile(root+"/张三/企微/conversations", "与李四的单聊.md", []byte("# 会话\n- 你好"))
	fs.addFile(root+"/张三/企微/合同", "m1_采购合同.docx", []byte("合同内容"))
	fs.addFile(root+"/张三/企微/合同", "m1_采购合同.docx.meta.json", []byte(`{"frozen":false}`))
	fs.addFile(root+"/张三/企微/messages", "m1.json", []byte(`{}`))

	fg, baseURL := newFakeFastGPT(t)
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	task := newPushTask(t, baseURL, fs, st)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 应建库"张三"，推 2 个文件（md + docx），跳过 messages/m1.json
	if _, ok := fg.datasets["张三"]; !ok {
		t.Fatalf("应创建知识库 张三, 现有: %v", fg.datasets)
	}
	if len(fg.uploads) != 2 {
		t.Fatalf("应推 2 个文件, 实际 %d: %v", len(fg.uploads), fg.uploads)
	}

	// 台账应记录
	fi, err := st.GetFileIndex("张三", "wecom", root+"/张三/企微/合同/m1_采购合同.docx")
	if err != nil || fi.CollectionID == "" {
		t.Fatalf("台账未记录: %v %+v", err, fi)
	}
	if fi.Category != "合同" {
		t.Fatalf("分类应为 合同, 得到 %s", fi.Category)
	}

	// 第二次 Run：内容未变 → 不再上传
	before := len(fg.uploads)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run2: %v", err)
	}
	if len(fg.uploads) != before {
		t.Fatalf("幂等失败, 重复上传: %v", fg.uploads)
	}
	// 不应重复建库
	if fg.datasets["张三"] == "" {
		t.Fatal("知识库丢了")
	}
}

func TestPushFrozenSkipped(t *testing.T) {
	fs := newFakeNAS()
	root := "/FastGPT-test"
	fs.dirs[root] = nil
	fs.addDir(root, "李四")
	fs.addDir(root+"/李四", "企微")
	fs.addDir(root+"/李四/企微", "合同")
	fs.addFile(root+"/李四/企微/合同", "m9_冻结合同.docx", []byte("x"))
	fs.addFile(root+"/李四/企微/合同", "m9_冻结合同.docx.meta.json", []byte(`{"frozen":true}`))

	fg, baseURL := newFakeFastGPT(t)
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	task := newPushTask(t, baseURL, fs, st)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fg.uploads) != 0 {
		t.Fatalf("冻结文件不应推送: %v", fg.uploads)
	}
}

func TestFrozenDeletesPushed(t *testing.T) {
	fs := newFakeNAS()
	root := "/FastGPT-test"
	fs.dirs[root] = nil
	fs.addDir(root, "李四")
	fs.addDir(root+"/李四", "企微")
	fs.addDir(root+"/李四/企微", "合同")
	metaPath := root + "/李四/企微/合同/m9_冻结合同.docx.meta.json"
	fs.addFile(root+"/李四/企微/合同", "m9_冻结合同.docx", []byte("x"))
	fs.addFile(root+"/李四/企微/合同", "m9_冻结合同.docx.meta.json", []byte(`{"frozen":false}`))

	fg, baseURL := newFakeFastGPT(t)
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	task := newPushTask(t, baseURL, fs, st)
	task.Run(context.Background())
	if len(fg.collections) != 1 {
		t.Fatalf("应先推 1 个, 实际 %d", len(fg.collections))
	}

	// 冻结后再跑 → 应从 FastGPT 删除
	fs.files[metaPath] = []byte(`{"frozen":true}`)
		task.Run(context.Background())
		if len(fg.collections) != 0 {
			t.Fatalf("冻结后应删除, 还剩 %d", len(fg.collections))
		}
	}

func TestPushFileUpdateDeletesOldCollection(t *testing.T) {
	fs := newFakeNAS()
	root := "/FastGPT-test"
	fs.dirs[root] = nil
	fs.addDir(root, "张三")
	fs.addDir(root+"/张三", "企微")
	fs.addDir(root+"/张三/企微", "conversations")
	convPath := root + "/张三/企微/conversations/与李四的单聊.md"
	fs.addFile(root+"/张三/企微/conversations", "与李四的单聊.md", []byte("# 会话\n- 第一条消息"))

	fg, baseURL := newFakeFastGPT(t)
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	task := newPushTask(t, baseURL, fs, st)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run 1 失败: %v", err)
	}

	// 第一次推送：产生 1 个集合
	if len(fg.collections) != 1 {
		t.Fatalf("首次推送后应有 1 个集合, 实际: %d", len(fg.collections))
	}
	oldCid := ""
	for cid := range fg.collections {
		oldCid = cid
	}

	// 模拟文件追加写入，内容变长、MTime 增加
	p := convPath
	newData := []byte("# 会话\n- 第一条消息\n- 第二条新消息")
	fs.files[p] = newData
	for i, entry := range fs.dirs[root+"/张三/企微/conversations"] {
		if entry.Path == p {
			fs.dirs[root+"/张三/企微/conversations"][i].Size = int64(len(newData))
			fs.dirs[root+"/张三/企微/conversations"][i].MTime = time.Unix(1760001000, 0)
			break
		}
	}

	// 第二次推送：文件已更新，应产生新集合并删除旧集合
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run 2 失败: %v", err)
	}

	if len(fg.collections) != 1 {
		t.Fatalf("文件更新后应只保留 1 个最新集合, 实际堆积了 %d 个集合: %v", len(fg.collections), fg.collections)
	}

	if _, exists := fg.collections[oldCid]; exists {
		t.Fatalf("旧集合 %s 未被删除，存在重复数据堆积", oldCid)
	}

		// 台账应该指向新集合
		fi, err := st.GetFileIndex("张三", "wecom", convPath)
		if err != nil || fi.CollectionID == oldCid {
			t.Fatalf("台账未更新为新集合: %v %+v", err, fi)
		}
	}

func TestPushDateSubdirectoryOptionA(t *testing.T) {
	fs := newFakeNAS()
	root := "/FastGPT-test"
	fs.dirs[root] = nil
	fs.addDir(root, "王五")
	fs.addDir(root+"/王五", "企微")
	fs.addDir(root+"/王五/企微", "conversations")
	fs.addDir(root+"/王五/企微", "2026-10-26") // 方案 A：按日期子目录归档
	fs.addFile(root+"/王五/企微/conversations", "与赵六的单聊.md", []byte("# 会话\n- 你好"))
	fs.addFile(root+"/王五/企微/2026-10-26", "产品采购合同.pdf", []byte("合同数据"))

	fg, baseURL := newFakeFastGPT(t)
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	task := newPushTask(t, baseURL, fs, st)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 验证上传列表包含纯净原始文件名
	if len(fg.uploads) != 2 {
		t.Fatalf("应推送 2 个文件, 实际: %d (%v)", len(fg.uploads), fg.uploads)
	}

	foundContract := false
	for _, up := range fg.uploads {
		if up == "产品采购合同.pdf" {
			foundContract = true
		}
	}
	if !foundContract {
		t.Fatalf("未找到纯净名称的产品采购合同.pdf: %v", fg.uploads)
	}

	// 验证台账中的分类被正确识别为「合同」
	fi, err := st.GetFileIndex("王五", "wecom", root+"/王五/企微/2026-10-26/产品采购合同.pdf")
	if err != nil || fi.CollectionID == "" {
		t.Fatalf("台账记录失败: %v", err)
	}
	if fi.Category != "合同" {
		t.Fatalf("日期子目录下的文件应根据文件名自动识别为 合同, 实际: %s", fi.Category)
	}
}


