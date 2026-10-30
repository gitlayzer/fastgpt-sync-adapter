package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
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

	// 文件不存在时：不在名单里，不是纳管员工，没有名字
	if got := r.PersonName("wecom", "zhangsan"); got != "" {
		t.Fatalf("无映射文件时应返回空串, 得到 %q", got)
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

// TestLocalResolverLoadsOnConstruct 构造即加载：飞书共享文件夹发现等模块直接读 identities 表，
// 不能等到第一次 Resolve 调用才把映射文件写入。
func TestLocalResolverLoadsOnConstruct(t *testing.T) {
	t.Run("映射文件存在", func(t *testing.T) {
		dir := t.TempDir()
		mapFile := filepath.Join(dir, "identity_map.json")

		data, _ := json.Marshal([]Mapping{
			{WeComUserID: "u1", FeishuUserID: "ou_x", PersonName: "张三"},
		})
		if err := os.WriteFile(mapFile, data, 0644); err != nil {
			t.Fatal(err)
		}

		st, err := storage.Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatalf("storage.Open: %v", err)
		}
		defer st.Close()

		// 只构造，不调用任何 Resolve 方法
		_ = NewLocalResolver(mapFile, st)

		ids, err := st.ListFeishuUserIDs()
		if err != nil {
			t.Fatalf("ListFeishuUserIDs: %v", err)
		}
		found := false
		for _, id := range ids {
			if id == "ou_x" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("构造后 identities 表应含 ou_x, 得到 %v", ids)
		}
	})

	t.Run("映射文件不存在", func(t *testing.T) {
		dir := t.TempDir()
		mapFile := filepath.Join(dir, "missing.json")

		st, err := storage.Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatalf("storage.Open: %v", err)
		}
		defer st.Close()

		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("文件不存在时构造不应 panic: %v", rec)
			}
		}()
		_ = NewLocalResolver(mapFile, st)

		ids, err := st.ListFeishuUserIDs()
		if err != nil {
			t.Fatalf("ListFeishuUserIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("无映射文件时应为空, 得到 %v", ids)
		}
	})
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
				"企业微信姓名":     "张三",
				"飞书人员":       []any{map[string]any{"id": "ou_bitable_app_zhangsan"}},
				"飞书open_id":  "ou_sync_app_zhangsan",
			},
			// 2. 纯飞书用户：企微为空，应以 feishu:open_id 存入并保留名字
			{
				"企业微信UserID": "",
				"企业微信姓名":     "纯飞书员工",
				"飞书人员":       []any{map[string]any{"id": "ou_feishu_only"}},
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

func TestParseStatus(t *testing.T) {
	cases := map[string]string{
		"":           storage.IdentityActive,
		"在职":         storage.IdentityActive,
		"active":     storage.IdentityActive,
		" 在职 ":       storage.IdentityActive,
		"离职":         storage.IdentityOffboarded,
		" 离职\t":      storage.IdentityOffboarded,
		"offboarded": storage.IdentityOffboarded,
		"OFFBOARDED": storage.IdentityOffboarded,
		"其它状态":       storage.IdentityActive,
	}
	for in, want := range cases {
		if got := parseStatus(in); got != want {
			t.Fatalf("parseStatus(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 多维表格同步：读取状态列（单选字符串）与离职日期；表里少了一人后该人被标记为移出。
func TestBitableResolverRosterStatus(t *testing.T) {
	dir := t.TempDir()
	st, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()

	src := &fakeBitableSource{
		recs: []map[string]any{
			{ // 在职（状态列出空）
				"企业微信UserID": "zhangsan",
				"企业微信姓名":     "张三",
				"飞书open_id":  "ou_zhangsan",
			},
			{ // 离职（单选返回纯字符串）
				"企业微信UserID": "lisi",
				"企业微信姓名":     "李四",
				"飞书open_id":  "ou_lisi",
				"状态":         "离职",
				"离职日期":       "2024-01-31",
			},
			{ // 即将从名单消失
				"企业微信UserID": "wangwu",
				"企业微信姓名":     "王五",
				"飞书open_id":  "ou_wangwu",
				"状态":         "在职",
			},
		},
	}

	r := NewBitableResolver(BitableConfig{AppToken: "app", TableID: "tbl"}, src, st)
	if err := r.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	zhangsan, err := st.GetIdentityByWeCom("zhangsan")
	if err != nil {
		t.Fatalf("GetIdentityByWeCom: %v", err)
	}
	if zhangsan.Status != storage.IdentityActive || zhangsan.LeaveDate != "" {
		t.Fatalf("状态列为空应为在职: %+v", zhangsan)
	}
	lisi, err := st.GetIdentityByWeCom("lisi")
	if err != nil {
		t.Fatalf("GetIdentityByWeCom(lisi): %v", err)
	}
	if lisi.Status != storage.IdentityOffboarded || lisi.LeaveDate != "2024-01-31" {
		t.Fatalf("应读出离职状态与离职日期: %+v", lisi)
	}

	// 在职员工纳管；离职员工不纳管
	if id, ok := r.Managed("wecom", "zhangsan"); !ok || id.PersonName != "张三" {
		t.Fatalf("在职员工应纳管: %+v,%v", id, ok)
	}
	if id, ok := r.Managed("wecom", "lisi"); ok || id != nil {
		t.Fatalf("离职员工不应纳管: %+v,%v", id, ok)
	}
	if id, ok := r.Managed("feishu", "ou_zhangsan"); !ok || id.PersonName != "张三" {
		t.Fatalf("按飞书 ID 也应纳管: %+v,%v", id, ok)
	}

	// 下一轮：王五从名单里消失
	src.recs = src.recs[:2]
	if err := r.Sync(); err != nil {
		t.Fatalf("Sync(第二轮): %v", err)
	}
	wangwu, err := st.GetIdentityByWeCom("wangwu")
	if err != nil {
		t.Fatalf("GetIdentityByWeCom(wangwu): %v", err)
	}
	if wangwu.Status != storage.IdentityRemoved {
		t.Fatalf("从名单消失的人应标记 removed: %+v", wangwu)
	}
	if _, ok := r.Managed("wecom", "wangwu"); ok {
		t.Fatal("已移出名单的人不应纳管")
	}
	if _, ok := r.Managed("wecom", "zhangsan"); !ok {
		t.Fatal("仍在名单内的在职员工应继续纳管")
	}
}

// PersonName 只对纳管员工返回姓名，其余返回空串；Managed 覆盖正常/非正常。
func TestPersonNameOnlyManaged(t *testing.T) {
	dir := t.TempDir()
	mapFile := filepath.Join(dir, "identity_map.json")
	st, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()

	data, _ := json.Marshal([]Mapping{
		{WeComUserID: "zhangsan", FeishuUserID: "ou_zhangsan", PersonName: "张三"},
		{WeComUserID: "lisi", FeishuUserID: "ou_lisi", PersonName: "李四", Status: "离职", LeaveDate: "2024-01-31"},
		{WeComUserID: "nomane", FeishuUserID: "ou_nomane", PersonName: "  "},
		{WeComUserID: "dup1", FeishuUserID: "ou_dup1", PersonName: "赵六"},
		{WeComUserID: "dup2", FeishuUserID: "ou_dup2", PersonName: "赵六"},
	})
	if err := os.WriteFile(mapFile, data, 0644); err != nil {
		t.Fatal(err)
	}
	r := NewLocalResolver(mapFile, st)

	// 纳管员工：返回姓名
	if got := r.PersonName("wecom", "zhangsan"); got != "张三" {
		t.Fatalf("纳管员工应返回姓名, 得到 %q", got)
	}
	if got := r.PersonName("feishu", "ou_zhangsan"); got != "张三" {
		t.Fatalf("按飞书 ID 也应返回姓名, 得到 %q", got)
	}
	// 非纳管：不在名单 / 离职 / 姓名空 / 重名 / 未知 source 都返回空串
	for _, tc := range []struct{ source, id, why string }{
		{"wecom", "nobody", "不在名单"},
		{"feishu", "ou_nobody", "不在名单"},
		{"wecom", "lisi", "已离职"},
		{"feishu", "ou_lisi", "已离职"},
		{"wecom", "nomane", "姓名空"},
		{"wecom", "dup1", "重名"},
		{"wecom", "dup2", "重名"},
		{"unknown", "zhangsan", "未知 source"},
	} {
		if got := r.PersonName(tc.source, tc.id); got != "" {
			t.Fatalf("%s(%s %s) 应返回空串, 得到 %q", tc.why, tc.source, tc.id, got)
		}
	}

	// Managed：正常返回行与 true；非正常返回 false
	id, ok := r.Managed("wecom", "zhangsan")
	if !ok || id == nil || id.PersonName != "张三" || id.Status != storage.IdentityActive {
		t.Fatalf("Managed(zhangsan) = %+v,%v", id, ok)
	}
	for _, tc := range []struct{ source, id string }{
		{"wecom", "nobody"}, {"wecom", "lisi"}, {"wecom", "nomane"},
		{"wecom", "dup1"}, {"wecom", "dup2"}, {"feishu", "ou_lisi"}, {"", "zhangsan"},
	} {
		if id, ok := r.Managed(tc.source, tc.id); ok || id != nil {
			t.Fatalf("Managed(%s %s) 不应纳管: %+v,%v", tc.source, tc.id, id, ok)
		}
	}

	// 名单里去掉重名的其中一人后，另一人恢复纳管
	time.Sleep(10 * time.Millisecond)
	data2, _ := json.Marshal([]Mapping{
		{WeComUserID: "zhangsan", FeishuUserID: "ou_zhangsan", PersonName: "张三"},
		{WeComUserID: "nomane", FeishuUserID: "ou_nomane", PersonName: "  "},
		{WeComUserID: "dup2", FeishuUserID: "ou_dup2", PersonName: "赵六"},
	})
	if err := os.WriteFile(mapFile, data2, 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(mapFile, time.Now(), time.Now().Add(2*time.Second))
	if got := r.PersonName("wecom", "dup2"); got != "赵六" {
		t.Fatalf("重名者离开名单后应纳管, 得到 %q", got)
	}
	// 从名单里消失的人（dup1）应被标记移出
	dup1, err := st.GetIdentityByWeCom("dup1")
	if err != nil {
		t.Fatalf("GetIdentityByWeCom(dup1): %v", err)
	}
	if dup1.Status != storage.IdentityRemoved {
		t.Fatalf("从名单消失的人应标记 removed: %+v", dup1)
	}
}
