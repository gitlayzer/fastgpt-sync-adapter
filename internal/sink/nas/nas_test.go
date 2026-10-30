package nas

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/classifier"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/synology"
)

// fakeFS 内存文件系统（UploadBytes/Download）。
type fakeFS struct {
	files map[string][]byte
}

func newFakeFS() *fakeFS { return &fakeFS{files: map[string][]byte{}} }

func (f *fakeFS) UploadBytes(filename, destDir string, data []byte) error {
	f.files[path.Join(destDir, filename)] = data
	return nil
}

func (f *fakeFS) Download(p string) (io.ReadCloser, error) {
	data, ok := f.files[p]
	if !ok {
		return nil, fmt.Errorf("%w: %s", synology.ErrFileNotFound, p)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (f *fakeFS) Delete(p string) error {
	delete(f.files, p)
	return nil
}

type fakeResolver struct{ names map[string]string }

func (r fakeResolver) PersonName(source, id string) string {
	if n, ok := r.names[id]; ok {
		return n
	}
	return id
}
func (r fakeResolver) ResolveWeCom(u string) (*storage.Identity, error) {
	return &storage.Identity{WeComUserID: u, PersonName: r.PersonName("wecom", u)}, nil
}
func (r fakeResolver) ResolveFeishu(u string) (*storage.Identity, error) {
	return &storage.Identity{FeishuUserID: u, PersonName: r.PersonName("feishu", u)}, nil
}

// Managed 测试用：假解析器里所有人都是在职员工。
func (r fakeResolver) Managed(source, id string) (*storage.Identity, bool) {
	name := r.PersonName(source, id)
	if name == "" {
		return nil, false
	}
	return &storage.Identity{WeComUserID: id, PersonName: name, Status: storage.IdentityActive}, true
}

type errorFS struct {
	*fakeFS
	err error
}

func (f *errorFS) Download(p string) (io.ReadCloser, error) {
	return nil, f.err
}

func newTestSink(t *testing.T) (*Sink, *fakeFS, *storage.Store) {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fs := newFakeFS()
	ids := fakeResolver{names: map[string]string{"u1": "张三", "u2": "李四", "u3": "王五"}}
	s := New(Config{RootPath: "/FastGPT-test"}, classifier.New(classifier.Config{}), ids, st)
	s.fs = fs
	return s, fs, st
}

func dmRecord(msgid, from, to, content string) model.Record {
	return model.Record{
		Source: "wecom", Kind: "text", ID: msgid, Timestamp: 1760000000000,
		SenderID: from, OwnerIDs: []string{from, to},
		Body: []byte(fmt.Sprintf(`{"msgid":"%s","action":"send","from":"%s","tolist":["%s"],"msgtime":1760000000000,"msgtype":"text","text":{"content":"%s"}}`, msgid, from, to, content)),
	}
}

func TestSelfChatFileName(t *testing.T) {
	s, fs, _ := newTestSink(t)
	if err := s.WriteRecord(context.Background(), dmRecord("m1", "u1", "u1", "备忘"), nil); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if _, ok := fs.files["/FastGPT-test/张三/企微/群聊记录/与自己的单聊.md"]; !ok {
		t.Fatalf("自己给自己发的消息应写入 与自己的单聊.md，实际文件: %v", fs.files)
	}
}

func TestSingleChatFanOut(t *testing.T) {
	s, fs, _ := newTestSink(t)
	rec := dmRecord("m1", "u1", "u2", "合同看一下")
	if err := s.WriteRecord(context.Background(), rec, nil); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	// 双方目录都应有会话 md；但不再落 messages/*.json 原文
	for _, p := range []string{"张三", "李四"} {
		if _, ok := fs.files["/FastGPT-test/"+p+"/企微/messages/m1.json"]; ok {
			t.Fatalf("%s 不应再写 messages/m1.json（已改为只存正文与附件）", p)
		}
		if _, ok := fs.files["/FastGPT-test/"+p+"/企微/群聊记录/与"+map[string]string{"张三": "李四", "李四": "张三"}[p]+"的单聊.md"]; !ok {
			t.Fatalf("%s 缺会话 md", p)
		}
	}
	md1 := string(fs.files["/FastGPT-test/张三/企微/群聊记录/与李四的单聊.md"])
	md2 := string(fs.files["/FastGPT-test/李四/企微/群聊记录/与张三的单聊.md"])
	if !strings.Contains(md1, "合同看一下") || !strings.Contains(md1, "张三") {
		t.Fatalf("张三 md 内容不对:\n%s", md1)
	}
	if !strings.Contains(md2, "合同看一下") {
		t.Fatalf("李四 md 内容不对:\n%s", md2)
	}
	md1 = strings.TrimPrefix(md1, "\xef\xbb\xbf")
	if !strings.HasPrefix(md1, "# 与李四的单聊") {
		t.Fatalf("md 缺标题:\n%s", md1)
	}
}

func TestMarkdownRollingAppendAndDedup(t *testing.T) {
	s, fs, _ := newTestSink(t)
	r1 := dmRecord("m1", "u1", "u2", "第一条")
	r2 := dmRecord("m2", "u2", "u1", "第二条")
	s.WriteRecord(context.Background(), r1, nil)
	s.WriteRecord(context.Background(), r2, nil)
	// 重复投递 m1（模拟崩溃重拉）：不应重复追加
	s.WriteRecord(context.Background(), r1, nil)

	md := string(fs.files["/FastGPT-test/张三/企微/群聊记录/与李四的单聊.md"])
	if c := strings.Count(md, "第一条"); c != 1 {
		t.Fatalf("去重失败, 第一条出现 %d 次:\n%s", c, md)
	}
	if !strings.Contains(md, "第二条") {
		t.Fatalf("滚动追加失败:\n%s", md)
	}
	// 顺序：第一条在第二条前
	if strings.Index(md, "第一条") > strings.Index(md, "第二条") {
		t.Fatalf("顺序不对:\n%s", md)
	}
}

func TestGroupChatLearnsMembers(t *testing.T) {
	s, fs, _ := newTestSink(t)
	s.cfg.GroupNames = map[string]string{"roomA": "项目组"}
	room := `{"msgid":"%s","action":"send","from":"%s","roomid":"roomA","msgtime":1760000000000,"msgtype":"text","text":{"content":"%s"}}`
	// u1 先发言：此时已知成员只有 u1，只有 u1 目录有
	s.WriteRecord(context.Background(), model.Record{
		Source: "wecom", Kind: "text", ID: "g1", Timestamp: 1760000000000, SenderID: "u1",
		OwnerIDs: []string{"u1"}, Body: []byte(fmt.Sprintf(room, "g1", "u1", "群消息1")),
	}, nil)
	// u2 发言：学到 u2，u2 目录开始有这个群的记录
	s.WriteRecord(context.Background(), model.Record{
		Source: "wecom", Kind: "text", ID: "g2", Timestamp: 1760000001000, SenderID: "u2",
		OwnerIDs: []string{"u2"}, Body: []byte(fmt.Sprintf(room, "g2", "u2", "群消息2")),
	}, nil)

	md1 := string(fs.files["/FastGPT-test/张三/企微/群聊记录/群聊_项目组.md"])
	md2 := string(fs.files["/FastGPT-test/李四/企微/群聊记录/群聊_项目组.md"])
	if !strings.Contains(md1, "群消息1") || !strings.Contains(md1, "群消息2") {
		t.Fatalf("张三应有两条群消息:\n%s", md1)
	}
	// u2 目录只有 g2（g1 时群成员未知 u2）
	if !strings.Contains(md2, "群消息2") || strings.Contains(md2, "群消息1") {
		t.Fatalf("李四应只有群消息2:\n%s", md2)
	}
}

func TestAttachmentArchive(t *testing.T) {
	s, fs, _ := newTestSink(t)
	rec := dmRecord("m9", "u1", "u2", "看这个文件")
	rec.Kind = "file"
	rec.Body = []byte(`{"msgid":"m9","action":"send","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"file","file":{"sdkfileid":"fid9","filename":"采购合同V1.docx","filesize":3}}`)
	rec.Attachments = []model.Attachment{{Name: "采购合同V1.docx", FileID: "fid9", Size: 3}}
	read := func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("PDF")), nil
	}
	if err := s.WriteRecord(context.Background(), rec, read); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	// 附件按日期组织在 <人名>/企微/<YYYY-MM-DD>/ 下（方案 A），保持原始文件名无 msgid 前缀
	dateFolder := time.UnixMilli(rec.Timestamp).Format("2006-01-02")
	for _, p := range []string{"张三", "李四"} {
		att := fmt.Sprintf("/FastGPT-test/%s/企微/%s/采购合同V1.docx", p, dateFolder)
		if string(fs.files[att]) != "PDF" {
			t.Fatalf("%s 附件内容不对（路径应为: %s）", p, att)
		}
		if _, ok := fs.files[att+".meta.json"]; ok {
			t.Fatalf("%s 不应再写 .meta.json 侧边车", p)
		}
	}
}

func TestImageAttachmentArchive(t *testing.T) {
	s, fs, _ := newTestSink(t)
	rec := dmRecord("img101", "u1", "u2", "发了一张图片")
	rec.Kind = "image"
	rec.Body = []byte(`{"msgid":"img101","action":"send","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"image","image":{"sdkfileid":"fid_img101","filesize":2048}}`)
	rec.Attachments = []model.Attachment{{Name: "image_img101.jpg", FileID: "fid_img101", Size: 2048}}
	read := func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("IMAGE_BYTES")), nil
	}
	if err := s.WriteRecord(context.Background(), rec, read); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	// 1760000000000 对应 2025-10-09
	dateFolder := time.UnixMilli(rec.Timestamp).Format("2006-01-02")
	for _, p := range []string{"张三", "李四"} {
		// 图片应存放于 群聊记录/images/<date>/image_img101.jpg
		imgPath := fmt.Sprintf("/FastGPT-test/%s/企微/群聊记录/images/%s/image_img101.jpg", p, dateFolder)
		if string(fs.files[imgPath]) != "IMAGE_BYTES" {
			t.Fatalf("%s 图片未正确落盘至专属路径: %s, 内容: %q", p, imgPath, string(fs.files[imgPath]))
		}
		// 会话 markdown 应包含相对路径出图语法
		convPath := fmt.Sprintf("/FastGPT-test/%s/企微/群聊记录/与%s的单聊.md", p, map[string]string{"张三": "李四", "李四": "张三"}[p])
		md := string(fs.files[convPath])
		expectedImgSyntax := fmt.Sprintf("![图片](images/%s/image_img101.jpg)", dateFolder)
		if !strings.Contains(md, expectedImgSyntax) {
			t.Fatalf("%s 会话记录中应包含相对图片链接 %q, 实际: %s", p, expectedImgSyntax, md)
		}
	}
}

func TestOversizedAttachmentSkipped(t *testing.T) {
	s, fs, _ := newTestSink(t)
	// 设置测试上限为 1MB
	s.cfg.MaxFileSizeMB = 1

	// 1. 已知 Size 超出上限：不拉取、不落盘
	rec := dmRecord("m_big", "u1", "u2", "超大文件")
	rec.Kind = "file"
	rec.Attachments = []model.Attachment{
		{Name: "大视频.mp4", FileID: "fid_big", Size: 500 * 1024 * 1024}, // 500MB
	}
	readCalled := false
	read := func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
		readCalled = true
		return io.NopCloser(strings.NewReader("BIG")), nil
	}
	if err := s.WriteRecord(context.Background(), rec, read); err != nil {
		t.Fatalf("WriteRecord 不应阻断错误: %v", err)
	}
	if readCalled {
		t.Fatalf("超过 1MB 阈值的文件在已知 Size 时不应触发 read")
	}

	dateFolder := time.UnixMilli(rec.Timestamp).Format("2006-01-02")
	attPath := fmt.Sprintf("/FastGPT-test/张三/企微/%s/大视频.mp4", dateFolder)
	if _, ok := fs.files[attPath]; ok {
		t.Fatalf("超大文件不应上传到 NAS")
	}

	// 2. 飞书超大文件流式截断防护
	feishuRec := model.Record{
		Source: "feishu", Kind: "docx", ID: "doc_huge",
		OwnerIDs: []string{"u1"}, SenderID: "u1",
		Attachments: []model.Attachment{
			{Name: "超大导出版.docx", FileID: "docx:doc_huge", Size: 0}, // 未知 Size
		},
	}
	// 返回 2MB 字节流，超过 1MB 限制
	hugeData := strings.Repeat("A", 2*1024*1024)
	readHuge := func(ctx context.Context, a *model.Attachment) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(hugeData)), nil
	}
	if err := s.WriteRecord(context.Background(), feishuRec, readHuge); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	feishuPath := "/FastGPT-test/张三/飞书/云文档/超大导出版.docx"
	if _, ok := fs.files[feishuPath]; ok {
		t.Fatalf("超出流式上限的飞书文件不应落盘 NAS")
	}
}

func TestAppendFileCleansLegacyHTML(t *testing.T) {
	s, fs, _ := newTestSink(t)
	key := "/FastGPT-test/张三/企微/群聊记录/群聊_roomX.md"
	legacyHTML := `<!DOCTYPE html><html><body><p>Sorry, the page you are looking for is not found.</p></body></html>`
	fs.files[key] = []byte(legacyHTML)

	if err := s.appendFile(path.Dir(key), path.Base(key), "- **[2026-10-23 08:51:03] Adele**：一条新消息\n"); err != nil {
		t.Fatalf("appendFile: %v", err)
	}
	got := fs.files[key]
	text := string(got)
	if strings.Contains(text, "<!DOCTYPE") || strings.Contains(text, "Synology Inc.") || strings.Contains(text, "not found") {
		t.Fatalf("旧 HTML 错误页没有清掉: %q", text)
	}
	if !strings.Contains(text, "# 群聊_roomX") || !strings.Contains(text, "一条新消息") {
		t.Fatalf("清理后标题或新消息丢失: %q", text)
	}
}

func TestStripSynologyHTMLPreservesMarkdownTail(t *testing.T) {
	legacy := "<!DOCTYPE html><html>Synology Inc.</html>\n- **[2026-10-23 08:51:03] Adele**：保留这条\n"
	got := string(stripSynologyHTMLPrefix([]byte(legacy)))
	if !strings.Contains(got, "保留这条") || strings.Contains(got, "Synology Inc.") {
		t.Fatalf("清理 HTML 前缀结果不对: %q", got)
	}
}

func TestAppendFileDoesNotOverwriteOnNASReadError(t *testing.T) {
	s, fs, _ := newTestSink(t)
	s.fs = &errorFS{fakeFS: fs, err: fmt.Errorf("NAS connection reset")}
	if err := s.appendFile("/FastGPT-test/张三/企微/群聊记录", "群聊_roomY.md", "新行\n"); err == nil {
		t.Fatal("NAS 读取失败时应停止，不能误当作新文件覆盖")
	}
	if len(fs.files) != 0 {
		t.Fatalf("读取失败后不应上传覆盖: %#v", fs.files)
	}
}

func TestAppendFileTreatsOnlyNotFoundAsNew(t *testing.T) {
	s, fs, _ := newTestSink(t)
	s.fs = &errorFS{fakeFS: fs, err: synology.ErrFileNotFound}
	if err := s.appendFile("/FastGPT-test/张三/企微/群聊记录", "群聊_roomZ.md", "新行\n"); err != nil {
		t.Fatalf("not found 应作为新文件: %v", err)
	}
	got := string(fs.files["/FastGPT-test/张三/企微/群聊记录/群聊_roomZ.md"])
	if !strings.Contains(got, "# 群聊_roomZ") || !strings.Contains(got, "新行") {
		t.Fatalf("新文件内容不正确: %q", got)
	}
}

type countingFS struct {
	*fakeFS
	uploadCount   int
	downloadCount int
}

func (c *countingFS) UploadBytes(filename, destDir string, data []byte) error {
	c.uploadCount++
	return c.fakeFS.UploadBytes(filename, destDir, data)
}

func (c *countingFS) Download(p string) (io.ReadCloser, error) {
	c.downloadCount++
	return c.fakeFS.Download(p)
}

func TestBatchModeCoalescesUploadsAndDownloads(t *testing.T) {
	s, fs, _ := newTestSink(t)
	cfs := &countingFS{fakeFS: fs}
	s.fs = cfs

	// 开启批次模式
	s.SetBatchMode(true)

	// 模拟同一会话连续到达 5 条消息（群聊/单聊向张三李四扇出）
	for i := 1; i <= 5; i++ {
		rec := dmRecord(fmt.Sprintf("m%d", i), "u1", "u2", fmt.Sprintf("这是第%d条消息", i))
		if err := s.WriteRecord(context.Background(), rec, nil); err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
	}

	// 此时尚未 Flush，会话 md 均在内存缓冲中，不应向 NAS 发起上传
	if cfs.uploadCount != 0 {
		t.Fatalf("批次模式下 Flush 前不应发起上传，当前 uploadCount=%d", cfs.uploadCount)
	}

	// 执行 Flush
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}

	// 张三 和 李四 双方目录下各写 1 篇会话 md，因此共计 2 次上传（优化前为 5条 × 2人 = 10 次上传）
	if cfs.uploadCount != 2 {
		t.Fatalf("批次刷写后应只发生 2 次上传（双方各 1 次），实际发生 %d 次", cfs.uploadCount)
	}

	// 验证内容完整性：所有 5 条消息均被合并写入
	md1 := string(fs.files["/FastGPT-test/张三/企微/群聊记录/与李四的单聊.md"])
	for i := 1; i <= 5; i++ {
		expected := fmt.Sprintf("这是第%d条消息", i)
		if !strings.Contains(md1, expected) {
			t.Fatalf("会话内容丢失: %s 未在产出中找到:\n%s", expected, md1)
		}
	}
}

func TestDailyChunkingHeaders(t *testing.T) {
	s, fs, _ := newTestSink(t)
	dir := "/FastGPT-test/张三/企微/群聊记录"
	filename := "与李四的单聊.md"

	// 第一天消息 1
	if err := s.appendFile(dir, filename, "- **[2026-10-25 10:00:00] 张三**：消息1\n"); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	// 第一天消息 2（同天，不应重复产生日期标题）
	if err := s.appendFile(dir, filename, "- **[2026-10-25 11:00:00] 李四**：消息2\n"); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	// 第二天消息 3（跨天，应自动产生 ## 2026-10-26 二级标题）
	if err := s.appendFile(dir, filename, "- **[2026-10-26 09:00:00] 张三**：消息3\n"); err != nil {
		t.Fatalf("append 3: %v", err)
	}

	content := string(fs.files[path.Join(dir, filename)])

	if !strings.HasPrefix(content, "# 与李四的单聊") {
		t.Fatalf("标题缺失或格式错误: %s", content)
	}
	if c := strings.Count(content, "## 2026-10-25"); c != 1 {
		t.Fatalf("第一天日期标题应且仅出现 1 次, 实际出现 %d 次:\n%s", c, content)
	}
	if c := strings.Count(content, "## 2026-10-26"); c != 1 {
		t.Fatalf("第二天日期标题应且仅出现 1 次, 实际出现 %d 次:\n%s", c, content)
	}

	// 验证时间顺序
	idxDay1 := strings.Index(content, "## 2026-10-25")
	idxMsg1 := strings.Index(content, "消息1")
	idxMsg2 := strings.Index(content, "消息2")
	idxDay2 := strings.Index(content, "## 2026-10-26")
	idxMsg3 := strings.Index(content, "消息3")

	if !(idxDay1 < idxMsg1 && idxMsg1 < idxMsg2 && idxMsg2 < idxDay2 && idxDay2 < idxMsg3) {
		t.Fatalf("跨天分段顺序不正确:\n%s", content)
	}
}

func TestNASDeletesFileOnCloudDelete(t *testing.T) {
	s, fs, _ := newTestSink(t)
	// 模拟已存在的文件
	path1 := "/FastGPT-test/张三/飞书/旧文档.docx"
	fs.files[path1] = []byte("内容")

	delRec := model.Record{
		Source:   "feishu",
		Kind:     "delete",
		ID:       "doc_del",
		OwnerIDs: []string{"u1"}, // 张三
		Attachments: []model.Attachment{{
			Name: "旧文档.docx",
		}},
	}

	if err := s.WriteRecord(context.Background(), delRec, nil); err != nil {
		t.Fatalf("WriteRecord delete: %v", err)
	}

	if _, exists := fs.files[path1]; exists {
		t.Fatalf("云端删除后，NAS 对应文件应被清理删除")
	}
}

// fakeExternalNamer 假外部联系人解析器（Display/FileLabel 直接查表）。
type fakeExternalNamer struct {
	display map[string]string
	labels  map[string]string
}

func (n fakeExternalNamer) Display(_ context.Context, id string) string {
	if v, ok := n.display[id]; ok {
		return v
	}
	return "外部:" + id
}

func (n fakeExternalNamer) FileLabel(_ context.Context, id string) string {
	if v, ok := n.labels[id]; ok {
		return v
	}
	return "外部联系人_" + id
}

// emptyNameResolver 内部员工姓名解析统一返回空串（模拟 T1 之后的非纳管员工）。
type emptyNameResolver struct{}

func (emptyNameResolver) PersonName(string, string) string                 { return "" }
func (emptyNameResolver) Managed(string, string) (*storage.Identity, bool) { return nil, false }
func (emptyNameResolver) ResolveWeCom(u string) (*storage.Identity, error) {
	return &storage.Identity{WeComUserID: u}, nil
}
func (emptyNameResolver) ResolveFeishu(u string) (*storage.Identity, error) {
	return &storage.Identity{FeishuUserID: u}, nil
}

// 外部联系人单聊：文件名用固定标签，正文发送人显示"外部:名字@公司"。
func TestSingleChatWithExternalContact(t *testing.T) {
	s, fs, _ := newTestSink(t)
	s.SetExternalNamer(fakeExternalNamer{
		display: map[string]string{"wmABCDEF12": "外部:张三@某公司"},
		labels:  map[string]string{"wmABCDEF12": "张三@某公司_EF12"},
	})

	// 客户发来消息：发送人是外部联系人
	rec := dmRecord("e1", "wmABCDEF12", "u1", "报价单发我一下")
	if err := s.WriteRecord(context.Background(), rec, nil); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	// 外部联系人自身不建员工目录
	for p := range fs.files {
		if strings.Contains(p, "/wmABCDEF12/") || strings.Contains(p, "/外部:") {
			t.Fatalf("不应给外部联系人或显示名建目录: %s", p)
		}
	}

	const mdPath = "/FastGPT-test/张三/企微/群聊记录/与张三@某公司_EF12的单聊.md"
	md, ok := fs.files[mdPath]
	if !ok {
		t.Fatalf("缺会话 md %s，实际文件: %v", mdPath, keysOf(fs.files))
	}
	if !strings.Contains(string(md), "外部:张三@某公司") {
		t.Fatalf("发送人应显示外部联系人名字:\n%s", md)
	}
	if strings.Contains(string(md), "**wmABCDEF12**") {
		t.Fatalf("发送人不应再显示 ID:\n%s", md)
	}
}

// 没注入 ExternalNamer 时保持旧行为：外部联系人仍用 ID。
func TestSingleChatExternalWithoutNamer(t *testing.T) {
	s, fs, _ := newTestSink(t)
	if err := s.WriteRecord(context.Background(), dmRecord("e2", "wmABCDEF12", "u1", "hi"), nil); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	const mdPath = "/FastGPT-test/张三/企微/群聊记录/与wmABCDEF12的单聊.md"
	if _, ok := fs.files[mdPath]; !ok {
		t.Fatalf("未配置客户联系时应沿用 ID 文件名，实际: %v", keysOf(fs.files))
	}
}

// 内部员工 PersonName 返回空串时用 uid 兜底（不能出现"与的单聊.md"和空发送人）。
func TestEmptyPersonNameFallsBackToUID(t *testing.T) {
	st, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()
	fs := newFakeFS()
	s := New(Config{RootPath: "/FastGPT-test"}, classifier.New(classifier.Config{}), emptyNameResolver{}, st)
	s.fs = fs

	if err := s.WriteRecord(context.Background(), dmRecord("m5", "u1", "u2", "正文"), nil); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	const mdPath = "/FastGPT-test/u1/企微/群聊记录/与u2的单聊.md"
	md, ok := fs.files[mdPath]
	if !ok {
		t.Fatalf("空姓名应兜底用 uid，实际: %v", keysOf(fs.files))
	}
	if !strings.Contains(string(md), "] u1**：正文") {
		t.Fatalf("发送人应兜底显示 uid:\n%s", md)
	}
}

// keysOf 便于失败时打印实际文件列表。
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
