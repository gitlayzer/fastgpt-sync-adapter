package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/apitoken"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// OAuthGrant
	g := &OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_xxx", Code: "c1",
		AccessToken: "at1", RefreshToken: "rt1",
		ExpireAt: time.Now().Unix() + 7200, RefreshExpireAt: time.Now().Unix() + 30*86400,
		Status: "authorized",
	}
	if err := s.UpsertOAuthGrant(g); err != nil {
		t.Fatalf("UpsertOAuthGrant: %v", err)
	}
	got, err := s.GetOAuthGrant("feishu", "ou_xxx")
	if err != nil {
		t.Fatalf("GetOAuthGrant: %v", err)
	}
	if got.AccessToken != "at1" || got.RefreshToken != "rt1" || got.Status != "authorized" {
		t.Fatalf("grant 回读不符: %+v", got)
	}
	// 更新（模拟 refresh）
	g.AccessToken = "at2"
	if err := s.UpsertOAuthGrant(g); err != nil {
		t.Fatalf("UpsertOAuthGrant 更新: %v", err)
	}
	got2, _ := s.GetOAuthGrant("feishu", "ou_xxx")
	if got2.AccessToken != "at2" {
		t.Fatalf("grant 更新失败: %s", got2.AccessToken)
	}
	list, err := s.ListOAuthGrants("feishu")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListOAuthGrants: %v len=%d", err, len(list))
	}
	if _, err := s.GetOAuthGrant("feishu", "nobody"); err != ErrNotFound {
		t.Fatalf("期望 ErrNotFound, 得到 %v", err)
	}

	// Identity
	if err := s.UpsertIdentity(&Identity{WeComUserID: "zhangsan", FeishuUserID: "ou_csl", PersonName: "张三"}); err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	id1, err := s.GetIdentityByWeCom("zhangsan")
	if err != nil || id1.PersonName != "张三" {
		t.Fatalf("GetIdentityByWeCom: %v %+v", err, id1)
	}
	id2, err := s.GetIdentityByFeishu("ou_csl")
	if err != nil || id2.WeComUserID != "zhangsan" {
		t.Fatalf("GetIdentityByFeishu: %v %+v", err, id2)
	}

	// FileIndex
	if err := s.UpsertFileIndex(&FileIndexEntry{Person: "张三", Source: "wecom", NasPath: "/FastGPT-test/张三/企微/合同/x.docx", Category: "合同", CollectionID: "col1", Checksum: "abc"}); err != nil {
		t.Fatalf("UpsertFileIndex: %v", err)
	}
	fi, err := s.GetFileIndex("张三", "wecom", "/FastGPT-test/张三/企微/合同/x.docx")
	if err != nil || fi.CollectionID != "col1" || fi.Category != "合同" {
		t.Fatalf("GetFileIndex: %v %+v", err, fi)
	}

	// Checkpoint
	if v, err := s.GetCheckpoint("wecom"); err != nil || v != "" {
		t.Fatalf("空 checkpoint 应返回空串: %q %v", v, err)
	}
	if err := s.SetCheckpoint("wecom", "2762"); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if v, _ := s.GetCheckpoint("wecom"); v != "2762" {
		t.Fatalf("GetCheckpoint: %q", v)
	}
	if err := s.SetCheckpoint("wecom", "3000"); err != nil {
		t.Fatalf("SetCheckpoint 更新: %v", err)
	}
	if v, _ := s.GetCheckpoint("wecom"); v != "3000" {
		t.Fatalf("GetCheckpoint 更新: %q", v)
	}

	// 重开（持久化验证）
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.GetCheckpoint("wecom"); v != "3000" {
		t.Fatalf("重开后 checkpoint 丢失: %q", v)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db 文件不存在: %v", err)
	}
}

func TestMigrateExistingTableWithoutKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	// 手动创建旧版表结构（缺少 kind 列）
	sRaw, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 重建旧表结构（无 kind 字段）
	if _, err := sRaw.db.Exec(`
DROP TABLE IF EXISTS feishu_doc_copy;
CREATE TABLE feishu_doc_copy (
  token      TEXT NOT NULL,
  person_id  TEXT NOT NULL,
  via        TEXT NOT NULL DEFAULT '',
  edit_time  INTEGER NOT NULL DEFAULT 0,
  file_name  TEXT NOT NULL DEFAULT '',
  owner_id   TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (token, person_id)
);
INSERT INTO feishu_doc_state (token, edit_time, title, owner_id, updated_at)
VALUES ('old_doc_1', 12345, '旧文档.docx', 'ou_test', 100);
`); err != nil {
		t.Fatalf("准备旧结构: %v", err)
	}
	sRaw.Close()

	// 重新打开：应自动执行 ALTER TABLE 补充 kind 列并完成回填
	sMigrated, err := Open(path)
	if err != nil {
		t.Fatalf("迁移重开失败: %v", err)
	}
	defer sMigrated.Close()

	copies, err := sMigrated.ListCopiesByPerson("ou_test")
	if err != nil {
		t.Fatalf("ListCopiesByPerson: %v", err)
	}
	if len(copies) != 1 || copies[0].Token != "old_doc_1" {
		t.Fatalf("期望回填 1 条记录，实际: %+v", copies)
	}

	// 验证 UpsertCopy 写入带 kind 字段正常
	if err := sMigrated.UpsertCopy(DocCopy{
		Token: "min_1", PersonID: "ou_test", Via: "owner",
		EditTime: 123456, FileName: "会议纪要.md", OwnerID: "ou_test", Kind: "minutes",
	}); err != nil {
		t.Fatalf("UpsertCopy with kind: %v", err)
	}

	updated, err := sMigrated.ListCopiesByPerson("ou_test")
	if err != nil || len(updated) != 2 {
		t.Fatalf("更新后应有 2 条记录，实际: %v len=%d", err, len(updated))
	}
	var foundMinutes bool
	for _, c := range updated {
		if c.Token == "min_1" && c.Kind == "minutes" {
			foundMinutes = true
		}
	}
	if !foundMinutes {
		t.Fatalf("未找到 kind=minutes 的副本: %+v", updated)
	}
}

// 旧库（没有 feishu_shared_root 表）重新打开后能正常读写共享表。
func TestMigrateAddsSharedRootTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE feishu_shared_root`); err != nil {
		t.Fatalf("准备旧结构: %v", err)
	}
	if err := s.UpsertCopy(DocCopy{Token: "old_doc", PersonID: "ou_a", Via: "owner", FileName: "旧.docx"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("迁移重开失败: %v", err)
	}
	defer s.Close()
	if cs, err := s.ListCopiesByPerson("ou_a"); err != nil || len(cs) != 1 {
		t.Fatalf("旧数据应保留: %v %+v", err, cs)
	}
	if err := s.UpsertSharedRoot(SharedRoot{ViewerID: "ou_a", Token: "fld"}); err != nil {
		t.Fatalf("UpsertSharedRoot: %v", err)
	}
	if err := s.UpsertSharedRoot(SharedRoot{ViewerID: "ou_a", Token: "fld"}); err != nil {
		t.Fatalf("重复 UpsertSharedRoot: %v", err)
	}
	roots, err := s.ListSharedRoots("ou_a")
	if err != nil || len(roots) != 1 || roots[0].Token != "fld" {
		t.Fatalf("ListSharedRoots: %v %+v", err, roots)
	}
	if err := s.DeleteSharedRoot("ou_a", "fld"); err != nil {
		t.Fatal(err)
	}
	if roots, _ := s.ListSharedRoots("ou_a"); len(roots) != 0 {
		t.Fatalf("删除后应为空: %+v", roots)
	}
}

// 旧库（共享表没有 doc_type 列，只记文件夹）重新打开：补上列，已有记录视为文件夹，新记录能写智能纪要。
func TestMigrateSharedRootDocType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE feishu_shared_root;
CREATE TABLE feishu_shared_root (viewer_id TEXT NOT NULL, token TEXT NOT NULL, PRIMARY KEY (viewer_id, token));
INSERT INTO feishu_shared_root (viewer_id, token) VALUES ('ou_a', 'old_fld');`); err != nil {
		t.Fatalf("准备旧结构: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("迁移重开失败: %v", err)
	}
	defer s.Close()
	if err := s.UpsertSharedRoot(SharedRoot{ViewerID: "ou_a", Token: "note1", DocType: "docx"}); err != nil {
		t.Fatalf("UpsertSharedRoot: %v", err)
	}
	roots, err := s.ListSharedRoots("ou_a")
	if err != nil || len(roots) != 2 {
		t.Fatalf("ListSharedRoots: %v %+v", err, roots)
	}
	got := map[string]string{}
	for _, r := range roots {
		got[r.Token] = r.DocType
	}
	if got["old_fld"] != "folder" || got["note1"] != "docx" {
		t.Fatalf("旧记录应为 folder、新记录为 docx，实际 %+v", got)
	}
}

// 员工 API token 全生命周期：签发 / 轮换（宽限期）/ 过期 / 吊销 / 列表。
func TestAPITokenLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const person = "zhangsan"
	// 空 personID 应报错，不落库
	if _, err := s.IssueAPIToken("  "); err == nil {
		t.Fatal("空 personID 签发应报错")
	}
	if _, err := s.RotateAPIToken("", time.Hour); err == nil {
		t.Fatal("空 personID 轮换应报错")
	}

	// Issue → Lookup 命中
	tok1, err := s.IssueAPIToken(person)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	got, ok, err := s.LookupAPIToken(tok1)
	if err != nil || !ok || got != person {
		t.Fatalf("LookupAPIToken(tok1) = %q,%v,%v", got, ok, err)
	}
	if has, err := s.HasActiveAPIToken(person); err != nil || !has {
		t.Fatalf("HasActiveAPIToken = %v,%v", has, err)
	}
	// 错误明文不命中
	if _, ok, err := s.LookupAPIToken(tok1 + "x"); err != nil || ok {
		t.Fatalf("错误明文不应命中: %v,%v", ok, err)
	}

	// Rotate(grace=1h)：新旧都命中
	tok2, err := s.RotateAPIToken(person, time.Hour)
	if err != nil {
		t.Fatalf("RotateAPIToken: %v", err)
	}
	if tok2 == tok1 {
		t.Fatal("轮换后明文应不同")
	}
	// 只有旧 token 带宽限期，新 token 不过期
	var newExp int64
	if err := s.db.QueryRow(`SELECT expires_at FROM api_tokens WHERE token_hash=?`,
		apitoken.Hash(tok2)).Scan(&newExp); err != nil || newExp != 0 {
		t.Fatalf("新 token 不应设过期时间: %d,%v", newExp, err)
	}
	for name, tk := range map[string]string{"新": tok2, "旧(宽限期)": tok1} {
		if got, ok, err := s.LookupAPIToken(tk); err != nil || !ok || got != person {
			t.Fatalf("%s token 应命中: %q,%v,%v", name, got, ok, err)
		}
	}

	// 把旧 token 的 expires_at 改成过去 → 旧的不命中、新的命中
	if _, err := s.db.Exec(`UPDATE api_tokens SET expires_at=? WHERE token_hash=?`,
		time.Now().Add(-time.Minute).Unix(), apitoken.Hash(tok1)); err != nil {
		t.Fatalf("手动改旧 token 过期时间: %v", err)
	}
	if _, ok, err := s.LookupAPIToken(tok1); err != nil || ok {
		t.Fatalf("过期 token 不应命中: %v,%v", ok, err)
	}
	if got, ok, err := s.LookupAPIToken(tok2); err != nil || !ok || got != person {
		t.Fatalf("新 token 仍应命中: %q,%v,%v", got, ok, err)
	}

	// ListAPITokens：两条，且不含明文
	list, err := s.ListAPITokens()
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有两枚 token，实际 %d: %+v", len(list), list)
	}
	for _, tk := range list {
		if tk.Display == "" || tk.Display == tok1 || tk.Display == tok2 {
			t.Fatalf("Display 不应为空或等于明文: %+v", tk)
		}
		if tk.PersonID != person || tk.CreatedAt == 0 {
			t.Fatalf("token 记录不符: %+v", tk)
		}
		if tk.Status != "active" {
			t.Fatalf("这两枚都应为 active: %+v", tk)
		}
	}

	// Revoke → 两个都不命中，HasActive=false
	n, err := s.RevokeAPITokens(person)
	if err != nil {
		t.Fatalf("RevokeAPITokens: %v", err)
	}
	if n != 2 {
		t.Fatalf("应吊销 2 条，实际 %d", n)
	}
	for _, tk := range []string{tok1, tok2} {
		if _, ok, err := s.LookupAPIToken(tk); err != nil || ok {
			t.Fatalf("吊销后不应命中: %v,%v", ok, err)
		}
	}
	if has, err := s.HasActiveAPIToken(person); err != nil || has {
		t.Fatalf("吊销后 HasActive 应为 false: %v,%v", has, err)
	}
	// 其他员工不受影响
	other, err := s.IssueAPIToken("lisi")
	if err != nil {
		t.Fatalf("IssueAPIToken(lisi): %v", err)
	}
	if got, ok, err := s.LookupAPIToken(other); err != nil || !ok || got != "lisi" {
		t.Fatalf("他人 token 应命中: %q,%v,%v", got, ok, err)
	}
}

// 旧库没有 feishu_mail_folder 表：重开时自动建表；没有记录返回 0，写入后可读回、重复写会更新。
func TestMailFolderScannedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE feishu_mail_folder`); err != nil {
		t.Fatalf("准备旧结构: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("迁移重开失败: %v", err)
	}
	defer s.Close()
	if ts := s.MailFolderScannedAt("ou_a", "INBOX"); ts != 0 {
		t.Fatalf("没有记录应返回 0，实际 %d", ts)
	}
	if err := s.SetMailFolderScannedAt("ou_a", "INBOX", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMailFolderScannedAt("ou_a", "INBOX", 200); err != nil {
		t.Fatal(err)
	}
	if ts := s.MailFolderScannedAt("ou_a", "INBOX"); ts != 200 {
		t.Fatalf("应读回最后一次写入的 200，实际 %d", ts)
	}
	if ts := s.MailFolderScannedAt("ou_b", "INBOX"); ts != 0 {
		t.Fatalf("其他员工不受影响，实际 %d", ts)
	}
}

// 已处理邮件表：按 message_id 和 RFC 邮件 ID 都能查到；空的 RFC 邮件 ID 不算命中。
func TestMailSeen(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.AddMailSeen("ou_a", "m1", "<x@mail>"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMailSeen("ou_a", "m2", ""); err != nil {
		t.Fatal(err)
	}
	if !s.MailSeen("ou_a", "m1") || !s.MailSeen("ou_a", "m2") || s.MailSeen("ou_b", "m1") {
		t.Fatal("按 message_id 查询不符")
	}
	if !s.MailSeenSMTP("ou_a", "<x@mail>") || s.MailSeenSMTP("ou_b", "<x@mail>") || s.MailSeenSMTP("ou_a", "") {
		t.Fatal("按 RFC 邮件 ID 查询不符")
	}
}
