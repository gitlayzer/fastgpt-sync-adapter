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
		if _, ok := fs.files["/FastGPT-test/"+p+"/企微/conversations/与"+map[string]string{"张三": "李四", "李四": "张三"}[p]+"的单聊.md"]; !ok {
			t.Fatalf("%s 缺会话 md", p)
		}
	}
	md1 := string(fs.files["/FastGPT-test/张三/企微/conversations/与李四的单聊.md"])
	md2 := string(fs.files["/FastGPT-test/李四/企微/conversations/与张三的单聊.md"])
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

	md := string(fs.files["/FastGPT-test/张三/企微/conversations/与李四的单聊.md"])
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

	md1 := string(fs.files["/FastGPT-test/张三/企微/conversations/群聊_roomA.md"])
	md2 := string(fs.files["/FastGPT-test/李四/企微/conversations/群聊_roomA.md"])
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

func TestAppendFileCleansLegacyHTML(t *testing.T) {
	s, fs, _ := newTestSink(t)
	key := "/FastGPT-test/张三/企微/conversations/群聊_roomX.md"
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
	if err := s.appendFile("/FastGPT-test/张三/企微/conversations", "群聊_roomY.md", "新行\n"); err == nil {
		t.Fatal("NAS 读取失败时应停止，不能误当作新文件覆盖")
	}
	if len(fs.files) != 0 {
		t.Fatalf("读取失败后不应上传覆盖: %#v", fs.files)
	}
}

func TestAppendFileTreatsOnlyNotFoundAsNew(t *testing.T) {
	s, fs, _ := newTestSink(t)
	s.fs = &errorFS{fakeFS: fs, err: synology.ErrFileNotFound}
	if err := s.appendFile("/FastGPT-test/张三/企微/conversations", "群聊_roomZ.md", "新行\n"); err != nil {
		t.Fatalf("not found 应作为新文件: %v", err)
	}
	got := string(fs.files["/FastGPT-test/张三/企微/conversations/群聊_roomZ.md"])
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
		md1 := string(fs.files["/FastGPT-test/张三/企微/conversations/与李四的单聊.md"])
		for i := 1; i <= 5; i++ {
			expected := fmt.Sprintf("这是第%d条消息", i)
			if !strings.Contains(md1, expected) {
				t.Fatalf("会话内容丢失: %s 未在产出中找到:\n%s", expected, md1)
			}
		}
	}

func TestDailyChunkingHeaders(t *testing.T) {
	s, fs, _ := newTestSink(t)
	dir := "/FastGPT-test/张三/企微/conversations"
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


