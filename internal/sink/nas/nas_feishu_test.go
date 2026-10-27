package nas

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
)

// 第 2 条：文档改名后，新文件写成功再删除旧文件。
func TestFeishuRenameDeletesPrevFile(t *testing.T) {
	s, fs, _ := newTestSink(t)
	oldPath := "/FastGPT-test/张三/飞书/云文档/旧标题_Rename01.docx"
	fs.files[oldPath] = []byte("旧内容")

	rec := model.Record{
		Source: "feishu", Kind: "docx", ID: "docRename01", OwnerIDs: []string{"u1"}, SenderID: "u1",
		Attachments: []model.Attachment{{Name: "新标题_Rename01.docx", FileID: "docx:docRename01", PrevName: "旧标题_Rename01.docx"}},
	}
	read := func(context.Context, *model.Attachment) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("新内容")), nil
	}
	if err := s.WriteRecord(context.Background(), rec, read); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if _, ok := fs.files["/FastGPT-test/张三/飞书/云文档/新标题_Rename01.docx"]; !ok {
		t.Fatalf("新文件应写入")
	}
	if _, ok := fs.files[oldPath]; ok {
		t.Fatalf("旧文件应被删除")
	}
}

type failDeleteFS struct{ *fakeFS }

func (f *failDeleteFS) Delete(string) error { return errors.New("NAS 删除失败") }

// 第 4 条：NAS 删除失败要返回错误（不 Ack，下一轮重试），不能吞掉。
func TestFeishuDeleteFailureReturnsError(t *testing.T) {
	s, fs, _ := newTestSink(t)
	s.fs = &failDeleteFS{fakeFS: fs}
	rec := model.Record{
		Source: "feishu", Kind: "delete", ID: "del_d1_u1", OwnerIDs: []string{"u1"},
		Attachments: []model.Attachment{{Name: "文档_d1.docx"}},
	}
	if err := s.WriteRecord(context.Background(), rec, nil); err == nil {
		t.Fatalf("删除失败应返回错误")
	}
}
