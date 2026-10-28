package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if err := s.UpsertSharedRoot(SharedRoot{ViewerID: "ou_a", Token: "fld", DocType: "folder"}); err != nil {
		t.Fatalf("UpsertSharedRoot: %v", err)
	}
	if err := s.UpsertSharedRoot(SharedRoot{ViewerID: "ou_a", Token: "fld", DocType: "folder"}); err != nil {
		t.Fatalf("重复 UpsertSharedRoot: %v", err)
	}
	roots, err := s.ListSharedRoots("ou_a")
	if err != nil || len(roots) != 1 || roots[0].Token != "fld" || roots[0].DocType != "folder" {
		t.Fatalf("ListSharedRoots: %v %+v", err, roots)
	}
	if err := s.DeleteSharedRoot("ou_a", "fld"); err != nil {
		t.Fatal(err)
	}
	if roots, _ := s.ListSharedRoots("ou_a"); len(roots) != 0 {
		t.Fatalf("删除后应为空: %+v", roots)
	}
}
