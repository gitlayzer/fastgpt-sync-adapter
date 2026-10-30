package feishu

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// mockAPI 内存版飞书 API。
type mockAPI struct {
	folders   map[string][]feishuapi.DriveFile // folderToken -> children
	roots     map[string]string                // userToken -> rootFolderToken
	fileData  map[string]string                // fileToken -> content
	docxData  map[string]string
	downloads int

	wikiSpaces map[string][]feishuapi.WikiSpace // userToken -> spaces
	wikiNodes  map[string][]feishuapi.WikiNode  // spaceID -> nodes
	exports    int                              // 导出次数

	members map[string][]feishuapi.PermissionMember // docToken -> 协作者
}

func newMockAPI() *mockAPI {
	return &mockAPI{
		folders:    map[string][]feishuapi.DriveFile{},
		roots:      map[string]string{},
		fileData:   map[string]string{},
		docxData:   map[string]string{},
		wikiSpaces: map[string][]feishuapi.WikiSpace{},
		wikiNodes:  map[string][]feishuapi.WikiNode{},
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

// wiki 相关：默认返回空，需要时在测试里赋值
func (m *mockAPI) ListWikiSpaces(token string) ([]feishuapi.WikiSpace, error) {
	return m.wikiSpaces[token], nil
}

func (m *mockAPI) GetMyLibrarySpace(userToken string) (*feishuapi.WikiSpace, error) {
	return nil, nil
}

func (m *mockAPI) WalkWikiSpace(token, spaceID string, maxDepth int, fn func(feishuapi.WikiSpace, feishuapi.WikiNode)) error {
	for _, n := range m.wikiNodes[spaceID] {
		fn(feishuapi.WikiSpace{SpaceID: spaceID}, n)
	}
	return nil
}

// 导出：测试里把 docx/sheet 内容模拟成「导出的文件字节」
func (m *mockAPI) ExportFile(tok, token, docType string) (string, error) {
	m.exports++
	return "ft_" + token, nil
}

func (m *mockAPI) DownloadExport(tok, fileToken string) (io.ReadCloser, error) {
	m.downloads++
	token := strings.TrimPrefix(fileToken, "ft_")
	return io.NopCloser(strings.NewReader(m.docxData[token])), nil
}

func (m *mockAPI) SearchDocs(userToken, searchKey string, offset, count int) ([]feishuapi.SearchDocEntity, error) {
	return nil, nil
}

func (m *mockAPI) ListPermissionMembers(userToken, token, docType string) ([]feishuapi.PermissionMember, error) {
	return m.members[token], nil
}

func (m *mockAPI) SearchMinutes(userToken string) ([]feishuapi.MinuteItem, error) {
	return nil, nil
}

func (m *mockAPI) SearchDocWiki(string, string, []string, int64, int64, string) ([]feishuapi.DocWikiResult, string, error) {
	return nil, "", nil
}

func (m *mockAPI) BatchQueryMetas(string, [][2]string) ([]feishuapi.DocMeta, []feishuapi.DocMetaFailed, error) {
	return nil, nil, nil
}

func (m *mockAPI) DownloadMinutesTranscript(userToken, minuteToken string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("逐字稿发言内容")), nil
}

func (m *mockAPI) ListMailFolders(userToken string) ([]feishuapi.MailFolder, error) {
	return nil, nil
}

func (m *mockAPI) ListMailboxMessages(userToken, folderID, pageToken string) ([]string, string, bool, error) {
	return nil, "", false, nil
}

func (m *mockAPI) GetMailMessage(userToken, messageID string) (*feishuapi.MailMessage, error) {
	return nil, nil
}

func (m *mockAPI) DownloadMailAttachment(userToken, messageID, attachmentID string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("邮件附件流")), nil
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
		// docx 走导出，归档名是 .docx（源文件），末尾带唯一 token 避免同名覆盖
		if docRec.Attachments[0].FileID != "docx:docx1" || docRec.Attachments[0].Name != "产品方案_docx1.docx" {
			t.Fatalf("docx 附件映射不对: %+v", docRec.Attachments[0])
		}
		fileRec := recs[byID["f1"]]
		if fileRec.Attachments[0].FileID != "file:f1" || fileRec.Attachments[0].Name != "采购合同_f1.pdf" {
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

	// 第二次扫描：Ack 之后才视作已处理 → 0 产出
	// （状态在 Ack 时才写，模拟引擎在 Sink 写入成功后的回调）
	for _, r := range recs {
		if err := c.Ack(r.ID); err != nil {
			t.Fatal(err)
		}
	}
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

// TestUserModeWikiOnly 复现真实场景：用户「我的文档库」里有文档，
// 但 drive 根目录为空（飞书新版把个人文档库归入 wiki 体系）。
// 只扫 drive 会得到 0 篇，必须走 wiki 才能正常产出。
func TestUserModeWikiOnly(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_u", AccessToken: "ut1", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	api := newMockAPI()
	api.roots["ut1"] = "rootU"
	api.folders["rootU"] = nil // 云空间为空——这是关键前提

	api.wikiSpaces["ut1"] = []feishuapi.WikiSpace{
		{SpaceID: "sp1", Name: "我的文档库", SpaceType: "person"},
	}
	api.wikiNodes["sp1"] = []feishuapi.WikiNode{
		{Token: "nd1", ObjToken: "doccn1", ObjType: "docx", Title: "杜新田的测试文档", NodeEditTime: "1700000100"},
		{Token: "nd2", ObjToken: "sht1", ObjType: "sheet", Title: "员工名册", NodeEditTime: "1700000100"},
		{Token: "nd3", ObjToken: "pdf1", ObjType: "file", Title: "迁移文档.pdf", NodeEditTime: "1700000100"},
	}
	api.docxData["doccn1"] = "docx-bytes"
	api.docxData["sht1"] = "xlsx-bytes"

	c := NewWithAPI(Config{Mode: "user", WikiEnabled: true}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	recs, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// docx + sheet + file 三种都要产出（sheet 现在通过导出支持）
	if len(recs) != 3 {
		t.Fatalf("应产出 3 篇（docx/sheet/file），实际 %d: %+v", len(recs), recs)
	}
	byID := map[string]model.Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	if byID["doccn1"].Attachments[0].FileID != "docx:doccn1" {
		t.Fatalf("docx 附件标识错误: %s", byID["doccn1"].Attachments[0].FileID)
	}
		// sheet 归档名应为 .xlsx 且带唯一 token 短码防同名覆盖
		if got := byID["sht1"].Attachments[0].Name; got != "员工名册_sht1.xlsx" {
			t.Fatalf("sheet 归档名应为 员工名册_sht1.xlsx，实际 %q", got)
		}
		// file 应带唯一 token 短码防同名覆盖
		if got := byID["pdf1"].Attachments[0].Name; got != "迁移文档_pdf1.pdf" {
			t.Fatalf("file 归档名应为 迁移文档_pdf1.pdf，实际 %q", got)
		}

		// 第二轮应无新增（Ack 之后 doc_state 才记录）
		for _, r := range recs {
			if err := c.Ack(r.ID); err != nil {
				t.Fatal(err)
			}
		}
		recs2, _, err := c.Fetch(context.Background(), "", 10)
		if err != nil || len(recs2) != 0 {
			t.Fatalf("第二轮应无新数据: n=%d err=%v", len(recs2), err)
		}
	}

func TestFeishuCloudDeleteEmitsDeleteRecord(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_u", AccessToken: "ut1", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	api := newMockAPI()
	api.roots["ut1"] = "rootU"
	api.folders["rootU"] = []feishuapi.DriveFile{
		{Token: "doc1", Name: "即将被删除的文档.docx", Type: "docx", ModifiedTime: "1700000100", OwnerID: "ou_u"},
		{Token: "doc2", Name: "正常保留的文档.docx", Type: "docx", ModifiedTime: "1700000100", OwnerID: "ou_u"},
	}

	c := NewWithAPI(Config{Mode: "user"}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 第一轮：产出 2 篇并全部 Ack
	recs1, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil || len(recs1) != 2 {
		t.Fatalf("首轮应产出 2 篇, 实际: %d", len(recs1))
	}
	for _, r := range recs1 {
		if err := c.Ack(r.ID); err != nil {
			t.Fatal(err)
		}
	}

	// 模拟飞书云端将 doc1 彻底删除或移出，rootU 中只剩下 doc2
	api.folders["rootU"] = []feishuapi.DriveFile{
		{Token: "doc2", Name: "正常保留的文档.docx", Type: "docx", ModifiedTime: "1700000100", OwnerID: "ou_u"},
	}

	// 第二轮：差集比对，应产出 1 条 Kind="delete" 的删除记录
	recs2, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("Fetch 2: %v", err)
	}
	if len(recs2) != 1 {
		t.Fatalf("次轮应检测到 1 篇云端删除，实际产出 %d 篇: %+v", len(recs2), recs2)
	}
	if recs2[0].Kind != "delete" || recs2[0].ID != "del_doc1_ou_u" {
		t.Fatalf("删除记录属性不符合预期: %+v", recs2[0])
	}

	// Ack 删除后，feishu_doc_state 中的对应记录应被清理
	if err := c.Ack(recs2[0].ID); err != nil {
		t.Fatal(err)
	}
	if st.GetDocEditTime("doc1") != 0 {
		t.Fatalf("Ack 删除后，doc1 的状态应被彻底清理")
	}
}


// TestNotAckedMeansRetry 验证幂等性的关键语义：
// 没有 Ack 的记录（模拟写入 Sink 失败）下一轮必须重新产出，
// 否则数据会静默丢失。
func TestNotAckedMeansRetry(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_u", AccessToken: "ut1", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	api := newMockAPI()
	api.roots["ut1"] = "rootU"
	api.wikiSpaces["ut1"] = []feishuapi.WikiSpace{{SpaceID: "sp1", Name: "我的文档库"}}
	api.wikiNodes["sp1"] = []feishuapi.WikiNode{
		{Token: "nd1", ObjToken: "doc1", ObjType: "docx", Title: "文档", NodeEditTime: "1700000100"},
	}
	api.docxData["doc1"] = "内容"

	c := NewWithAPI(Config{Mode: "user", WikiEnabled: true}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 第一轮产出但不 Ack（模拟写 NAS 失败）
	recs1, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil || len(recs1) != 1 {
		t.Fatalf("第一轮: n=%d err=%v", len(recs1), err)
	}

	// 第二轮必须重新产出同一篇——这是幂等性的保证
	recs2, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 1 || recs2[0].ID != "doc1" {
		t.Fatalf("未 Ack 的记录应重新产出, 实际 n=%d", len(recs2))
	}

		// Ack 之后不再产出
		if err := c.Ack("doc1"); err != nil {
			t.Fatal(err)
		}
		recs3, _, err := c.Fetch(context.Background(), "", 10)
		if err != nil || len(recs3) != 0 {
			t.Fatalf("Ack 后应无产出, 实际 n=%d err=%v", len(recs3), err)
		}
	}

type mockSearchAPI struct {
	*mockAPI
	sharedDocs []feishuapi.SearchDocEntity
}

func (m *mockSearchAPI) SearchDocs(userToken, searchKey string, offset, count int) ([]feishuapi.SearchDocEntity, error) {
	return m.sharedDocs, nil
}

func TestSharedWithMeSearchAndOwnerViewerStored(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_viewer", AccessToken: "ut_viewer", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	baseAPI := newMockAPI()
	baseAPI.roots["ut_viewer"] = "root_viewer"
	baseAPI.docxData["doc_shared"] = "共享文档内容"

	api := &mockSearchAPI{
		mockAPI: baseAPI,
		sharedDocs: []feishuapi.SearchDocEntity{
			{Token: "doc_shared", Title: "IT运维部任务管理.docx", Type: "docx", OwnerID: "ou_luke"},
		},
	}

	c := NewWithAPI(Config{Mode: "user", SearchKeys: []string{"任务"}}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	recs, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("应抓取到 1 篇与我共享文档, 实际: %d", len(recs))
	}

	r := recs[0]
	if r.SenderID != "ou_luke" {
		t.Fatalf("归属人(Owner)应为 ou_luke, 实际: %s", r.SenderID)
	}
	// OwnerIDs 必须同时包含所有者 ou_luke 和查看人 ou_viewer
	hasOwner := false
	hasViewer := false
	for _, oid := range r.OwnerIDs {
		if oid == "ou_luke" {
			hasOwner = true
		}
		if oid == "ou_viewer" {
			hasViewer = true
		}
	}
	if !hasOwner || !hasViewer {
		t.Fatalf("OwnerIDs 必须同时包含 Owner(ou_luke) 与 Viewer(ou_viewer), 实际: %v", r.OwnerIDs)
	}
}

type mockMinutesAndMailAPI struct {
	*mockAPI
	minutes []feishuapi.MinuteItem
	mailIDs []string
	mailMap map[string]*feishuapi.MailMessage
}

func (m *mockMinutesAndMailAPI) SearchMinutes(userToken string) ([]feishuapi.MinuteItem, error) {
	return m.minutes, nil
}

func (m *mockMinutesAndMailAPI) DownloadMinutesTranscript(userToken, minuteToken string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("宋文帅：今天讨论项目方案\n张建丰：收到")), nil
}

func (m *mockMinutesAndMailAPI) ListMailboxMessages(userToken, folderID, pageToken string) ([]string, string, bool, error) {
	if folderID != "INBOX" {
		return nil, "", false, nil
	}
	return m.mailIDs, "", false, nil
}

func (m *mockMinutesAndMailAPI) GetMailMessage(userToken, messageID string) (*feishuapi.MailMessage, error) {
	if m.mailMap != nil {
		return m.mailMap[messageID], nil
	}
	return nil, nil
}

func (m *mockMinutesAndMailAPI) DownloadMailAttachment(userToken, messageID, attachmentID string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("邮件合同PDF字节")), nil
}

func TestMinutesSyncAndTranscriptExport(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_u", AccessToken: "ut_min", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	baseAPI := newMockAPI()
	baseAPI.roots["ut_min"] = "root_min"

	api := &mockMinutesAndMailAPI{
		mockAPI: baseAPI,
		minutes: []feishuapi.MinuteItem{
			{MinuteToken: "min123", Title: "制造中心项目讨论", OwnerID: "ou_u", CreateTime: 1700000000000, Duration: 1800},
		},
	}

	c := NewWithAPI(Config{Mode: "user", MinutesEnabled: true}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	recs, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("应抓取到 1 篇妙记, 实际: %d", len(recs))
	}
	r := recs[0]
	if r.Kind != "minutes" || r.ID != "min123" {
		t.Fatalf("妙记记录属性错误: %+v", r)
	}
	if len(r.Attachments) != 1 || r.Attachments[0].Name != "制造中心项目讨论.md" {
		t.Fatalf("妙记附件名错误: %+v", r.Attachments)
	}

	// 验证 FetchAttachment 导出的 Markdown 结构
	rc, err := c.FetchAttachment(context.Background(), &r.Attachments[0])
	if err != nil {
		t.Fatalf("FetchAttachment 失败: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	text := string(data)
	if !strings.Contains(text, "制造中心项目讨论") || !strings.Contains(text, "逐字稿与发言记录") {
		t.Fatalf("妙记 Markdown 结构不完整: %s", text)
	}
}

func TestMailAttachmentsSync(t *testing.T) {
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_u", AccessToken: "ut_mail", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})

	baseAPI := newMockAPI()
	baseAPI.roots["ut_mail"] = "root_mail"

	api := &mockMinutesAndMailAPI{
		mockAPI: baseAPI,
		mailIDs: []string{"msg_m1"},
		mailMap: map[string]*feishuapi.MailMessage{
			"msg_m1": {
				MessageID:   "msg_m1",
				Subject:     "采购合同确认",
				CreatedTime: 1700000000000,
				Attachments: []feishuapi.MailAttachment{
					{ID: "att_1", Name: "采购合同.pdf", Size: 1024},
				},
			},
		},
	}

	c := NewWithAPI(Config{Mode: "user"}, st, api)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	recs, _, err := c.Fetch(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("应抓取到 1 封含附件邮件, 实际: %d", len(recs))
	}
	r := recs[0]
	if r.Kind != "mail" || len(r.Attachments) != 2 || r.Attachments[0].Name != "2023-11-15_采购合同确认_msg_m1.html" ||
		r.Attachments[1].Name != "采购合同_msg_m1.pdf" {
		t.Fatalf("邮件记录附件属性错误: %+v", r)
	}

	rc, err := c.FetchAttachment(context.Background(), &r.Attachments[1])
	if err != nil {
		t.Fatalf("FetchAttachment 失败: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "邮件合同PDF字节" {
		t.Fatalf("下载内容不符合预期: %s", string(data))
	}
}


