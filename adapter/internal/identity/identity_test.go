package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fastgpt-sync-adapter/adapter/internal/storage"
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
