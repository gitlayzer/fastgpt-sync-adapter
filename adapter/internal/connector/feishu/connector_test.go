package feishu

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	feishuapi "fastgpt-sync-adapter/adapter/internal/feishu"
	"fastgpt-sync-adapter/adapter/internal/storage"
)

// mockAPI 内存版飞书 API。
type mockAPI struct {
	folders   map[string][]feishuapi.DriveFile // folderToken -> children
	roots     map[string]string                // userToken -> rootFolderToken
	fileData  map[string]string                // fileToken -> content
	docxData  map[string]string
	downloads int
}

func newMockAPI() *mockAPI {
	return &mockAPI{
		folders:  map[string][]feishuapi.DriveFile{},
		roots:    map[string]string{},
		fileData: map[string]string{},
		docxData: map[string]string{},
	}
}

func (m *mockAPI) TenantAccessToken() (string, error) { return "tat", nil }

func (m *mockAPI) ListFolderChildren(token, folderToken string) ([]feishuapi.DriveFile, error) {
	return m.folders[folderToken], nil
}

func (m *mockAPI) WalkFolder(token, folderToken string, depth int, fn func(feishuapi.DriveFile)) error {
	if depth <= 0 {
		return nil
	}
	for _, f := range m.folders[folderToken] {
		if f.Type == "folder" {
			if err := m.WalkFolder(token, f.Token, depth-1, fn); err != nil {
				return err
			}
			continue
		}
		fn(f)
	}
	return nil
}

func (m *mockAPI) RootFolderMeta(token string) (string, error) { return m.roots[token], nil }

func (m *mockAPI) DownloadFile(token, fileToken string) (io.ReadCloser, error) {
	m.downloads++
	return io.NopCloser(strings.NewReader(m.fileData[fileToken])), nil
}

func (m *mockAPI) DocxRawContent(token, documentID string) (string, error) {
	m.downloads++
	return m.docxData[documentID], nil
}

func (m *mockAPI) DocRawContent(token, docToken string) (string, error) {
	m.downloads++
	return m.docxData[docToken], nil
}

func TestTenantScanAndChangeDetection(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	api := newMockAPI()
	api.folders["fldA"] = []feishuapi.DriveFile{
		{Token: "docx1", Name: "产品方案", Type: "docx", OwnerID: "ou_a", ModifiedTime: "1700000100"},
		{Token: "f1", Name: "采购合同.pdf", Type: "file", OwnerID: "ou_b", ModifiedTime: "1700000200"},
	}
	api.docxData["docx1"] = "# 产品方案正文"
	api.fileData["f1"] = "%PDF-1.4 合同内容"

	c := NewWithAPI(Config{Mode: "tenant", Folders: []string{"fldA"}}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 第一次 Fetch：2 篇都产出
	recs, next, err := c.Fetch(context.Background(), "", 10)
	if err != nil || len(recs) != 2 || next != "done" {
		t.Fatalf("首次产出: n=%d next=%s err=%v", len(recs), next, err)
	}
	byID := map[string]int{}
	for i, r := range recs {
		byID[r.ID] = i
	}
	docRec := recs[byID["docx1"]]
	if docRec.Attachments[0].FileID != "docx:docx1" || docRec.Attachments[0].Name != "产品方案.md" {
		t.Fatalf("docx 附件映射不对: %+v", docRec.Attachments[0])
	}
	fileRec := recs[byID["f1"]]
	if fileRec.Attachments[0].FileID != "file:f1" || fileRec.Attachments[0].Name != "采购合同.pdf" {
		t.Fatalf("file 附件映射不对: %+v", fileRec.Attachments[0])
	}

	// 附件拉取
	rc, err := c.FetchAttachment(context.Background(), &docRec.Attachments[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "# 产品方案正文" {
		t.Fatalf("docx 内容不对: %q", data)
	}

	// 第二次扫描：都未变化 → 0 产出
	recs2, _, _ := c.Fetch(context.Background(), "", 10)
	if len(recs2) != 0 {
		t.Fatalf("未变化应 0 产出, 实际 %d", len(recs2))
	}

	// 模拟一篇更新（modified_time 变新）→ 只产出它
	api.folders["fldA"][0].ModifiedTime = "1700000300"
	recs3, _, _ := c.Fetch(context.Background(), "", 10)
	if len(recs3) != 1 || recs3[0].ID != "docx1" {
		t.Fatalf("增量应只产出 docx1, 实际 %+v", recs3)
	}
}

func TestUserModeOwnerAndViewerBothStored(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	// 已授权用户 ou_viewer
	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_viewer", AccessToken: "ut1", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	api := newMockAPI()
	api.roots["ut1"] = "rootU"
	api.folders["rootU"] = []feishuapi.DriveFile{
		// 归属别人（ou_owner）但 viewer 可见
		{Token: "docx9", Name: "别人的方案", Type: "docx", OwnerID: "ou_owner", ModifiedTime: "1700000100"},
	}
	api.docxData["docx9"] = "内容"

	c := NewWithAPI(Config{Mode: "user"}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	recs, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("产出: n=%d err=%v", len(recs), err)
	}
	r := recs[0]
	if r.SenderID != "ou_owner" {
		t.Fatalf("归属应以所有者为主: %s", r.SenderID)
	}
	// owner + viewer 都在 OwnerIDs（NAS fan-out 各存一份）
	if len(r.OwnerIDs) != 2 || r.OwnerIDs[0] != "ou_owner" || r.OwnerIDs[1] != "ou_viewer" {
		t.Fatalf("OwnerIDs 应含 owner+viewer: %v", r.OwnerIDs)
	}
}
