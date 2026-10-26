package nasstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
)

func TestChildrenFlattening(t *testing.T) {
	// 模拟群晖 WebAPI
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Query().Get("folder_path")
		if unescaped, err := url.QueryUnescape(p); err == nil {
			p = unescaped
		}

		var files []map[string]any
		switch p {
		case "/FastGPT-test":
			files = []map[string]any{
				{"name": "Jeff 张建丰", "path": "/FastGPT-test/Jeff 张建丰", "isdir": true},
			}
		case "/FastGPT-test/Jeff 张建丰":
			files = []map[string]any{
				{"name": "企微", "path": "/FastGPT-test/Jeff 张建丰/企微", "isdir": true},
			}
		case "/FastGPT-test/Jeff 张建丰/企微":
			files = []map[string]any{
				{"name": "2026-10-26", "path": "/FastGPT-test/Jeff 张建丰/企微/2026-10-26", "isdir": true},
				{"name": "conversations", "path": "/FastGPT-test/Jeff 张建丰/企微/conversations", "isdir": true},
			}
		case "/FastGPT-test/Jeff 张建丰/企微/2026-10-26":
			files = []map[string]any{
				{"name": "合同.pdf", "path": "/FastGPT-test/Jeff 张建丰/企微/2026-10-26/合同.pdf", "isdir": false},
			}
		case "/FastGPT-test/Jeff 张建丰/企微/conversations":
			files = []map[string]any{
				{"name": "群聊_A.md", "path": "/FastGPT-test/Jeff 张建丰/企微/conversations/群聊_A.md", "isdir": false},
			}
		}

		resp := map[string]any{
			"success": true,
			"data":    map[string]any{"files": files},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := synology.New(srv.URL, "user", "pass")
	backend := New(client, "/FastGPT-test")

	// 1. 根目录：只返回员工目录 (folder)
	rootChildren, err := backend.Children("")
	if err != nil {
		t.Fatalf("Children root: %v", err)
	}
	if len(rootChildren) != 1 || rootChildren[0].Name != "Jeff 张建丰" || rootChildren[0].Type != "folder" {
		t.Fatalf("根目录应返回 1 个员工文件夹, 实际: %+v", rootChildren)
	}

	// 2. 员工目录：API 层虚拟平铺，直接返回该员工名下的全部文件 (file)，不返回中间目录
	empChildren, err := backend.Children(rootChildren[0].ID)
	if err != nil {
		t.Fatalf("Children employee: %v", err)
	}
	if len(empChildren) != 2 {
		t.Fatalf("员工目录应平铺返回 2 个文件, 实际返回 %d 项: %+v", len(empChildren), empChildren)
	}

	foundContract := false
	foundConv := false
	for _, n := range empChildren {
		if n.Type != "file" {
			t.Fatalf("平铺结果项类型必须为 file, 实际: %+v", n)
		}
		if strings.Contains(n.Name, "合同.pdf") {
			foundContract = true
		}
		if strings.Contains(n.Name, "群聊_A.md") {
			foundConv = true
		}
	}
	if !foundContract || !foundConv {
		t.Fatalf("平铺结果缺少预期文件: %+v", empChildren)
	}
}
