package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
)

func TestLocalResolver(t *testing.T) {
	dir := t.TempDir()
	mapFile := filepath.Join(dir, "identity_map.json")

	st, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()

	r := NewLocalResolver(mapFile, st)

	// 文件不存在时：兜底返回 userid 本身
	if got := r.PersonName("wecom", "zhangsan"); got != "zhangsan" {
		t.Fatalf("无映射文件时应兜底为 userid, 得到 %q", got)
	}

	// 写入映射文件
	data, _ := json.Marshal([]Mapping{
		{WeComUserID: "zhangsan", FeishuUserID: "ou_csl", PersonName: "张三"},
		{WeComUserID: "lisi", FeishuUserID: "ou_dxt", PersonName: "李四"},
	})
	if err := os.WriteFile(mapFile, data, 0644); err != nil {
		t.Fatal(err)
	}
	if got := r.PersonName("wecom", "zhangsan"); got != "张三" {
		t.Fatalf("期望 张三, 得到 %q", got)
	}
	if got := r.PersonName("feishu", "ou_dxt"); got != "李四" {
		t.Fatalf("期望 李四, 得到 %q", got)
	}
	id, err := r.ResolveWeCom("lisi")
	if err != nil || id.FeishuUserID != "ou_dxt" {
		t.Fatalf("ResolveWeCom: %v %+v", err, id)
	}

	// mtime 变化自动重载：改文件
	time.Sleep(10 * time.Millisecond)
	data2, _ := json.Marshal([]Mapping{
		{WeComUserID: "zhangsan", FeishuUserID: "ou_csl", PersonName: "崔总"},
	})
	if err := os.WriteFile(mapFile, data2, 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(mapFile, time.Now(), time.Now().Add(time.Second))
	if got := r.PersonName("wecom", "zhangsan"); got != "崔总" {
		t.Fatalf("重载后应得 崔总, 得到 %q", got)
	}
}

type fakeBitableSource struct {
	recs []map[string]any
}

func (f *fakeBitableSource) TenantAccessToken() (string, error) { return "fake-token", nil }
func (f *fakeBitableSource) SearchRecords(token, appToken, tableID string) ([]feishuapi.BitableRecord, error) {
	var out []feishuapi.BitableRecord
	for _, r := range f.recs {
		out = append(out, feishuapi.BitableRecord{Fields: r})
	}
	return out, nil
}

func TestBitableResolver(t *testing.T) {
	dir := t.TempDir()
	st, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()

	src := &fakeBitableSource{
		recs: []map[string]any{
			// 1. 企微 + 飞书用户：显式 open_id 优先于人员对象
			{
				"企业微信UserID": "zhangsan",
				"企业微信姓名":   "张三",
				"飞书人员":     []any{map[string]any{"id": "ou_bitable_app_zhangsan"}},
				"飞书open_id":  "ou_sync_app_zhangsan",
			},
			// 2. 纯飞书用户：企微为空，应以 feishu:open_id 存入并保留名字
			{
				"企业微信UserID": "",
				"企业微信姓名":   "纯飞书员工",
				"飞书人员":     []any{map[string]any{"id": "ou_feishu_only"}},
				"飞书open_id":  "",
			},
		},
	}

	r := NewBitableResolver(BitableConfig{AppToken: "app", TableID: "tbl"}, src, st)
	if err := r.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// 验证张三查到的是优先的 ou_sync_app_zhangsan
	if got := r.PersonName("feishu", "ou_sync_app_zhangsan"); got != "张三" {
		t.Fatalf("期望 张三, 得到 %q", got)
	}
	// 验证纯飞书员工能查到名字
	if got := r.PersonName("feishu", "ou_feishu_only"); got != "纯飞书员工" {
		t.Fatalf("纯飞书员工应查到 纯飞书员工, 得到 %q", got)
	}
}
