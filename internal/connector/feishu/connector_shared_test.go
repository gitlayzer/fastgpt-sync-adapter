package feishu

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// sharedAPI 模拟搜索与批量查元数据：docs 是“查看者能搜到的文档”，按创建人、创建时间过滤。
type sharedAPI struct {
	*mockAPI
	docs         []feishuapi.DocWikiResult
	created      map[string]int64             // token → 创建时间（秒），模拟按创建时间过滤
	metas        map[string]feishuapi.DocMeta // token → 元数据（查得到）
	failed       map[string]int               // token → 失败码
	metaErr      error
	invalid      map[string]int             // 不可用的 open_id → 搜索返回的错误码
	searchErr    error                      // 搜索直接报错（非名单问题）
	searchBy     map[string]int             // userToken → 搜索次数
	searchCap    int                        // 单次搜索最多返回条数（模拟 200 上限）
	visible      map[string]map[string]bool // userToken → 能看到的文档（模拟关键词搜索的权限范围；为空时都能看到）
	searches     int
	company      []string // 全公司员工
	companyErr   error
	companyCalls int
	starts       map[string][]int64 // userToken → 每次按创建人搜的起始创建时间
	overPaged    int                // 总条数已达上限还继续翻页的次数
}

func (m *sharedAPI) ListCompanyUserIDs(string) ([]string, error) {
	m.companyCalls++
	return m.company, m.companyErr
}

func newSharedAPI() *sharedAPI {
	return &sharedAPI{mockAPI: newMockAPI(), metas: map[string]feishuapi.DocMeta{},
		failed: map[string]int{}, invalid: map[string]int{}, created: map[string]int64{}, searchBy: map[string]int{}, searchCap: 200}
}

func (m *sharedAPI) SearchDocWiki(tok, query string, ids []string, start, end int64, pageToken string) ([]feishuapi.DocWikiResult, string, int, error) {
	m.searches++
	m.searchBy[tok]++
	if m.searchErr != nil {
		return nil, "", 0, m.searchErr
	}
	if len(ids) > 0 && pageToken == "" {
		if m.starts == nil {
			m.starts = map[string][]int64{}
		}
		m.starts[tok] = append(m.starts[tok], start)
	}
	want := map[string]bool{}
	for _, id := range ids {
		if code := m.invalid[id]; code != 0 {
			return nil, "", 0, &feishuapi.APIError{Code: code, Msg: "bad open_id"}
		}
		want[id] = true
	}
	var hit []feishuapi.DocWikiResult
	for _, d := range m.docs {
		// 按创建人搜：只看名单里的人；按关键词搜（不限创建人）：标题含关键词、且本人能看到（visible 为空时都能看到）
		match := want[d.OwnerID]
		if len(ids) == 0 {
			match = strings.Contains(d.Title, query) && (m.visible == nil || m.visible[tok][d.Token])
		}
		if ct := m.created[d.Token]; match && (start == 0 && end == 0 || ct >= start && ct <= end) {
			hit = append(hit, d)
		}
	}
	if len(hit) > m.searchCap {
		hit = hit[:m.searchCap] // 超出部分静默截断
	}
	if pageToken != "" && len(hit) >= searchSplitAt {
		m.overPaged++
	}
	// 每页 20 条
	from := 0
	fmt.Sscanf(pageToken, "%d", &from)
	to := min(from+20, len(hit))
	next := ""
	if to < len(hit) {
		next = fmt.Sprint(to)
	}
	return hit[from:to], next, len(hit), nil
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
	if got := copyPersons(t, st, "f1"); fmt.Sprint(got) != "[ou_a]" {
		t.Fatalf("f1 副本只应给 A，所有者 B 没授权，实际 %v", got)
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
	if fmt.Sprint(kinds(recs)) != "[delete:del_f1_ou_a]" {
		t.Fatalf("应删除 A 的副本，实际 %v", kinds(recs))
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
	api.invalid["ou_x3"] = codeOpenIDNotExist
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

// 名单里混入别的应用的 open_id（99992361，评审实测）：同样拆开重搜跳过它，其余人的共享文件夹照常发现。
func TestSharedSearchSkipsCrossAppOpenID(t *testing.T) {
	st, api := sharedSetup(t)
	for i := 0; i < 25; i++ { // 名单 27 人，B 和坏 ID 分在不同批
		st.UpsertIdentity(&storage.Identity{WeComUserID: fmt.Sprint("wx", i), FeishuUserID: fmt.Sprint("ou_x", i)})
	}
	api.invalid["ou_x0"] = codeOpenIDCrossApp
	api.invalid["ou_x20"] = codeOpenIDCrossApp
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 1 || roots[0].Token != "fld" {
		t.Fatalf("应跳过跨应用 open_id 后搜到 B 的共享文件夹，实际 %+v", roots)
	}
	searches := api.searches
	round(t, c)
	if api.searches != searches {
		t.Fatalf("跳过坏 ID 后算搜索完整，未到周期不应再搜：%d → %d", searches, api.searches)
	}
}

// 首轮员工授权已过期（没人被搜）：重新授权后下一轮立即搜索，不用等一个周期。
func TestSharedDiscoverAfterReauthorize(t *testing.T) {
	st, api := sharedSetup(t)
	g, _ := st.GetOAuthGrant("feishu", "ou_a")
	g.ExpireAt = time.Now().Unix() - 10
	st.UpsertOAuthGrant(g)
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if api.searches != 0 {
		t.Fatalf("授权过期时不应搜索，实际 %d 次", api.searches)
	}
	g.ExpireAt = time.Now().Unix() + 3600
	st.UpsertOAuthGrant(g)
	round(t, c)
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 1 {
		t.Fatalf("重新授权后下一轮应立即搜到共享文件夹，实际 %+v", roots)
	}
}

// 搜索失败：这个员工本轮停下（不再搜剩下的批次），不算搜完，下一轮重搜；新授权的员工不受别人计时影响。
func TestSharedDiscoverRetryAndPerViewer(t *testing.T) {
	st, api := sharedSetup(t)
	for i := 0; i < 45; i++ { // 名单 47 人：A 要搜 3 批
		st.UpsertIdentity(&storage.Identity{WeComUserID: fmt.Sprint("wx", i), FeishuUserID: fmt.Sprint("ou_x", i)})
	}
	api.searchErr = errors.New("飞书错误 code=1277001 msg=rate limit exceeded")
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if api.searches != 1 {
		t.Fatalf("第一批失败就应停下，实际搜索 %d 次", api.searches)
	}
	api.searchErr = nil
	round(t, c)
	if roots, _ := st.ListSharedRoots("ou_a"); len(roots) != 1 {
		t.Fatalf("下一轮应重搜并找到，实际 %+v", roots)
	}

	// A 刚搜完；C 这时才授权：C 下一轮就搜，A 不重搜
	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_x5", AccessToken: "ut_c", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})
	api.roots["ut_c"] = "root_c"
	before := api.searchBy["ut0"]
	round(t, c)
	if api.searchBy["ut_c"] == 0 || api.searchBy["ut0"] != before {
		t.Fatalf("应只搜新授权的 C：C %d 次，A %d → %d", api.searchBy["ut_c"], before, api.searchBy["ut0"])
	}
}

// 智能纪要：参会人搜到组织者的“智能纪要：”文档就记下并导出；其他单篇文档仍然不要。
// 之后纪要被修改会重新导出，参会人看不到了（970003）就删掉他那份。
func TestSharedSmartNoteFoundBySearch(t *testing.T) {
	st, api := sharedSetup(t)
	api.docs = append(api.docs, feishuapi.DocWikiResult{Token: "sn1", DocType: "DOCX", OwnerID: "ou_b", Title: "智能纪要：周会 2026年5月15日"})
	api.metas["sn1"] = feishuapi.DocMeta{DocToken: "sn1", Title: "智能纪要：周会 2026年5月15日", OwnerID: "ou_b", LatestModifyTime: "100"}
	c := newUserConn(t, st, api, Config{})

	round(t, c)
	if got := copyPersons(t, st, "sn1"); fmt.Sprint(got) != "[ou_a]" {
		t.Fatalf("智能纪要只应给参会人 A，组织者 B 没授权，实际 %v", got)
	}
	if got := copyPersons(t, st, "d1"); len(got) != 0 {
		t.Fatalf("普通单篇文档不应存，实际 %v", got)
	}
	if fmt.Sprint(rootTypes(st, "ou_a")) != "map[fld:folder sn1:docx]" {
		t.Fatalf("共享表应记文件夹和智能纪要，实际 %v", rootTypes(st, "ou_a"))
	}

	api.metas["sn1"] = feishuapi.DocMeta{DocToken: "sn1", Title: "智能纪要：周会 2026年5月15日", OwnerID: "ou_b", LatestModifyTime: "300"}
	if recs := round(t, c); fmt.Sprint(kinds(recs)) != "[docx:sn1]" {
		t.Fatalf("纪要修改后应重新导出，实际 %v", kinds(recs))
	}

	api.failed["sn1"] = codeMetaNoPerm
	recs := round(t, c)
	if fmt.Sprint(kinds(recs)) != "[delete:del_sn1_ou_a]" {
		t.Fatalf("看不到后应删除副本，实际 %v", kinds(recs))
	}
}

// 智能纪要按关键词搜、不限创建人：组织者不在员工名单里、或纪要是本人自己的（不在本人云盘目录里也行）都能记下；
// 标题只是含“智能纪要”、不以“智能纪要：”开头的不要。
func TestSharedSmartNoteNotLimitedToRoster(t *testing.T) {
	st, api := sharedSetup(t)
	api.docs = append(api.docs,
		feishuapi.DocWikiResult{Token: "sn_out", DocType: "DOCX", OwnerID: "ou_outsider", Title: "智能纪要：外部组织者的会 2026年5月15日"},
		feishuapi.DocWikiResult{Token: "sn_own", DocType: "DOCX", OwnerID: "ou_a", Title: "智能纪要:A 自己的会"},
		feishuapi.DocWikiResult{Token: "guide", DocType: "DOCX", OwnerID: "ou_b", Title: "智能纪要使用说明"},
	)
	api.metas["sn_out"] = feishuapi.DocMeta{DocToken: "sn_out", Title: "智能纪要：外部组织者的会 2026年5月15日", OwnerID: "ou_outsider", LatestModifyTime: "100"}
	api.metas["sn_own"] = feishuapi.DocMeta{DocToken: "sn_own", Title: "智能纪要:A 自己的会", OwnerID: "ou_a", LatestModifyTime: "100"}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[fld:folder sn_out:docx sn_own:docx]" {
		t.Fatalf("应记下名单外组织者的纪要和本人的纪要，不记使用说明，实际 %v", got)
	}
	if got := copyPersons(t, st, "sn_own"); fmt.Sprint(got) != "[ou_a]" {
		t.Fatalf("本人的纪要应存进本人目录，实际 %v", got)
	}
}

// 员工名单里只有本人（或名单为空）：文件夹那步跳过，智能纪要照样按关键词搜，本人的纪要能记下；
// 这一轮算没搜完，下一轮还会再搜（名单读失败时不用等一个周期）。
func TestSharedSmartNoteWithoutRoster(t *testing.T) {
	st, api := sharedSetup(t)
	st.UpsertIdentity(&storage.Identity{WeComUserID: "wb", FeishuUserID: "", PersonName: "B"}) // 名单里只剩 A
	api.docs = append(api.docs, feishuapi.DocWikiResult{Token: "sn_own", DocType: "DOCX", OwnerID: "ou_a", Title: "智能纪要：A 自己的会"})
	api.metas["sn_own"] = feishuapi.DocMeta{DocToken: "sn_own", Title: "智能纪要：A 自己的会", OwnerID: "ou_a", LatestModifyTime: "100"}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[sn_own:docx]" {
		t.Fatalf("名单只有本人时也应记下本人的纪要，实际 %v", got)
	}
	before := api.searchBy["ut0"]
	round(t, c)
	if api.searchBy["ut0"] == before {
		t.Fatalf("名单为空时这一轮不算搜完，下一轮应再搜")
	}
}

func rootTypes(st *storage.Store, viewer string) map[string]string {
	types := map[string]string{}
	roots, _ := st.ListSharedRoots(viewer)
	for _, r := range roots {
		types[r.Token] = r.DocType
	}
	return types
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
	if api.overPaged != 0 {
		t.Fatalf("第一页 total 达到上限时应直接拆分、不翻页，实际多翻 %d 页", api.overPaged)
	}
}

// 所有者不在名单里、但在全公司员工里：能搜到他共享的文件夹；名单里的人仍然要搜。
func TestSharedFolderOwnerOutsideRoster(t *testing.T) {
	st, api := sharedSetup(t)
	api.company = []string{"ou_a", "ou_outsider"}
	api.docs = append(api.docs, feishuapi.DocWikiResult{Token: "fld_out", DocType: "FOLDER", OwnerID: "ou_outsider"})
	api.metas["fld_out"] = feishuapi.DocMeta{DocToken: "fld_out"}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[fld:folder fld_out:folder]" {
		t.Fatalf("应记下名单外同事和名单内同事的文件夹，实际 %v", got)
	}
}

// 全公司员工读失败：沿用上次成功的结果；从没读成功过时只用名单，照常搜。一天内不重读。
func TestSharedCompanyListFallback(t *testing.T) {
	st, api := sharedSetup(t)
	api.companyErr = errors.New("飞书错误 code=99991672 msg=Access denied")
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[fld:folder]" {
		t.Fatalf("没开通讯录权限时应只用名单，实际 %v", got)
	}

	api.companyErr = nil
	api.company = []string{"ou_outsider"}
	api.docs = append(api.docs, feishuapi.DocWikiResult{Token: "fld_out", DocType: "FOLDER", OwnerID: "ou_outsider"})
	api.metas["fld_out"] = feishuapi.DocMeta{DocToken: "fld_out"}
	c.discoveredAt["ou_a"] = 0
	round(t, c)
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[fld:folder fld_out:folder]" {
		t.Fatalf("读到全公司员工后应搜到名单外同事的文件夹，实际 %v", got)
	}
	calls := api.companyCalls
	c.discoveredAt["ou_a"] = 0
	round(t, c)
	if api.companyCalls != calls {
		t.Fatalf("一天内不应重读全公司员工：%d → %d", calls, api.companyCalls)
	}

	// 过期后读失败：不能变成空名单
	st.SetCheckpoint(companyUsersKey, `{"at":1,"ids":["ou_outsider"]}`)
	api.companyErr = errors.New("飞书错误 code=131001 msg=rpc fail")
	if got := c.allCreators(time.Now().Unix()); fmt.Sprint(got) != "[ou_outsider ou_a ou_b]" {
		t.Fatalf("读失败时应沿用上次的结果并加上名单，实际 %v", got)
	}
}

// 平时只搜最近新建的；很早以前建、刚共享的文件夹等到全量搜索才找到。
func TestSharedIncrementalAndFull(t *testing.T) {
	st, api := sharedSetup(t)
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if got := api.starts["ut0"]; len(got) != 1 || got[0] != 0 {
		t.Fatalf("第一次应搜全部时间，实际 %v", got)
	}

	now := time.Now().Unix()
	api.docs = append(api.docs,
		feishuapi.DocWikiResult{Token: "fld_old", DocType: "FOLDER", OwnerID: "ou_b"},
		feishuapi.DocWikiResult{Token: "fld_new", DocType: "FOLDER", OwnerID: "ou_b"},
	)
	api.created["fld_old"] = 1000
	api.created["fld_new"] = now - 60
	api.metas["fld_old"] = feishuapi.DocMeta{DocToken: "fld_old"}
	api.metas["fld_new"] = feishuapi.DocMeta{DocToken: "fld_new"}
	c.discoveredAt["ou_a"] -= 7200
	round(t, c)
	if got := api.starts["ut0"]; got[len(got)-1] <= 1 {
		t.Fatalf("没到全量周期时只应搜最近新建的，实际 %v", got)
	}
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[fld:folder fld_new:folder]" {
		t.Fatalf("增量搜索应只找到新建的文件夹，实际 %v", got)
	}

	c.discoveredAt["ou_a"] -= 7200
	c.sharedFullAt["ou_a"] -= int64(sharedFullDefault / time.Second)
	round(t, c)
	if got := rootTypes(st, "ou_a"); fmt.Sprint(got) != "map[fld:folder fld_new:folder fld_old:folder]" {
		t.Fatalf("全量搜索应找到以前建的文件夹，实际 %v", got)
	}
}

// 每轮限量：最久没轮到的优先，人多时后面的人不会一直排不上；搜索失败的人不占住名额。
func TestSharedPerRoundFair(t *testing.T) {
	st, api := sharedSetup(t)
	for i := 1; i <= 3; i++ {
		tok := fmt.Sprint("ut_x", i)
		st.UpsertOAuthGrant(&storage.OAuthGrant{
			Platform: "feishu", ExternalUserID: fmt.Sprint("ou_x", i), AccessToken: tok, RefreshToken: "rt",
			ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
		})
		api.roots[tok] = "root_" + tok
	}
	c := newUserConn(t, st, api, Config{SharedPerRound: 2})
	round(t, c)
	if len(api.searchBy) != 2 {
		t.Fatalf("第一轮应只有 2 人搜索，实际 %v", api.searchBy)
	}
	first := map[string]bool{}
	for tok := range api.searchBy {
		first[tok] = true
	}
	for id := range c.discoveredAt { // 第一批人又到期了
		c.discoveredAt[id] -= 7200
	}
	api.searchBy = map[string]int{}
	round(t, c)
	if len(api.searchBy) != 2 {
		t.Fatalf("第二轮应有 2 人搜索，实际 %v", api.searchBy)
	}
	fresh := 0
	for tok := range api.searchBy {
		if !first[tok] {
			fresh++
		}
	}
	if fresh != 2 {
		t.Fatalf("第二轮应先轮到还没搜过的 2 个人，第一轮 %v，第二轮 %v", first, api.searchBy)
	}
}

// 非纳管员工不占搜索名额：每轮只有 1 个名额时，纳管员工不管排第几都能搜到。
func TestSharedUnmanagedTakesNoSlot(t *testing.T) {
	for _, managed := range []string{"ou_a", "ou_x1", "ou_x2"} {
		st, api := sharedSetup(t)
		tokens := map[string]string{"ou_a": "ut0"}
		for _, id := range []string{"ou_x1", "ou_x2"} {
			tokens[id] = "ut_" + id
			st.UpsertOAuthGrant(&storage.OAuthGrant{
				Platform: "feishu", ExternalUserID: id, AccessToken: tokens[id], RefreshToken: "rt",
				ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
			})
			api.roots[tokens[id]] = "root_" + id
		}
		c := newUserConn(t, st, api, Config{SharedPerRound: 1})
		c.SetManaged(func(id string) bool { return id == managed })
		round(t, c)
		if len(api.searchBy) != 1 {
			t.Fatalf("只应有纳管员工 %s 搜索，实际 %v", managed, api.searchBy)
		}
		if got := rootTypes(st, managed); got["fld"] != "folder" {
			t.Fatalf("纳管员工 %s 应记下共享文件夹，实际 %v", managed, got)
		}
	}
}

// refreshAPI 模拟扫描途中令牌被刷新：第一个员工开始扫描时，后面员工的令牌换成新的。
type refreshAPI struct {
	*sharedAPI
	onRoot func()
	tokens []string
}

func (a *refreshAPI) RootFolderMeta(tok string) (string, error) {
	a.tokens = append(a.tokens, tok)
	if a.onRoot != nil {
		a.onRoot()
		a.onRoot = nil
	}
	return a.sharedAPI.RootFolderMeta(tok)
}

// 一轮可能很长：扫描途中刷新的令牌，轮到这个人时要用新的。
func TestScanUsesFreshToken(t *testing.T) {
	st, shared := sharedSetup(t)
	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_x", AccessToken: "ut_x_old", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400, Status: "authorized",
	})
	shared.roots["ut_x_old"] = "root_x"
	shared.roots["ut_x_new"] = "root_x"
	shared.roots["ut0_new"] = "root_a"
	api := &refreshAPI{sharedAPI: shared}
	api.onRoot = func() { // 不管谁先扫，另一个人的令牌都在他开始前被换掉
		for id, tok := range map[string]string{"ou_a": "ut0_new", "ou_x": "ut_x_new"} {
			if g, _ := st.GetOAuthGrant("feishu", id); g.AccessToken != api.tokens[0] {
				g.AccessToken = tok
				st.UpsertOAuthGrant(g)
			}
		}
	}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	if len(api.tokens) != 2 || (api.tokens[1] != "ut0_new" && api.tokens[1] != "ut_x_new") {
		t.Fatalf("第二个员工应使用扫描途中刷新的令牌，实际 %v", api.tokens)
	}
}
