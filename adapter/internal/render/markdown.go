// Package render 企微消息 → Markdown 行渲染，以及会话归组。
// 归组规则：单聊按"两人对"归一组（与 roomid 无关）；群聊按 roomid。
// 渲染只产出可读 Markdown 行，原始 JSON 由 sink 另行原样落盘（不清洗红线）。
package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Msg 企微消息的结构化视图（字段覆盖常见 msgtype）。
type Msg struct {
	MsgID   string   `json:"msgid"`
	Action  string   `json:"action"` // send / recall / switch
	From    string   `json:"from"`
	ToList  []string `json:"tolist"`
	RoomID  string   `json:"roomid"`
	MsgTime int64    `json:"msgtime"` // 毫秒
	MsgType string   `json:"msgtype"`

	Text     *struct{ Content string `json:"content"` }     `json:"text"`
	Markdown *struct{ Content string `json:"content"` }     `json:"markdown"`
	Image    *struct{ SDKFileID string `json:"sdkfileid"` }   `json:"image"`
	File     *struct {
		SDKFileID string `json:"sdkfileid"`
		FileName  string `json:"filename"`
		FileSize  int64  `json:"filesize"`
	} `json:"file"`
	Voice   *struct {
		SDKFileID string `json:"sdkfileid"`
		VoiceSize int64  `json:"voice_size"`
	} `json:"voice"`
	Video   *struct{ SDKFileID string `json:"sdkfileid"` } `json:"video"`
	Emotion *struct {
		SDKFileID string `json:"sdkfileid"`
		Type      int    `json:"type"`
	} `json:"emotion"`
	Link *struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
	} `json:"link"`
	Location *struct {
		Title   string `json:"title"`
		Address string `json:"address"`
	} `json:"location"`
	Weapp *struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"weapp"`
	Card *struct {
		CorpName string `json:"corpname"`
		UserID   string `json:"userid"`
	} `json:"card"`
	Todo *struct{ Title string `json:"title"` } `json:"todo"`
	Vote *struct{ Title string `json:"title"` } `json:"vote"`
	ChatRecord *struct {
		Title string `json:"title"`
	} `json:"chatrecord"`
	Mixed *struct {
		Item []struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"item"`
	} `json:"mixed"`
}

// Parse 解析消息原始 JSON。
func Parse(raw json.RawMessage) (*Msg, error) {
	var m Msg
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析消息 JSON: %w", err)
	}
	return &m, nil
}

// Participants 本条消息的参与者集合（from + tolist）。
func (m *Msg) Participants() []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range append([]string{m.From}, m.ToList...) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// ConvKey 会话稳定标识（与具体某个查看者无关）：
// 单聊 = "dm_" + 排序后的两个 userid；群聊 = "room_" + roomid。
func (m *Msg) ConvKey() string {
	if m.RoomID != "" {
		return "room_" + m.RoomID
	}
	pair := m.Participants()
	sort.Strings(pair)
	return "dm_" + strings.Join(pair, "_")
}

// ConvFileName 某个查看者视角的会话文件名（不含目录）。
// 单聊：以对方姓名命名（"与张三的单聊.md"）；群聊：群聊_<roomid>.md。
func (m *Msg) ConvFileName(forUser string, resolveName func(string) string) string {
	if m.RoomID != "" {
		return fmt.Sprintf("群聊_%s.md", m.RoomID)
	}
	counterpart := ""
	for _, u := range m.Participants() {
		if u != forUser {
			counterpart = u
			break
		}
	}
	if counterpart == "" {
		counterpart = m.From
	}
	name := counterpart
	if resolveName != nil {
		name = resolveName(counterpart)
	}
	return fmt.Sprintf("与%s的单聊.md", name)
}

// RenderLine 把一条消息渲染成一行 Markdown。
// 返回 skip=true 表示该行不该写入（如切换企业的系统动作）。
func RenderLine(m *Msg, resolveName func(string) string) (line string, skip bool) {
	ts := time.UnixMilli(m.MsgTime).Format("2006-01-02 15:04:05")
	sender := m.From
	if resolveName != nil {
		sender = resolveName(m.From)
	}

	if m.Action == "switch" {
		return "", true // 切换企业日志，无实质内容
	}
	if m.Action == "recall" {
		return fmt.Sprintf("- *[%s] %s 撤回了一条消息*\n", ts, sender), false
	}

	content := m.contentText()
	if content == "" {
		content = "[" + m.MsgType + "]"
	}
	return fmt.Sprintf("- **[%s] %s**：%s\n", ts, sender, content), false
}

// contentText 按 msgtype 提取可读文本。
func (m *Msg) contentText() string {
	switch {
	case m.Text != nil:
		return m.Text.Content
	case m.Markdown != nil:
		return m.Markdown.Content
	case m.Image != nil:
		return "[图片]"
	case m.File != nil:
		name := m.File.FileName
		if name == "" {
			name = m.File.SDKFileID
		}
		return fmt.Sprintf("[文件：%s]", name)
	case m.Voice != nil:
		return "[语音]"
	case m.Video != nil:
		return "[视频]"
	case m.Emotion != nil:
		return "[表情]"
	case m.Link != nil:
		return fmt.Sprintf("[链接：%s](%s)", m.Link.Title, m.Link.URL)
	case m.Location != nil:
		return fmt.Sprintf("[位置：%s %s]", m.Location.Title, m.Location.Address)
	case m.Weapp != nil:
		return fmt.Sprintf("[小程序：%s]", m.Weapp.Title)
	case m.Card != nil:
		return fmt.Sprintf("[名片：%s %s]", m.Card.CorpName, m.Card.UserID)
	case m.Todo != nil:
		return fmt.Sprintf("[待办：%s]", m.Todo.Title)
	case m.Vote != nil:
		return fmt.Sprintf("[投票：%s]", m.Vote.Title)
	case m.ChatRecord != nil:
		return fmt.Sprintf("[聊天记录：%s]", m.ChatRecord.Title)
	case m.Mixed != nil:
		var parts []string
		for _, it := range m.Mixed.Item {
			if it.Type == "text" {
				parts = append(parts, it.Content)
			} else {
				parts = append(parts, "["+it.Type+"]")
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}
