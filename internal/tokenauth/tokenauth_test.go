package tokenauth

import (
	"path/filepath"
	"testing"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// newTestStore 建一个临时 SQLite，返回 store 与写入测试员工的辅助函数。
func newTestStore(t *testing.T) (*storage.Store, func(wecomID, name string)) {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	put := func(wecomID, name string) {
		if err := st.UpsertIdentity(&storage.Identity{
			WeComUserID: wecomID, FeishuUserID: "ou_" + wecomID, PersonName: name,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", wecomID, err)
		}
	}
	return st, put
}

func TestResolveTokenOK(t *testing.T) {
	st, put := newTestStore(t)
	put("zhangsan", "San 张三")
	token, err := st.IssueAPIToken("zhangsan")
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	a := New(st, "/FastGPT-test")
	personID, root, ok, err := a.ResolveToken(token)
	if err != nil || !ok {
		t.Fatalf("ResolveToken: ok=%v err=%v", ok, err)
	}
	if personID != "zhangsan" {
		t.Fatalf("personID = %q, 期望 zhangsan", personID)
	}
	if root != "/FastGPT-test/San 张三" {
		t.Fatalf("root = %q, 期望 /FastGPT-test/San 张三", root)
	}
}

func TestResolveTokenNASRootTrailingSlash(t *testing.T) {
	st, put := newTestStore(t)
	put("lisi", "Si 李四")
	token, _ := st.IssueAPIToken("lisi")

	a := New(st, "/FastGPT-test/")
	if _, root, ok, _ := a.ResolveToken(token); !ok || root != "/FastGPT-test/Si 李四" {
		t.Fatalf("尾斜杠应被规整，实际 root=%q ok=%v", root, ok)
	}
}

func TestResolveTokenRevoked(t *testing.T) {
	st, put := newTestStore(t)
	put("wangwu", "Wu 王五")
	token, _ := st.IssueAPIToken("wangwu")

	a := New(st, "/FastGPT-test")
	if _, _, ok, err := a.ResolveToken(token); !ok || err != nil {
		t.Fatalf("吊销前应可用: ok=%v err=%v", ok, err)
	}
	if n, err := st.RevokeAPITokens("wangwu"); err != nil || n != 1 {
		t.Fatalf("RevokeAPITokens: n=%d err=%v", n, err)
	}
	if _, root, ok, err := a.ResolveToken(token); ok || err != nil || root != "" {
		t.Fatalf("吊销后应 ok=false 且无错误: ok=%v err=%v root=%q", ok, err, root)
	}
}

func TestResolveTokenUnknown(t *testing.T) {
	st, _ := newTestStore(t)
	a := New(st, "/FastGPT-test")
	if _, _, ok, err := a.ResolveToken("fsa_unknown"); ok || err != nil {
		t.Fatalf("未知 token 应 ok=false 且无错误: ok=%v err=%v", ok, err)
	}
	// 空 token 直接 false，不查库
	if _, _, ok, _ := a.ResolveToken("   "); ok {
		t.Fatal("空 token 应 ok=false")
	}
}

func TestRootOf(t *testing.T) {
	st, put := newTestStore(t)
	a := New(st, "/FastGPT-test")

	// person_name 为空 → false
	put("nobody", "")
	if root, ok := a.RootOf("nobody"); ok || root != "" {
		t.Fatalf("person_name 为空应 false，实际 root=%q ok=%v", root, ok)
	}
	// 身份不存在 → false
	if _, ok := a.RootOf("ghost"); ok {
		t.Fatal("身份不存在应 false")
	}
	// 正常
	put("zhaoliu", "Liu 赵六")
	if root, ok := a.RootOf("zhaoliu"); !ok || root != "/FastGPT-test/Liu 赵六" {
		t.Fatalf("RootOf = %q ok=%v", root, ok)
	}
}

func TestRootOfInvalidPersonName(t *testing.T) {
	st, put := newTestStore(t)
	a := New(st, "/FastGPT-test")

	for _, name := range []string{".", "..", "a/b", "../etc", `a\b`, "x\x00y", "\n"} {
		put("bad", name)
		if root, ok := a.RootOf("bad"); ok || root != "" {
			t.Fatalf("非法 person_name %q 应 false，实际 root=%q ok=%v", name, root, ok)
		}
	}
}

func TestValidPersonName(t *testing.T) {
	valid := []string{"张三", "San 张三", "a.b", "-x-", " 中间 空格 "}
	for _, n := range valid {
		if !ValidPersonName(n) {
			t.Fatalf("%q 应合法", n)
		}
	}
	invalid := []string{"", ".", "..", "/", "a/b", `a\b`, "tab\t"}
	for _, n := range invalid {
		if ValidPersonName(n) {
			t.Fatalf("%q 应非法", n)
		}
	}
}

func TestIssuedTokenWithEmptyNameIsUnauthorized(t *testing.T) {
	// token 有效但员工姓名缺失时按无效 token 处理（401），不能返回 500。
	st, put := newTestStore(t)
	put("noname", "")
	token, _ := st.IssueAPIToken("noname")

	a := New(st, "/FastGPT-test")
	if _, root, ok, err := a.ResolveToken(token); ok || err != nil || root != "" {
		t.Fatalf("姓名缺失应 ok=false / err=nil，实际 ok=%v err=%v root=%q", ok, err, root)
	}
}
