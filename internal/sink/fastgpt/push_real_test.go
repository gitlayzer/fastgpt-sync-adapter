package fastgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
)

// 真实云端集成测试：FG_REAL=1 + FG_KEY=<fastgpt key> 时运行。
// 用内存 NAS 模拟目录结构，推送任务打真实 cloud.fastgpt.cn，验证后清理（删测试知识库）。

const realBaseURL = "https://cloud.fastgpt.cn"

func TestRealPushToCloud(t *testing.T) {
	if os.Getenv("FG_REAL") != "1" {
		t.Skip("设 FG_REAL=1 才跑真实云端集成测试")
	}
	apiKey := os.Getenv("FG_KEY")
	if apiKey == "" {
		t.Fatal("缺 FG_KEY")
	}

	person := "推送测试员"
	root := "/FastGPT-test"

	fs := newFakeNAS()
	fs.dirs[root] = nil
	fs.addDir(root, person)
	fs.addDir(path.Join(root, person), "企微")
	fs.addDir(path.Join(root, person, "企微"), "conversations")
	fs.addDir(path.Join(root, person, "企微"), "合同")
	fs.addDir(path.Join(root, person, "企微"), "messages")
	fs.addFile(path.Join(root, person, "企微", "conversations"), "与李四的单聊.md",
		[]byte("# 与李四的单聊\n\n- **[2026-10-22 10:00:01] 张三**：合同看一下\n- **[2026-10-22 10:01:20] 李四**：好的\n"))
	fs.addFile(path.Join(root, person, "企微", "合同"), "m999_云端推送测试合同.txt",
		[]byte("合同正文：甲方乙方……（FastGPT 云端推送端到端验证文件）"))
	fs.addFile(path.Join(root, person, "企微", "合同"), "m999_云端推送测试合同.txt.meta.json",
		[]byte(`{"record_id":"m999","category":"合同","frozen":false}`))
	fs.addFile(path.Join(root, person, "企微", "messages"), "m999.json", []byte(`{"msgid":"m999"}`))

	st, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	task := New(Config{BaseURL: realBaseURL, APIKey: apiKey, Interval: time.Minute}, fs, root, st)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 验证：知识库存在 + 文件在内 + 标签正确
	dsID, err := task.manager.FindDatasetByName(person)
	if err != nil || dsID == "" {
		t.Fatalf("知识库未创建: %v %q", err, dsID)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/core/dataset/delete?id=%s", realBaseURL, dsID), nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		t.Logf("已清理测试知识库 %s", dsID)
	})

	cols := listCollections(t, realBaseURL, apiKey, dsID)
	if len(cols) != 2 {
		t.Fatalf("知识库应有 2 个文件, 实际 %d: %v", len(cols), cols)
	}
	t.Logf("知识库 %s（%s）内文件:", person, dsID)
	for _, c := range cols {
		t.Logf("  - %s [%s]", c.Name, strings.Join(c.TagsFlat(), ","))
	}
	// 应有合同文件且带 "分类:合同" 标签（default_tag 数组内）；有会话 md
	var hasDocx, hasMd bool
	for _, c := range cols {
		if strings.Contains(c.Name, "云端推送测试合同") && c.HasTag("default_tag", "分类:合同") && c.HasTag("default_tag", "来源:企微") {
			hasDocx = true
		}
		if strings.HasSuffix(c.Name, ".md") {
			hasMd = true
		}
	}
	if !hasDocx {
		t.Error("缺合同文件或分类标签不对")
	}
	if !hasMd {
		t.Error("缺会话 markdown")
	}

	// 幂等：再跑一次，文件数不变
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run2: %v", err)
	}
	cols2 := listCollections(t, realBaseURL, apiKey, dsID)
	if len(cols2) != 2 {
		t.Fatalf("幂等失败, 第二次后文件数 %d", len(cols2))
	}
	t.Log("幂等验证通过")
}

// ===== 验证辅助 =====

type colInfo struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
	Tags []struct {
		Tag   string   `json:"tag"`
		Value []string `json:"value"`
	} `json:"tags"`
}

func (c colInfo) HasTag(tag, value string) bool {
	for _, tg := range c.Tags {
		if tg.Tag == tag {
			for _, v := range tg.Value {
				if v == value {
					return true
				}
			}
		}
	}
	return false
}

func (c colInfo) TagsFlat() []string {
	var out []string
	for _, tg := range c.Tags {
		for _, v := range tg.Value {
			out = append(out, tg.Tag+"="+v)
		}
	}
	return out
}

func listCollections(t *testing.T, base, key, datasetID string) []colInfo {
	t.Helper()
	body := fmt.Sprintf(`{"datasetId":%q,"pageNum":1,"pageSize":100}`, datasetID)
	req, _ := http.NewRequest("POST", base+"/api/core/dataset/collection/listV2", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listV2: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int `json:"code"`
		Data struct {
			List []colInfo `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Code != 200 {
		t.Fatalf("listV2 解析失败: %v body=%s", err, data)
	}
	return out.Data.List
}

var _ = synology.FileEntry{}
