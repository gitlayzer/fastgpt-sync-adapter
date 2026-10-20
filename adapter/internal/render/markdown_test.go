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
