package fastgpt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// 第 6 条：NAS 某个目录这一轮没列出来，不能把其中的文件当成已删除去删 FastGPT 集合。
func TestPushSkipsReconcileWhenListFails(t *testing.T) {
	fs := newFakeNAS()
	root := "/FastGPT-test"
	fs.dirs[root] = nil
	fs.addDir(root, "赵六")
	fs.addDir(root+"/赵六", "飞书")
	fs.addDir(root+"/赵六/飞书", "云文档")
	fs.addFile(root+"/赵六/飞书/云文档", "报告_abc12345.docx", []byte("报告内容"))

	fg, baseURL := newFakeFastGPT(t)
	st, _ := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	task := newPushTask(t, baseURL, fs, st)
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run 1 失败: %v", err)
	}
	if len(fg.collections) != 1 {
		t.Fatalf("首轮应生成 1 个集合, 实际: %d", len(fg.collections))
	}

	fs.listErr = map[string]error{root + "/赵六/飞书/云文档": errors.New("NAS 超时")}
	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run 2 失败: %v", err)
	}
	if len(fg.collections) != 1 {
		t.Fatalf("目录列不出来时不应删除集合，实际剩余: %v", fg.collections)
	}
}
