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
