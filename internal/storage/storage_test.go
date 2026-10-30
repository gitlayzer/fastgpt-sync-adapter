package storage

import (
	"os"
	"path/filepath"
	"strings"
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

// 死信表：同一 (source, record_id) 只保留一条，重复写入更新 record/last_error。
// 旧库没有 feishu_mail_file 表：打开时自动建表，能写能读。
func TestMigrateAddsMailFileTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE feishu_mail_file`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("旧库重新打开: %v", err)
	}
	defer s.Close()
	if err := s.AddMailFiles("p1", "mail_m1", []string{"a.html", "b.pdf", "a.html"}); err != nil {
		t.Fatal(err)
	}
	if got := s.ListMailFiles("p1", "mail_m1"); len(got) != 2 || got[0] != "a.html" || got[1] != "b.pdf" {
		t.Fatalf("文件名记录不符: %v", got)
	}
}

func TestMailDeleteRecords(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddMailSeen("p1", "i1", "smtp1")
	s.AddMailSeen("p1", "s1", "smtp1")
	s.AddMailSeen("p1", "x1", "")
	s.AddMailSeen("p2", "i1", "smtp1")
	s.AddMailFiles("p1", "mail_i1", []string{"a.html"})
	if got := s.MailCopyIDs("p1", "i1"); len(got) != 2 {
		t.Fatalf("同一封邮件应有两份拷贝，实际 %v", got)
	}
	if got := s.MailCopyIDs("p1", "x1"); len(got) != 1 {
		t.Fatalf("没有 RFC 邮件 ID 时只有自己，实际 %v", got)
	}
	if err := s.DeleteMailSeen("p1", "i1", "mail_i1"); err != nil {
		t.Fatal(err)
	}
	if s.MailSeen("p1", "i1") || s.MailSeen("p1", "s1") || len(s.ListMailFiles("p1", "mail_i1")) != 0 {
		t.Fatalf("两份拷贝的记录都应清掉")
	}
	if !s.MailSeen("p1", "x1") || !s.MailSeen("p2", "i1") {
		t.Fatalf("其他邮件和其他员工的记录不受影响")
	}
}

func TestDeadLetter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dead.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.AddDeadLetter("wecom", "msg-1", []byte(`{"id":"msg-1"}`), "第一次失败"); err != nil {
		t.Fatalf("AddDeadLetter: %v", err)
	}
	if err := s.AddDeadLetter("wecom", "msg-1", []byte(`{"id":"msg-1","v":2}`), "第二次失败"); err != nil {
		t.Fatalf("AddDeadLetter 更新: %v", err)
	}
	if n, err := s.CountDeadLetters("wecom"); err != nil || n != 1 {
		t.Fatalf("同一条重复写入后应仍为 1 条: n=%d err=%v", n, err)
	}
	var record, lastErr string
	if err := s.db.QueryRow(`SELECT record, last_error FROM dead_letter WHERE source=? AND record_id=?`,
		"wecom", "msg-1").Scan(&record, &lastErr); err != nil {
		t.Fatalf("回读死信: %v", err)
	}
	if lastErr != "第二次失败" {
		t.Fatalf("last_error 应为第二次的值: %q", lastErr)
	}
	if record != `{"id":"msg-1","v":2}` {
		t.Fatalf("record 应被更新: %q", record)
	}

	if err := s.AddDeadLetter("wecom", "msg-2", []byte(`{"id":"msg-2"}`), "失败"); err != nil {
		t.Fatalf("AddDeadLetter msg-2: %v", err)
	}
	if n, _ := s.CountDeadLetters("wecom"); n != 2 {
		t.Fatalf("应有 2 条死信: %d", n)
	}
	if n, _ := s.CountDeadLetters("feishu"); n != 0 {
		t.Fatalf("其它 source 应为 0 条: %d", n)
	}
}

// 旧库 identities 没有 status/leave_date/offboarded_at 列：重开后自动补列，旧记录按在职处理。
func TestMigrateIdentityRosterColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE identities;
CREATE TABLE identities (
  wecom_user_id  TEXT PRIMARY KEY,
  feishu_user_id TEXT NOT NULL DEFAULT '',
  person_name    TEXT NOT NULL DEFAULT '',
  updated_at     INTEGER NOT NULL DEFAULT 0
);
INSERT INTO identities (wecom_user_id, feishu_user_id, person_name, updated_at)
VALUES ('legacy_user', 'ou_legacy', '老王', 100);`); err != nil {
		t.Fatalf("准备旧结构: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("迁移重开失败: %v", err)
	}
	defer s.Close()

	for _, col := range []string{"status", "leave_date", "offboarded_at"} {
		if !s.hasColumn("identities", col) {
			t.Fatalf("迁移后应存在 identities.%s 列", col)
		}
	}
	id, err := s.GetIdentityByWeCom("legacy_user")
	if err != nil {
		t.Fatalf("GetIdentityByWeCom: %v", err)
	}
	if id.Status != IdentityActive || id.LeaveDate != "" || id.OffboardedAt != 0 {
		t.Fatalf("旧记录应视为在职且未处理: %+v", id)
	}
	// 旧记录也能被纳管（姓名非空、唯一）
	if _, ok, err := s.ManagedByWeCom("legacy_user"); err != nil || !ok {
		t.Fatalf("旧记录应纳管: ok=%v err=%v", ok, err)
	}
}

// 状态流转：active→offboarded 保留 offboarded_at；offboarded→active（复职）清零。
func TestIdentityStatusTransitions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.UpsertIdentity(&Identity{WeComUserID: "u1", PersonName: "张三"}); err != nil {
		t.Fatal(err)
	}
	// 空状态按在职存库
	if id, _ := s.GetIdentityByWeCom("u1"); id.Status != IdentityActive {
		t.Fatalf("空 status 应存为 active，得到 %q", id.Status)
	}
	if err := s.SetOffboarded("u1", 1700000000); err != nil {
		t.Fatalf("SetOffboarded: %v", err)
	}
	// 改离职（不带 offboarded_at）不应覆盖已记录的离职处理时间
	if err := s.UpsertIdentity(&Identity{WeComUserID: "u1", PersonName: "张三", Status: IdentityOffboarded, LeaveDate: "2024-01-31"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.GetIdentityByWeCom("u1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Status != IdentityOffboarded || id.LeaveDate != "2024-01-31" || id.OffboardedAt != 1700000000 {
		t.Fatalf("离职状态不符: %+v", id)
	}
	// 复职：状态回 active，offboarded_at 清零
	if err := s.UpsertIdentity(&Identity{WeComUserID: "u1", PersonName: "张三", Status: IdentityActive}); err != nil {
		t.Fatal(err)
	}
	id, _ = s.GetIdentityByWeCom("u1")
	if id.Status != IdentityActive || id.OffboardedAt != 0 {
		t.Fatalf("复职后应清空 offboarded_at: %+v", id)
	}
	// 复职前再写一次 offboarded_at 验证不会被 active 之外的更新误清
	if err := s.SetOffboarded("u1", 42); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIdentity(&Identity{WeComUserID: "u1", PersonName: "张三", Status: IdentityOffboarded}); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.GetIdentityByWeCom("u1"); id.OffboardedAt != 42 {
		t.Fatalf("非 active 更新不应覆盖 offboarded_at: %+v", id)
	}
	// ListIdentities 也要带出新字段
	list, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(list) != 1 || list[0].Status != IdentityOffboarded || list[0].OffboardedAt != 42 {
		t.Fatalf("ListIdentities 未读出新字段: %+v", list)
	}
}

// MarkIdentitiesRemoved：只动不在 keep 里且尚未移出的行；重复调用不再计入。
func TestMarkIdentitiesRemoved(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, id := range []*Identity{
		{WeComUserID: "keep1", PersonName: "张三"},
		{WeComUserID: "gone1", PersonName: "李四"},
		{WeComUserID: "gone2", PersonName: "王五", Status: IdentityOffboarded},
	} {
		if err := s.UpsertIdentity(id); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.MarkIdentitiesRemoved([]string{"keep1", "gone2"})
	if err != nil {
		t.Fatalf("MarkIdentitiesRemoved: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只标记 1 行，实际 %d", n)
	}
	if id, _ := s.GetIdentityByWeCom("keep1"); id.Status != IdentityActive {
		t.Fatalf("keep 内的人不应被改状态: %+v", id)
	}
	if id, _ := s.GetIdentityByWeCom("gone2"); id.Status != IdentityOffboarded {
		t.Fatalf("名单内已离职的人应保持 offboarded: %+v", id)
	}
	if id, _ := s.GetIdentityByWeCom("gone1"); id.Status != IdentityRemoved {
		t.Fatalf("不在名单内的人应标记 removed: %+v", id)
	}

	// 再次调用：已 removed 的行不再计入
	if n, err = s.MarkIdentitiesRemoved([]string{"keep1", "gone2"}); err != nil || n != 0 {
		t.Fatalf("重复标记应为 0 行: n=%d err=%v", n, err)
	}
	// 名单为空等效于全员不在名单内
	if n, err = s.MarkIdentitiesRemoved(nil); err != nil || n != 2 {
		t.Fatalf("空名单应把剩余 2 行标记移出: n=%d err=%v", n, err)
	}
}

// 纳管判定：不存在 / 离职 / 移出 / 姓名空 / 重名。
func TestManagedIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// 不存在
	if id, ok, err := s.ManagedByWeCom("nobody"); err != nil || ok || id != nil {
		t.Fatalf("不存在的企微 ID 不应纳管: %+v,%v,%v", id, ok, err)
	}
	if id, ok, err := s.ManagedByFeishu("ou_nobody"); err != nil || ok || id != nil {
		t.Fatalf("不存在的飞书 ID 不应纳管: %+v,%v,%v", id, ok, err)
	}
	// openID 为空直接 false
	if id, ok, err := s.ManagedByFeishu("  "); err != nil || ok || id != nil {
		t.Fatalf("空 openID 不应纳管: %+v,%v,%v", id, ok, err)
	}

	for _, id := range []*Identity{
		{WeComUserID: "active1", FeishuUserID: "ou_active1", PersonName: "张三"},
		{WeComUserID: "left1", FeishuUserID: "ou_left1", PersonName: "李四", Status: IdentityOffboarded},
		{WeComUserID: "gone1", FeishuUserID: "ou_gone1", PersonName: "王五", Status: IdentityRemoved},
		{WeComUserID: "noname", FeishuUserID: "ou_noname", PersonName: "   "},
		// 重名两人
		{WeComUserID: "dup1", FeishuUserID: "ou_dup1", PersonName: "赵六"},
		{WeComUserID: "dup2", FeishuUserID: "ou_dup2", PersonName: " 赵六 "},
	} {
		if err := s.UpsertIdentity(id); err != nil {
			t.Fatal(err)
		}
	}

	if id, ok, err := s.ManagedByWeCom("active1"); err != nil || !ok || id.PersonName != "张三" {
		t.Fatalf("在职且姓名唯一应纳管: %+v,%v,%v", id, ok, err)
	}
	if _, ok, err := s.ManagedByFeishu("ou_active1"); err != nil || !ok {
		t.Fatalf("按飞书 ID 应纳管: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.ManagedByWeCom("left1"); err != nil || ok {
		t.Fatalf("离职员工不应纳管: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.ManagedByWeCom("gone1"); err != nil || ok {
		t.Fatalf("已移出员工不应纳管: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.ManagedByWeCom("noname"); err != nil || ok {
		t.Fatalf("姓名为空（去空格）不应纳管: ok=%v err=%v", ok, err)
	}

	// 重名：两人都 false
	for _, uid := range []string{"dup1", "dup2"} {
		if _, ok, err := s.ManagedByWeCom(uid); err != nil || ok {
			t.Fatalf("重名 %s 不应纳管: ok=%v err=%v", uid, ok, err)
		}
	}
	dups, err := s.DuplicateActiveNames()
	if err != nil {
		t.Fatalf("DuplicateActiveNames: %v", err)
	}
	if ids := dups["赵六"]; len(ids) != 2 {
		t.Fatalf("应报告赵六重名 2 人，实际 %v", dups)
	}
	if _, ok := dups["张三"]; ok {
		t.Fatalf("唯一姓名不应出现在重名表: %v", dups)
	}

	// 其中一人离职后，另一人恢复纳管且不再重名
	if err := s.UpsertIdentity(&Identity{WeComUserID: "dup1", FeishuUserID: "ou_dup1", PersonName: "赵六", Status: IdentityOffboarded}); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := s.ManagedByWeCom("dup2"); err != nil || !ok || strings.TrimSpace(id.PersonName) != "赵六" {
		t.Fatalf("重名者之一离职后另一人应纳管: %+v,%v,%v", id, ok, err)
	}
	if dups, err = s.DuplicateActiveNames(); err != nil || len(dups) != 0 {
		t.Fatalf("重名表应为空: %v %v", dups, err)
	}
}

// 外部联系人缓存：首次保存 label 生效；改名后再存 name 更新、label 不变；notfound 记录可读出。
func TestExternalContactCache(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "contact.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const id = "woCORP001"
	// 没有记录：ok=false，不报错
	if c, ok, err := s.GetExternalContact(id); err != nil || ok || c.ExternalUserID != "" {
		t.Fatalf("空库应返回 ok=false: %+v,%v,%v", c, ok, err)
	}

	first := ExternalContact{
		ExternalUserID: id, Name: "张三", CorpName: "某公司",
		Label: "张三@某公司_RP001", Status: "ok", FetchedAt: 100,
	}
	if err := s.SaveExternalContact(first); err != nil {
		t.Fatalf("SaveExternalContact: %v", err)
	}
	got, ok, err := s.GetExternalContact(id)
	if err != nil || !ok {
		t.Fatalf("GetExternalContact: %v ok=%v", err, ok)
	}
	if got.Label != "张三@某公司_RP001" || got.Name != "张三" || got.Status != "ok" || got.FetchedAt != 100 {
		t.Fatalf("首次保存不符: %+v", got)
	}

	// 第二次：名字变了、传新 label → name/status/fetched_at 覆盖，label 保持旧值
	if err := s.SaveExternalContact(ExternalContact{
		ExternalUserID: id, Name: "张三丰", CorpName: "某某公司",
		Label: "张三丰@某某公司_RP001", Status: "ok", FetchedAt: 200,
	}); err != nil {
		t.Fatalf("SaveExternalContact 更新: %v", err)
	}
	got, _, err = s.GetExternalContact(id)
	if err != nil {
		t.Fatalf("GetExternalContact: %v", err)
	}
	if got.Name != "张三丰" || got.CorpName != "某某公司" || got.FetchedAt != 200 {
		t.Fatalf("name/corp_name/fetched_at 应被覆盖: %+v", got)
	}
	if got.Label != "张三@某公司_RP001" {
		t.Fatalf("label 首次写入后不应再变，实际 %q", got.Label)
	}

	// notfound：name 保留旧值（不清空），label 不动
	if err := s.SaveExternalContact(ExternalContact{
		ExternalUserID: id, Name: got.Name, CorpName: got.CorpName,
		Status: "notfound", FetchedAt: 300,
	}); err != nil {
		t.Fatalf("SaveExternalContact notfound: %v", err)
	}
	got, ok, err = s.GetExternalContact(id)
	if err != nil || !ok {
		t.Fatalf("读 notfound 记录: %v ok=%v", err, ok)
	}
	if got.Status != "notfound" || got.Name != "张三丰" || got.Label != "张三@某公司_RP001" || got.FetchedAt != 300 {
		t.Fatalf("notfound 记录不符: %+v", got)
	}
}

// 迁移：旧版 room_names（无 source 列）升级后补列，旧记录照常可读。
func TestMigrateRoomNamesSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_room.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE room_names;
CREATE TABLE room_names (
  room_id    TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO room_names (room_id, name, updated_at) VALUES ('wrTESTLEGACY', '老群名', 100);`); err != nil {
		t.Fatalf("准备旧结构: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("迁移重开失败: %v", err)
	}
	defer s.Close()

	if !s.hasColumn("room_names", "source") {
		t.Fatal("迁移后应存在 room_names.source 列")
	}
	name, source, ok := s.GetRoomNameWithSource("wrTESTLEGACY")
	if !ok || name != "老群名" || source != "" {
		t.Fatalf("旧记录读回不符: %q %q %v", name, source, ok)
	}
	// 无记录：ok=false，不报错
	if n, src, ok := s.GetRoomNameWithSource("wrTESTNONE"); ok || n != "" || src != "" {
		t.Fatalf("无记录应返回 ok=false: %q %q %v", n, src, ok)
	}
}

// 推断名只在首次写入时生效，之后同一群沿用旧名字（不再随成员变化）。
func TestSetInferredRoomNameKeepsFirst(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const room = "wrTESTINFER01"
	got, err := s.SetInferredRoomName(room, "张三、李四的群聊")
	if err != nil {
		t.Fatalf("SetInferredRoomName: %v", err)
	}
	if got != "张三、李四的群聊" {
		t.Fatalf("首次写入应生效，得到 %q", got)
	}
	name, source, ok := s.GetRoomNameWithSource(room)
	if !ok || name != "张三、李四的群聊" || source != RoomNameSourceInferred {
		t.Fatalf("首次写入记录不符: %q %q %v", name, source, ok)
	}

	// 第二次传不同名字：不覆盖，返回第一次的名字
	got, err = s.SetInferredRoomName(room, "张三、李四、王五的群聊")
	if err != nil {
		t.Fatalf("SetInferredRoomName 二次: %v", err)
	}
	if got != "张三、李四的群聊" {
		t.Fatalf("已有记录不应被覆盖，返回 %q", got)
	}
	if name, _, _ := s.GetRoomNameWithSource(room); name != "张三、李四的群聊" {
		t.Fatalf("库里的名字不应变化，得到 %q", name)
	}

	// 库里记录的名字为空（例如外部写入空串）时允许补写
	if err := s.SetRoomName(room, ""); err != nil {
		t.Fatalf("SetRoomName: %v", err)
	}
	got, err = s.SetInferredRoomName(room, "张三、李四、王五的群聊")
	if err != nil {
		t.Fatalf("SetInferredRoomName 空记录补写: %v", err)
	}
	if got != "张三、李四、王五的群聊" {
		t.Fatalf("名字为空时应允许补写，得到 %q", got)
	}
}

// 客户群名缓存：全字段覆盖读写。
func TestGroupChatCache(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "group.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const room = "wrTESTGROUP01"
	if g, ok, err := s.GetGroupChat(room); err != nil || ok || g.RoomID != "" {
		t.Fatalf("空库应返回 ok=false: %+v %v %v", g, ok, err)
	}
	if err := s.SaveGroupChat(GroupChat{RoomID: room, Name: "江湾测试群", Status: "ok", FetchedAt: 100}); err != nil {
		t.Fatalf("SaveGroupChat: %v", err)
	}
	g, ok, err := s.GetGroupChat(room)
	if err != nil || !ok {
		t.Fatalf("GetGroupChat: %v ok=%v", err, ok)
	}
	if g.RoomID != room || g.Name != "江湾测试群" || g.Status != "ok" || g.FetchedAt != 100 {
		t.Fatalf("首次保存不符: %+v", g)
	}

	// 第二次：群改名 → 全字段覆盖
	if err := s.SaveGroupChat(GroupChat{RoomID: room, Name: "江湾新群", Status: "ok", FetchedAt: 200}); err != nil {
		t.Fatalf("SaveGroupChat 覆盖: %v", err)
	}
	g, _, err = s.GetGroupChat(room)
	if err != nil {
		t.Fatalf("GetGroupChat: %v", err)
	}
	if g.Name != "江湾新群" || g.FetchedAt != 200 {
		t.Fatalf("应全字段覆盖: %+v", g)
	}

	// notfound 覆盖 ok（name 清空属正常）
	if err := s.SaveGroupChat(GroupChat{RoomID: room, Name: "", Status: "notfound", FetchedAt: 300}); err != nil {
		t.Fatalf("SaveGroupChat notfound: %v", err)
	}
	g, ok, err = s.GetGroupChat(room)
	if err != nil || !ok {
		t.Fatalf("读 notfound 记录: %v ok=%v", err, ok)
	}
	if g.Status != "notfound" || g.Name != "" || g.FetchedAt != 300 {
		t.Fatalf("notfound 记录不符: %+v", g)
	}
}
