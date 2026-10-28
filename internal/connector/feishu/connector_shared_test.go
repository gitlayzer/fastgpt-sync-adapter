package feishu

import (
	"errors"
	"fmt"
	"testing"
	"time"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// sharedAPI 模拟搜索与批量查元数据：docs 是“查看者能搜到的文档”，按创建人、创建时间过滤。
type sharedAPI struct {
	*mockAPI
	docs      []feishuapi.DocWikiResult
	created   map[string]int64             // token → 创建时间（秒），模拟按创建时间过滤
	metas     map[string]feishuapi.DocMeta // token → 元数据（查得到）
	failed    map[string]int               // token → 失败码
	metaErr   error
	invalid   map[string]bool // 无效的 open_id
	searchCap int             // 单次搜索最多返回条数（模拟 200 上限）
	searches  int
}

func newSharedAPI() *sharedAPI {
	return &sharedAPI{mockAPI: newMockAPI(), metas: map[string]feishuapi.DocMeta{},
		failed: map[string]int{}, invalid: map[string]bool{}, created: map[string]int64{}, searchCap: 200}
}

func (m *sharedAPI) SearchDocWiki(tok string, ids []string, start, end int64, pageToken string) ([]feishuapi.DocWikiResult, string, error) {
	m.searches++
	want := map[string]bool{}
	for _, id := range ids {
		if m.invalid[id] {
			return nil, "", &feishuapi.APIError{Code: codeInvalidOpenID, Msg: "not a valid {open_id}"}
		}
		want[id] = true
	}
	var hit []feishuapi.DocWikiResult
	for _, d := range m.docs {
		if ct := m.created[d.Token]; want[d.OwnerID] && (start == 0 && end == 0 || ct >= start && ct <= end) {
			hit = append(hit, d)
		}
	}
	if len(hit) > m.searchCap {
		hit = hit[:m.searchCap] // 超出部分静默截断
	}
	// 每页 20 条
	from := 0
	fmt.Sscanf(pageToken, "%d", &from)
	to := min(from+20, len(hit))
	next := ""
	if to < len(hit) {
		next = fmt.Sprint(to)
	}
	return hit[from:to], next, nil
}

func (m *sharedAPI) BatchQueryMetas(tok string, docs [][2]string) ([]feishuapi.DocMeta, []feishuapi.DocMetaFailed, error) {
	if m.metaErr != nil {
		return nil, nil, m.metaErr
	}
	var ok []feishuapi.DocMeta
	var bad []feishuapi.DocMetaFailed
	for _, d := range docs {
		if code := m.failed[d[0]]; code != 0 {
			bad = append(bad, feishuapi.DocMetaFailed{Token: d[0], Code: code})
			continue
		}
		if meta, has := m.metas[d[0]]; has {
			ok = append(ok, meta)
		}
	}
	return ok, bad, nil
}

// sharedSetup：A 已授权（ut0），B 在员工名单里但没授权；B 把文件夹 fld（含 f1）和文档 d1 共享给了 A。
func sharedSetup(t *testing.T) (*storage.Store, *sharedAPI) {
	old := searchMinInterval
	searchMinInterval = 0
	t.Cleanup(func() { searchMinInterval = old })
	st := copyStore(t, "ou_a")
	st.UpsertIdentity(&storage.Identity{WeComUserID: "wa", FeishuUserID: "ou_a", PersonName: "A"})
	st.UpsertIdentity(&storage.Identity{WeComUserID: "wb", FeishuUserID: "ou_b", PersonName: "B"})
	api := newSharedAPI()
	api.roots["ut0"] = "root_a"
	api.docs = []feishuapi.DocWikiResult{
		{Token: "fld", DocType: "FOLDER", OwnerID: "ou_b"},
		{Token: "d1", DocType: "DOCX", OwnerID: "ou_b"},
		{Token: "wk", DocType: "WIKI", OwnerID: "ou_b"},
	}
	api.metas["fld"] = feishuapi.DocMeta{DocToken: "fld"}
	api.folders["fld"] = []feishuapi.DriveFile{doc("f1", "文件夹里", "ou_b", "100")}
	return st, api
}

// 所有者没授权：搜索发现后，A 拿到文件夹里的文档（单篇共享文档、知识库不记）；之后文件夹里新增、修改不等下次搜索。
func TestSharedFolderFoundBySearch(t *testing.T) {
	st, api := sharedSetup(t)
	c := newUserConn(t, st, api, Config{})

	round(t, c)
	if got := copyPersons(t, st, "f1"); fmt.Sprint(got) != "[ou_a ou_b]" {
		t.Fatalf("f1 副本应给 A 和所有者 B，实际 %v", got)
	}
	if got := copyPersons(t, st, "d1"); len(got) != 0 {
		t.Fatalf("单篇共享文档不应存，实际 %v", got)
	}
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 1 || roots[0].Token != "fld" {
		t.Fatalf("共享表应只记文件夹，实际 %+v", roots)
	}
	searches := api.searches

	// 文件夹里新增 f2、修改 f1：下一轮就同步，不再搜索
	api.folders["fld"] = []feishuapi.DriveFile{doc("f1", "文件夹里", "ou_b", "300"), doc("f2", "新文件", "ou_b", "200")}
	recs := round(t, c)
	if api.searches != searches {
		t.Fatalf("未到搜索周期不应再搜索：%d → %d", searches, api.searches)
	}
	if fmt.Sprint(kinds(recs)) != "[docx:f1 docx:f2]" {
		t.Fatalf("应写入改过的 f1 和新增的 f2，实际 %v", kinds(recs))
	}
}

// 所有者收回 A 的权限（批量查元数据返回无权限）：移出共享表，删掉副本。
func TestSharedFolderRevoked(t *testing.T) {
	st, api := sharedSetup(t)
	c := newUserConn(t, st, api, Config{})
	round(t, c)

	api.failed["fld"] = codeMetaNoPerm
	recs := round(t, c)
	if fmt.Sprint(kinds(recs)) != "[delete:del_f1_ou_a delete:del_f1_ou_b]" {
		t.Fatalf("应删除 A 和所有者的副本，实际 %v", kinds(recs))
	}
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 0 {
		t.Fatalf("应移出共享表，实际 %+v", roots)
	}
}

// 批量查元数据接口报错：按扫描失败处理，不删任何副本，共享表不动。
func TestSharedMetaErrorKeepsCopies(t *testing.T) {
	st, api := sharedSetup(t)
	c := newUserConn(t, st, api, Config{})
	round(t, c)

	api.metaErr = errors.New("飞书错误 code=99991400 msg=rate limit")
	if recs := round(t, c); len(recs) != 0 {
		t.Fatalf("接口报错时不应产出删除，实际 %v", kinds(recs))
	}
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 1 {
		t.Fatalf("共享表应保持不变，实际 %+v", roots)
	}
}

// 名单里有无效 open_id：拆开重搜，跳过它，同批其他人照常搜到。
func TestSharedSearchSkipsInvalidOpenID(t *testing.T) {
	st, api := sharedSetup(t)
	for i := 0; i < 5; i++ {
		st.UpsertIdentity(&storage.Identity{WeComUserID: fmt.Sprint("wx", i), FeishuUserID: fmt.Sprint("ou_x", i)})
	}
	api.invalid["ou_x3"] = true
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 1 {
		t.Fatalf("应跳过无效 open_id 后搜到 B 的共享文件夹，实际 %+v", roots)
	}
}

// 所有者已授权、文件夹只分享给了群：协作者列表里只有群，要靠搜索拿到；搜索名单不含本人。
func TestSharedFolderOfAuthorizedOwnerViaGroup(t *testing.T) {
	st, api := sharedSetup(t)
	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_b", AccessToken: "ut_b", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})
	api.roots["ut_b"] = "root_b"
	api.members = map[string][]feishuapi.PermissionMember{"fld": {{MemberType: "openchat", MemberID: "oc_group"}}}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if got := copyPersons(t, st, "f1"); fmt.Sprint(got) != "[ou_a ou_b]" {
		t.Fatalf("A 应通过搜索拿到 B 分享给群的文件夹，实际 %v", got)
	}
	if roots, _ := st.ListSharedRoots("ou_b"); len(roots) != 0 {
		t.Fatalf("B 不应搜到自己的文档，实际 %+v", roots)
	}
}

// 结果达到 200 上限：按创建时间拆开重搜，拿全。
func TestSharedSearchSplitsAtCap(t *testing.T) {
	st, api := sharedSetup(t)
	api.docs = nil
	for i := 0; i < 450; i++ {
		tok := fmt.Sprint("s", i)
		api.docs = append(api.docs, feishuapi.DocWikiResult{Token: tok, DocType: "FOLDER", OwnerID: "ou_b"})
		api.created[tok] = int64(1000 + i*1000)
		api.metas[tok] = feishuapi.DocMeta{DocToken: tok}
	}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 450 {
		t.Fatalf("应拆分后拿全 450 个，实际 %d", len(roots))
	}
}
