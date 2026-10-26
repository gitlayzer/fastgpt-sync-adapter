package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) *Msg {
	t.Helper()
	m, err := Parse(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func TestConvKeyAndFileName(t *testing.T) {
	names := map[string]string{"u1": "张三", "u2": "李四"}
	resolve := func(u string) string {
		if n, ok := names[u]; ok {
			return n
		}
		return u
	}

	dm := mustParse(t, `{"msgid":"m1","action":"send","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"text","text":{"content":"你好"}}`)
	if got := dm.ConvKey(); got != "dm_u1_u2" {
		t.Fatalf("单聊 ConvKey: %q", got)
	}
	// 双方视角文件名互为对方
	if got := dm.ConvFileName("u1", resolve); got != "与李四的单聊.md" {
		t.Fatalf("u1 视角: %q", got)
	}
	if got := dm.ConvFileName("u2", resolve); got != "与张三的单聊.md" {
		t.Fatalf("u2 视角: %q", got)
	}

	room := mustParse(t, `{"msgid":"m2","action":"send","from":"u1","roomid":"roomA","msgtime":1760000000000,"msgtype":"text","text":{"content":"群里好"}}`)
	if got := room.ConvKey(); got != "room_roomA" {
		t.Fatalf("群聊 ConvKey: %q", got)
	}
	if got := room.ConvFileName("u1", resolve); got != "群聊_roomA.md" {
		t.Fatalf("群聊文件名: %q", got)
	}
}

func TestRenderLine(t *testing.T) {
	resolve := func(u string) string { return "姓名_" + u }

	text := mustParse(t, `{"msgid":"m1","action":"send","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"text","text":{"content":"合同看一下"}}`)
	line, skip := RenderLine(text, resolve)
	if skip || !strings.Contains(line, "姓名_u1") || !strings.Contains(line, "合同看一下") {
		t.Fatalf("text 渲染: %q skip=%v", line, skip)
	}

	file := mustParse(t, `{"msgid":"m2","action":"send","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"file","file":{"sdkfileid":"fid","filename":"采购合同V1.docx","filesize":123}}`)
	line, _ = RenderLine(file, resolve)
	if !strings.Contains(line, "[文件：采购合同V1.docx]") {
		t.Fatalf("file 渲染: %q", line)
	}

	recall := mustParse(t, `{"msgid":"m3","action":"recall","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"text"}`)
	line, skip = RenderLine(recall, resolve)
	if skip || !strings.Contains(line, "撤回了一条消息") {
		t.Fatalf("recall 渲染: %q skip=%v", line, skip)
	}

	sw := mustParse(t, `{"msgid":"m4","action":"switch","from":"u1","msgtime":1760000000000,"msgtype":"text"}`)
	if _, skip = RenderLine(sw, resolve); !skip {
		t.Fatal("switch 应跳过")
	}

	mixed := mustParse(t, `{"msgid":"m5","action":"send","from":"u1","tolist":["u2"],"msgtime":1760000000000,"msgtype":"mixed","mixed":{"item":[{"type":"text","content":"看这个"},{"type":"image"}]}}`)
	line, _ = RenderLine(mixed, resolve)
	if !strings.Contains(line, "看这个") || !strings.Contains(line, "[image]") {
		t.Fatalf("mixed 渲染: %q", line)
	}
}

// TestSanitizeMarkdownHTML 企微 markdown 消息里可能塞整段 HTML（含 base64 图片），
// 原样写进会话文件会变成几百 KB 乱码。这里验证会被压成一行可读文本。
func TestSanitizeMarkdownHTML(t *testing.T) {
	raw := `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<style>body{font-family:Arial;font-size:12px}h1{color:#06C}</style>
</head>
<body>
<img src="data:image/jpeg;base64,/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwD3+iiigD//2Q==">
<h1>系统告警</h1>
<p>服务器 CPU 使用率超过 90%</p>
</body>
</html>`

	got := sanitizeMarkdown(raw)
	t.Logf("输出: %q", got)

	if strings.Contains(got, "<") || strings.Contains(got, "DOCTYPE") {
		t.Fatalf("HTML 标签未清除: %q", got)
	}
	if strings.Contains(got, "base64") || strings.Contains(got, "/9j/4AAQ") {
		t.Fatalf("base64 图片未清除: %q", got)
	}
	if !strings.Contains(got, "系统告警") || !strings.Contains(got, "CPU 使用率超过 90%") {
		t.Fatalf("正文丢失: %q", got)
	}
	if !strings.Contains(got, "[图片]") {
		t.Fatalf("图片应替换为占位符: %q", got)
	}
	if len(got) > 200 {
		t.Fatalf("输出过长（%d 字符），base64 可能没清干净: %q", len(got), got)
	}
}

// TestSanitizeMarkdownPlain 纯 Markdown 文本应原样保留（只压空白）。
func TestSanitizeMarkdownPlain(t *testing.T) {
	in := "**重要**：会议改到\n明天上午十点"
	got := sanitizeMarkdown(in)
	if !strings.Contains(got, "**重要**") {
		t.Fatalf("Markdown 格式被破坏: %q", got)
	}
	if !strings.Contains(got, "会议改到") {
		t.Fatalf("内容丢失: %q", got)
	}
}

func TestRenderLineSanitizesHTMLInTextAndMarkdown(t *testing.T) {
	for _, msgtype := range []string{"text", "markdown"} {
		raw := `{"msgid":"m1","action":"send","from":"u1","msgtime":1760000000000,"msgtype":"` + msgtype + `","` + msgtype + `":{"content":"<html><body><p>中文通知</p><script>var x='bad';</script></body></html>"}}`
		m := mustParse(t, raw)
		line, skip := RenderLine(m, nil)
		if skip {
			t.Fatalf("%s unexpectedly skipped", msgtype)
		}
		if !strings.Contains(line, "中文通知") {
			t.Fatalf("%s 内容丢失: %q", msgtype, line)
		}
		for _, unwanted := range []string{"<html", "<body", "<p>", "<script>", "var x", "</html>"} {
			if strings.Contains(line, unwanted) {
				t.Fatalf("%s 未清理 %q: %q", msgtype, unwanted, line)
			}
		}
	}
}
