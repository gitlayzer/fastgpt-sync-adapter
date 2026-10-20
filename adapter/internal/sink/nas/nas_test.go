package nas

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"fastgpt-sync-adapter/adapter/internal/classifier"
	"fastgpt-sync-adapter/adapter/internal/model"
	"fastgpt-sync-adapter/adapter/internal/storage"
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
		return nil, fmt.Errorf("不存在: %s", p)
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
	// 双方目录都应有原文 + 会话 md
	for _, p := range []string{"张三", "李四"} {
		if _, ok := fs.files["/FastGPT-test/"+p+"/企微/messages/m1.json"]; !ok {
			t.Fatalf("%s 缺 messages/m1.json", p)
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
	for _, p := range []string{"张三", "李四"} {
		att := "/FastGPT-test/" + p + "/企微/合同/m9_采购合同V1.docx"
		if string(fs.files[att]) != "PDF" {
			t.Fatalf("%s 附件内容不对", p)
		}
		meta := att + ".meta.json"
		if !strings.Contains(string(fs.files[meta]), `"category": "合同"`) {
			t.Fatalf("%s meta 不对: %s", p, fs.files[meta])
		}
	}
}
