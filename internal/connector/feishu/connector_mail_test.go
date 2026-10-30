package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// mailAPI 邮箱假接口：按文件夹分页返回邮件 ID，记录各接口调用次数。
type mailAPI struct {
	*mockAPI
	folders    []feishuapi.MailFolder
	foldersErr error
	pages      map[string][][]string // 文件夹 → 每页的邮件 ID（从新到旧）
	listErr    map[string]error
	msgs       map[string]*feishuapi.MailMessage
	getErr     map[string]error
	inlineErr  error
	listCalls  map[string]int
	getCalls   int
}

func newMailAPI() *mailAPI {
	return &mailAPI{
		mockAPI:   newMockAPI(),
		pages:     map[string][][]string{},
		listErr:   map[string]error{},
		msgs:      map[string]*feishuapi.MailMessage{},
		getErr:    map[string]error{},
		listCalls: map[string]int{},
	}
}

func (m *mailAPI) ListMailFolders(string) ([]feishuapi.MailFolder, error) {
	return m.folders, m.foldersErr
}

func (m *mailAPI) ListMailboxMessages(_, folderID, pageToken string) ([]string, string, bool, error) {
	m.listCalls[folderID]++
	if err := m.listErr[folderID]; err != nil {
		return nil, "", false, err
	}
	pages := m.pages[folderID]
	i := 0
	if pageToken != "" {
		i, _ = strconv.Atoi(pageToken)
	}
	if i >= len(pages) {
		return nil, "", false, nil
	}
	if i+1 < len(pages) {
		return pages[i], strconv.Itoa(i + 1), true, nil
	}
	return pages[i], "", false, nil
}

func (m *mailAPI) GetMailMessage(_, msgID string) (*feishuapi.MailMessage, error) {
	m.getCalls++
	if err := m.getErr[msgID]; err != nil {
		return nil, err
	}
	return m.msgs[msgID], nil
}

func (m *mailAPI) DownloadMailAttachment(_, _, attachmentID string) (io.ReadCloser, error) {
	if m.inlineErr != nil && strings.HasPrefix(attachmentID, "img") {
		return nil, m.inlineErr
	}
	return io.NopCloser(strings.NewReader("\x89PNG\r\n\x1a\nfake")), nil
}

func (m *mailAPI) addMail(id, subject string) {
	m.msgs[id] = &feishuapi.MailMessage{MessageID: id, Subject: subject, CreatedTime: 1700000000000, BodyPlain: subject + " 正文"}
}

func mailRecs(recs []model.Record) []model.Record {
	var out []model.Record
	for _, r := range recs {
		if r.Kind == "mail" {
			out = append(out, r)
		}
	}
	return out
}

// 没有附件的邮件也要存：只产出正文 .html，内容含邮件头和转义后的纯文本正文。
func TestMailBodyOnlyMessage(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"msgAAAABBBB=="}}
	api.msgs["msgAAAABBBB=="] = &feishuapi.MailMessage{
		Subject: "周报 a/b", HeadFrom: "boss@x.com", FromName: "老板", CreatedTime: 1700000000000,
		To: []feishuapi.MailAddress{{Address: "a@x.com", Name: "A"}}, BodyPlain: "见附件 <b>不是标签</b>",
	}
	c := newUserConn(t, st, api, Config{})
	got := mailRecs(round(t, c))
	if len(got) != 1 || len(got[0].Attachments) != 1 {
		t.Fatalf("应产出 1 封邮件、只有正文，实际 %+v", got)
	}
	a := got[0].Attachments[0]
	if a.Name != "2023-11-15_周报 a_b_AAAABBBB.html" || a.FileID != "mailbody:msgAAAABBBB==" || a.UserID != "ou_a" {
		t.Fatalf("正文附件属性不符: %+v", a)
	}
	rc, err := c.FetchAttachment(context.Background(), &a)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	body := string(data)
	for _, want := range []string{"<title>周报 a/b</title>", "老板 &lt;boss@x.com&gt;", "A &lt;a@x.com&gt;", "2023-11-15 06:13:20",
		"<pre>见附件 &lt;b&gt;不是标签&lt;/b&gt;</pre>", "default-src 'none'; img-src data:;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("正文缺少 %q:\n%s", want, body)
		}
	}
	if api.getCalls != 1 {
		t.Fatalf("生成正文应复用扫描时取到的详情，详情接口应只调 1 次，实际 %d", api.getCalls)
	}
}

// 内联图片：正文里的 cid: 引用换成 data: 地址；图片下载失败时整体报错（下一轮重试）。
func TestMailBodyInlineImages(t *testing.T) {
	msg := &feishuapi.MailMessage{
		Subject:  "带图",
		BodyHTML: `<p>看图</p><img src="cid:c1">`,
		Attachments: []feishuapi.MailAttachment{
			{ID: "img1", Name: "image.png", IsInline: true, CID: "c1"},
			{ID: "f1", Name: "报价.xlsx"},
		},
	}
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.msgs["m1"] = msg
	c := newUserConn(t, st, api, Config{})
	rc, err := c.FetchAttachment(context.Background(), &model.Attachment{FileID: "mailbody:m1", UserID: "ou_a"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if s := string(data); strings.Contains(s, "cid:c1") || !strings.Contains(s, `src="data:image/png;base64,`) || !strings.Contains(s, "报价.xlsx") || strings.Contains(s, "image.png") {
		t.Fatalf("内联图片应嵌入、附件列表只列非内联附件:\n%s", s)
	}

	api.inlineErr = errors.New("expired")
	if _, err := c.FetchAttachment(context.Background(), &model.Attachment{FileID: "mailbody:m1", UserID: "ou_a"}); err == nil {
		t.Fatal("内联图片下载失败应返回错误")
	}
}

// 已存过的邮件不再取详情；补齐后只翻到“整页都存过”为止。
func TestMailSkipsStoredAndStopsEarly(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"m4", "m3"}, {"m2", "m1"}}
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		api.addMail(id, "主题"+id)
	}
	c := newUserConn(t, st, api, Config{})

	if got := mailRecs(round(t, c)); len(got) != 4 || api.getCalls != 4 || api.listCalls["INBOX"] != 2 {
		t.Fatalf("首轮应翻完 2 页、存 4 封，实际 %d 封，详情 %d 次，列表 %d 次", len(got), api.getCalls, api.listCalls["INBOX"])
	}
	// 第 2 轮：完整翻一遍确认没有漏存 → 记为已补齐，不取详情
	if got := mailRecs(round(t, c)); len(got) != 0 || api.getCalls != 4 || api.listCalls["INBOX"] != 4 {
		t.Fatalf("第 2 轮应只核对不取详情，实际 %d 封，详情 %d 次，列表 %d 次", len(got), api.getCalls, api.listCalls["INBOX"])
	}
	if st.MailFolderScannedAt("ou_a", "INBOX") == 0 {
		t.Fatal("完整核对通过后应记录时间")
	}
	// 第 3 轮：已补齐，第 1 页全存过就停
	if got := mailRecs(round(t, c)); len(got) != 0 || api.listCalls["INBOX"] != 5 {
		t.Fatalf("第 3 轮应只翻 1 页，实际 %d 封，列表 %d 次", len(got), api.listCalls["INBOX"])
	}
	// 新邮件进来：第 1 页有没存过的，继续翻到整页都存过
	api.pages["INBOX"] = [][]string{{"m6", "m5"}, {"m4", "m3"}, {"m2", "m1"}}
	api.addMail("m5", "新5")
	api.addMail("m6", "新6")
	if got := mailRecs(round(t, c)); len(got) != 2 || api.listCalls["INBOX"] != 7 {
		t.Fatalf("应存 2 封新邮件、翻 2 页，实际 %d 封，列表 %d 次", len(got), api.listCalls["INBOX"])
	}
}

// 每轮最多处理 MailMaxPerRound 封，剩下的下一轮接着处理。
func TestMailMaxPerRound(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"m3", "m2", "m1"}}
	for _, id := range []string{"m1", "m2", "m3"} {
		api.addMail(id, id)
	}
	c := newUserConn(t, st, api, Config{MailMaxPerRound: 2})
	if got := mailRecs(round(t, c)); len(got) != 2 || got[0].ID != "mail_m3" || got[1].ID != "mail_m2" {
		t.Fatalf("首轮应先处理最新的 2 封，实际 %v", kinds(got))
	}
	if got := mailRecs(round(t, c)); len(got) != 1 || got[0].ID != "mail_m1" {
		t.Fatalf("第 2 轮应处理剩下的 1 封，实际 %v", kinds(got))
	}
}

// 扫哪些文件夹：收件箱、已发送 + 自建文件夹；已归档、草稿、垃圾、已删除不扫。列文件夹失败时只扫收件箱和已发送。
func TestMailFolders(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.folders = []feishuapi.MailFolder{
		{ID: "INBOX", Type: 1}, {ID: "ARCHIVED", Type: 1}, {ID: "DRAFT", Type: 1}, {ID: "TRASH", Type: 1}, {ID: "SPAM", Type: 1}, {ID: "77", Type: 2, Name: "客户"},
	}
	api.pages["INBOX"] = [][]string{{"i1"}}
	api.pages["SENT"] = [][]string{{"s1"}}
	api.pages["ARCHIVED"] = [][]string{{"a1"}}
	api.pages["77"] = [][]string{{"c1"}}
	for _, id := range []string{"i1", "s1", "a1", "c1"} {
		api.addMail(id, id)
	}
	c := newUserConn(t, st, api, Config{})
	got := mailRecs(round(t, c))
	if fmt.Sprint(kinds(got)) != "[mail:mail_c1 mail:mail_i1 mail:mail_s1]" {
		t.Fatalf("应存收件箱、已发送、自建文件夹的邮件，实际 %v", kinds(got))
	}
	for _, f := range []string{"ARCHIVED", "DRAFT", "TRASH", "SPAM"} {
		if api.listCalls[f] != 0 {
			t.Fatalf("不应扫文件夹 %s，调用次数 %v", f, api.listCalls)
		}
	}

	api.foldersErr = errors.New("99991679 no permission")
	api.listCalls = map[string]int{}
	round(t, c)
	if api.listCalls["77"] != 0 || api.listCalls["INBOX"] == 0 || api.listCalls["SENT"] == 0 {
		t.Fatalf("列文件夹失败时应只扫收件箱和已发送，实际 %v", api.listCalls)
	}
}

// 列表出错：这个文件夹本轮不记为已补齐，下一轮继续完整翻；已拿到的邮件照常处理。
func TestMailListErrorNotMarked(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"m1"}}
	api.addMail("m1", "m1")
	api.listErr["SENT"] = errors.New("5000 internal error")
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	round(t, c)
	if st.MailFolderScannedAt("ou_a", "SENT") != 0 {
		t.Fatal("列表出错的文件夹不应记为已补齐")
	}
	if st.MailFolderScannedAt("ou_a", "INBOX") == 0 {
		t.Fatal("其他文件夹不受影响")
	}
}

// 正文生成：主题缺失（旧授权没有主题权限）时用“无主题”，文件名里的主题过长时截断。
func TestMailRecordNaming(t *testing.T) {
	r := mailToRecord("id12345678", &feishuapi.MailMessage{CreatedTime: 1700000000000}, "ou_a")
	if n := r.Attachments[0].Name; n != "2023-11-15_无主题_12345678.html" {
		t.Fatalf("无主题命名不符: %q", n)
	}
	long := strings.Repeat("长", 80)
	r = mailToRecord("id12345678", &feishuapi.MailMessage{Subject: long, CreatedTime: 1700000000000}, "ou_a")
	if n := r.Attachments[0].Name; n != "2023-11-15_"+strings.Repeat("长", 60)+"_12345678.html" {
		t.Fatalf("长主题应截到 60 字: %q", n)
	}
}

// 发给自己的邮件在收件箱和已发送各有一份（message_id 不同、RFC 邮件 ID 相同）：只存一份，两份都记为已处理。
func TestMailDedupeBySMTPID(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"in1"}}
	api.pages["SENT"] = [][]string{{"se1"}}
	api.addMail("in1", "发给自己")
	api.addMail("se1", "发给自己")
	api.msgs["in1"].SMTPID = "<x@mail>"
	api.msgs["se1"].SMTPID = "<x@mail>"
	c := newUserConn(t, st, api, Config{})
	if got := mailRecs(round(t, c)); len(got) != 1 {
		t.Fatalf("同一封邮件应只存一份，实际 %v", kinds(got))
	}
	if !st.MailSeen("ou_a", "in1") || !st.MailSeen("ou_a", "se1") {
		t.Fatal("两份拷贝都应记为已处理")
	}
	calls := api.getCalls
	if got := mailRecs(round(t, c)); len(got) != 0 || api.getCalls != calls {
		t.Fatalf("第 2 轮不应再产出或取详情，实际 %d 封、详情 %d 次", len(got), api.getCalls-calls)
	}

	// 跨轮：第 1 轮只处理了收件箱那份，第 2 轮遇到已发送那份 → 认出是重复，不再存
	st2 := copyStore(t, "ou_a")
	c2 := newUserConn(t, st2, api, Config{MailMaxPerRound: 1})
	if got := mailRecs(round(t, c2)); len(got) != 1 || got[0].ID != "mail_in1" {
		t.Fatalf("首轮应只存收件箱那份，实际 %v", kinds(got))
	}
	if got := mailRecs(round(t, c2)); len(got) != 0 || !st2.MailSeen("ou_a", "se1") {
		t.Fatalf("第 2 轮应认出重复拷贝、不再存，实际 %v", kinds(got))
	}
}

// 写入失败（没 Ack）：不记为已处理，下一轮重新处理。
func TestMailNotSeenUntilAck(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"m1"}}
	api.addMail("m1", "m1")
	c := newUserConn(t, st, api, Config{})
	recs, _, err := c.Fetch(context.Background(), "", 1000)
	if err != nil || len(mailRecs(recs)) != 1 {
		t.Fatalf("首轮应产出 1 封，实际 %v %v", err, kinds(recs))
	}
	if st.MailSeen("ou_a", "m1") {
		t.Fatal("没 Ack 前不应记为已处理")
	}
	if got := mailRecs(round(t, c)); len(got) != 1 || got[0].ID != "mail_m1" {
		t.Fatalf("没 Ack 的邮件下一轮应重新产出，实际 %v", kinds(got))
	}
}

// 旧数据：邮件已有只含附件的副本（feishu_doc_copy 里文件名是附件名），也要补存正文。
func TestMailOldAttachmentOnlyCopyGetsBody(t *testing.T) {
	st := copyStore(t, "ou_a")
	if err := st.UpsertCopy(storage.DocCopy{Token: "mail_m1", PersonID: "ou_a", Via: viaOwner, EditTime: 1700000000, FileName: "合同_m1.pdf", OwnerID: "ou_a", Kind: "mail"}); err != nil {
		t.Fatal(err)
	}
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"m1"}}
	api.addMail("m1", "旧邮件")
	api.msgs["m1"].Attachments = []feishuapi.MailAttachment{{ID: "a1", Name: "合同.pdf"}}
	c := newUserConn(t, st, api, Config{})
	got := mailRecs(round(t, c))
	if len(got) != 1 || len(got[0].Attachments) != 2 || !strings.HasSuffix(got[0].Attachments[0].Name, ".html") {
		t.Fatalf("旧副本只有附件时应重新产出（含正文），实际 %+v", got)
	}
	if !st.MailSeen("ou_a", "m1") {
		t.Fatal("写入后应记为已处理")
	}
}

// 补历史时待处理的已够本轮上限，就不再往后翻页；下一轮接着翻，最终全部存完并记为已补齐。
func TestMailStopsListingWhenEnough(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"m6", "m5"}, {"m4", "m3"}, {"m2", "m1"}}
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		api.addMail(id, id)
	}
	c := newUserConn(t, st, api, Config{MailMaxPerRound: 2})
	if got := mailRecs(round(t, c)); len(got) != 2 || api.listCalls["INBOX"] != 1 {
		t.Fatalf("首轮应只翻 1 页、存 2 封，实际 %d 封，列表 %d 次", len(got), api.listCalls["INBOX"])
	}
	total := 2
	for i := 0; i < 4; i++ {
		total += len(mailRecs(round(t, c)))
	}
	if total != 6 || st.MailFolderScannedAt("ou_a", "INBOX") == 0 {
		t.Fatalf("几轮后应全部存完并记为已补齐，实际存 %d 封", total)
	}
}

// 收件箱补历史占满上限时，已发送里的邮件每轮也要处理到（各文件夹轮流取）。
func TestMailFoldersShareQuota(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.pages["INBOX"] = [][]string{{"i4", "i3", "i2", "i1"}}
	api.pages["SENT"] = [][]string{{"s1"}}
	for _, id := range []string{"i1", "i2", "i3", "i4", "s1"} {
		api.addMail(id, id)
	}
	c := newUserConn(t, st, api, Config{MailMaxPerRound: 2})
	got := mailRecs(round(t, c))
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if len(got) != 2 || !ids["mail_i4"] || !ids["mail_s1"] {
		t.Fatalf("首轮应收件箱、已发送各处理 1 封，实际 %v", kinds(got))
	}
}

// mailDeleteSetup 邮箱里有全部系统文件夹和一个自建文件夹，ids 都在收件箱里，已存过一轮。
func mailDeleteSetup(t *testing.T, ids ...string) (*storage.Store, *mailAPI, *Connector) {
	t.Helper()
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	for _, f := range []string{"INBOX", "SENT", "ARCHIVED", "TRASH", "DRAFT", "SPAM"} {
		api.folders = append(api.folders, feishuapi.MailFolder{ID: f, Type: 1})
	}
	api.folders = append(api.folders, feishuapi.MailFolder{ID: "77", Type: 2})
	for _, id := range ids {
		api.pages["INBOX"] = append(api.pages["INBOX"], []string{id})
		api.addMail(id, id)
	}
	c := newUserConn(t, st, api, Config{})
	if got := mailRecs(round(t, c)); len(got) != len(ids) {
		t.Fatalf("应先存下 %d 封，实际 %v", len(ids), kinds(got))
	}
	return st, api, c
}

// move 把邮件移到另一个文件夹；to 为空表示彻底删除。
func (m *mailAPI) move(id, to string) {
	for f, pages := range m.pages {
		var kept [][]string
		for _, pg := range pages {
			var rest []string
			for _, x := range pg {
				if x != id {
					rest = append(rest, x)
				}
			}
			if len(rest) > 0 {
				kept = append(kept, rest)
			}
		}
		m.pages[f] = kept
	}
	if to != "" {
		m.pages[to] = append(m.pages[to], []string{id})
	}
}

func verifyDue(st *storage.Store) { st.SetCheckpoint("feishu_mail_verify:ou_a", "0") }

func deleteRecs(recs []model.Record) []model.Record {
	var out []model.Record
	for _, r := range recs {
		if r.Kind == "delete" {
			out = append(out, r)
		}
	}
	return out
}

// 邮件移到已删除：正文和附件都删；从已删除恢复后重新存。
func TestMailTrashedDeletesAllFiles(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.folders = []feishuapi.MailFolder{{ID: "INBOX", Type: 1}, {ID: "TRASH", Type: 1}}
	api.pages["INBOX"] = [][]string{{"m1"}, {"m2"}}
	api.addMail("m1", "m1")
	api.addMail("m2", "m2")
	api.msgs["m1"].Attachments = []feishuapi.MailAttachment{{ID: "a1", Name: "合同.pdf"}}
	c := newUserConn(t, st, api, Config{})
	stored := mailRecs(round(t, c))
	if len(stored) != 2 {
		t.Fatalf("应存下 2 封，实际 %v", kinds(stored))
	}
	api.move("m1", "TRASH")
	dels := deleteRecs(round(t, c))
	if len(dels) != 1 || dels[0].ID != "del_mail_m1_ou_a" || len(dels[0].Attachments) != 2 {
		t.Fatalf("应删除 m1 的正文和附件，实际 %+v", dels)
	}
	names := []string{dels[0].Attachments[0].Name, dels[0].Attachments[1].Name}
	sort.Strings(names)
	if fmt.Sprint(names) != "[2023-11-15_m1_m1.html 合同_m1.pdf]" {
		t.Fatalf("要删的文件不对: %v", names)
	}
	if got := copyPersons(t, st, "mail_m1"); len(got) != 0 {
		t.Fatalf("副本记录应清掉，实际 %v", got)
	}
	if st.MailSeen("ou_a", "m1") || len(st.ListMailFiles("ou_a", "mail_m1")) != 0 {
		t.Fatalf("已处理记录和文件名记录应清掉")
	}
	if got := copyPersons(t, st, "mail_m2"); fmt.Sprint(got) != "[ou_a]" {
		t.Fatalf("没删的邮件不受影响，实际 %v", got)
	}

	// 恢复的邮件回到原来的时间位置，平时翻不到那一页，由每天一次的完整核对补上
	api.move("m1", "INBOX")
	st.SetMailFolderScannedAt("ou_a", "INBOX", 0)
	if got := mailRecs(round(t, c)); len(got) != 1 || got[0].ID != "mail_m1" {
		t.Fatalf("恢复后应重新存，实际 %v", kinds(got))
	}
}

// 彻底删除：详情接口明确返回不存在才算删除。
func TestMailPurgedDeleted(t *testing.T) {
	_, api, c := mailDeleteSetup(t, "m1", "m2")
	api.move("m1", "")
	api.getErr["m1"] = &feishuapi.APIError{Code: codeMailNotFound, Msg: "user message not found"}
	if dels := deleteRecs(round(t, c)); len(dels) != 1 || dels[0].ID != "del_mail_m1_ou_a" {
		t.Fatalf("彻底删除的邮件应删除，实际 %v", kinds(dels))
	}
}

// 移到已归档、垃圾邮件、自建文件夹都不算删除，也不用查详情。
func TestMailMovedNotDeleted(t *testing.T) {
	_, api, c := mailDeleteSetup(t, "m1", "m2", "m3")
	api.move("m1", "ARCHIVED")
	api.move("m2", "SPAM")
	api.move("m3", "77")
	calls := api.getCalls
	if recs := round(t, c); len(deleteRecs(recs)) != 0 || api.getCalls != calls {
		t.Fatalf("移动不算删除，实际 %v，详情 %d → %d", kinds(recs), calls, api.getCalls)
	}
}

// 发给自己的邮件有两份拷贝：只删一份时 NAS 不动，两份都删了才删。
func TestMailAllCopiesMustBeGone(t *testing.T) {
	st := copyStore(t, "ou_a")
	api := newMailAPI()
	api.folders = []feishuapi.MailFolder{{ID: "INBOX", Type: 1}, {ID: "SENT", Type: 1}, {ID: "TRASH", Type: 1}}
	api.pages["INBOX"] = [][]string{{"i1"}}
	api.pages["SENT"] = [][]string{{"s1"}}
	api.addMail("i1", "同一封")
	api.addMail("s1", "同一封")
	api.msgs["i1"].SMTPID = "smtp-1"
	api.msgs["s1"].SMTPID = "smtp-1"
	c := newUserConn(t, st, api, Config{})
	if got := mailRecs(round(t, c)); len(got) != 1 {
		t.Fatalf("两份拷贝只存一份，实际 %v", kinds(got))
	}
	api.move("i1", "TRASH")
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("已发送里还有一份，不应删除，实际 %v", kinds(dels))
	}
	api.move("s1", "TRASH")
	verifyDue(st)
	if dels := deleteRecs(round(t, c)); len(dels) != 1 {
		t.Fatalf("两份都删了应删除，实际 %v", kinds(dels))
	}
}

// 不能确认时不删：详情报其他错误、或邮件还查得到；文件夹列表失败时本轮不核对，下一轮重试。
func TestMailDeleteNeedsConfirmation(t *testing.T) {
	st, api, c := mailDeleteSetup(t, "m1", "m2", "m3")
	api.move("m1", "")
	api.getErr["m1"] = errors.New("飞书错误 code=1277001 msg=rate limit exceeded")
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("详情报其他错误时不应删除，实际 %v", kinds(dels))
	}
	delete(api.getErr, "m1")
	verifyDue(st)
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("详情还查得到时不应删除，实际 %v", kinds(dels))
	}

	api.getErr["m1"] = &feishuapi.APIError{Code: codeMailNotFound}
	verifyDue(st)
	api.listErr["SENT"] = errors.New("飞书错误 code=1277001 msg=rate limit exceeded")
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("文件夹列表失败时不应删除，实际 %v", kinds(dels))
	}
	delete(api.listErr, "SENT")
	if dels := deleteRecs(round(t, c)); len(dels) != 1 {
		t.Fatalf("列表恢复后下一轮应继续核对，实际 %v", kinds(dels))
	}
}

// 疑似删除超过一半时当作接口异常，不删。
func TestMailDeleteFuse(t *testing.T) {
	_, api, c := mailDeleteSetup(t, "m1", "m2", "m3", "m4", "m5", "m6")
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		api.move(id, "TRASH")
	}
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("超过一半时应熔断，实际 %v", kinds(dels))
	}
}

// 所有文件夹都是空的（没开通邮箱时列表接口返回空）：分不清是不是全删了，不删。
func TestMailEmptyMailboxNotDeleted(t *testing.T) {
	_, api, c := mailDeleteSetup(t, "m1")
	api.move("m1", "")
	api.getErr["m1"] = &feishuapi.APIError{Code: codeMailNotFound}
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("文件夹全空时不应删除，实际 %v", kinds(dels))
	}
}

// 每天只核对一次，不到周期不再翻已归档等文件夹。
func TestMailVerifyDaily(t *testing.T) {
	_, api, c := mailDeleteSetup(t, "m1")
	round(t, c)
	calls := api.listCalls["ARCHIVED"]
	if calls == 0 {
		t.Fatalf("核对时应翻已归档")
	}
	round(t, c)
	if api.listCalls["ARCHIVED"] != calls {
		t.Fatalf("不到周期不应再核对：%d → %d", calls, api.listCalls["ARCHIVED"])
	}
}

// failRootAPI 模拟这个员工本轮云空间扫描失败。
type failRootAPI struct {
	*mailAPI
	fail bool
}

func (a *failRootAPI) RootFolderMeta(tok string) (string, error) {
	if a.fail {
		return "", errors.New("飞书错误 code=131001 msg=rpc fail")
	}
	return a.mailAPI.RootFolderMeta(tok)
}

// 员工本轮扫描失败时不核对邮件删除，恢复后下一轮再核对。
func TestMailVerifySkippedWhenScanFailed(t *testing.T) {
	st := copyStore(t, "ou_a")
	inner := newMailAPI()
	inner.folders = []feishuapi.MailFolder{{ID: "INBOX", Type: 1}, {ID: "TRASH", Type: 1}}
	inner.pages["INBOX"] = [][]string{{"m1"}, {"m2"}}
	inner.addMail("m1", "m1")
	inner.addMail("m2", "m2")
	api := &failRootAPI{mailAPI: inner}
	c := newUserConn(t, st, api, Config{})
	round(t, c)
	inner.move("m1", "TRASH")
	api.fail = true
	if dels := deleteRecs(round(t, c)); len(dels) != 0 {
		t.Fatalf("扫描失败时不应删除，实际 %v", kinds(dels))
	}
	api.fail = false
	if dels := deleteRecs(round(t, c)); len(dels) != 1 {
		t.Fatalf("恢复后应继续核对，实际 %v", kinds(dels))
	}
}
