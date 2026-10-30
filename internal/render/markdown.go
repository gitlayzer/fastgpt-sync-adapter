// Package render 企微消息 → Markdown 行渲染，以及会话归组。
// 归组规则：单聊按"两人对"归一组（与 roomid 无关）；群聊按 roomid。
// 渲染只产出可读 Markdown 行，原始 JSON 由 sink 另行原样落盘（不清洗红线）。
package render

import (
	"encoding/json"
	"fmt"
	"regexp"
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

	Text *struct {
		Content string `json:"content"`
	} `json:"text"`
	Markdown *struct {
		Content string `json:"content"`
	} `json:"markdown"`
	Image *struct {
		SDKFileID string `json:"sdkfileid"`
	} `json:"image"`
	File *struct {
		SDKFileID string `json:"sdkfileid"`
		FileName  string `json:"filename"`
		FileSize  int64  `json:"filesize"`
	} `json:"file"`
	Voice *struct {
		SDKFileID string `json:"sdkfileid"`
		VoiceSize int64  `json:"voice_size"`
	} `json:"voice"`
	Video *struct {
		SDKFileID string `json:"sdkfileid"`
	} `json:"video"`
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
	Todo *struct {
		Title string `json:"title"`
	} `json:"todo"`
	Vote *struct {
		Title string `json:"title"`
	} `json:"vote"`
	ChatRecord *struct {
		Title string           `json:"title"`
		Item  []ChatRecordItem `json:"item"`
	} `json:"chatrecord"`
	Mixed *struct {
		Item []struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"item"`
	} `json:"mixed"`
}

// ChatRecordItem 企微合并转发消息中的子项。
type ChatRecordItem struct {
	Type         string `json:"type"`
	MsgTime      int64  `json:"msgtime"`
	Content      string `json:"content"`
	FromChatroom bool   `json:"from_chatroom"`
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
	// 名片里的 userid 可能是外部联系人 ID，和发送人一样走名字解析
	if m.Card != nil && resolveName != nil {
		if name := resolveName(m.Card.UserID); name != "" && name != m.Card.UserID {
			content = fmt.Sprintf("[名片：%s]", name)
		}
	}
	if content == "" {
		content = "[" + m.MsgType + "]"
	}
	return fmt.Sprintf("- **[%s] %s**：%s\n", ts, sender, content), false
}

// contentText 按 msgtype 提取可读文本。
func (m *Msg) contentText() string {
	switch {
	case m.Text != nil:
		// 有些机器人或转发消息会把 HTML 塞进 text.content，而不只是 markdown.content。
		// 两种字段都做同一层清洗，避免整页 HTML / base64 被原样写进会话文件。
		return sanitizeMarkdown(m.Text.Content)
	case m.Markdown != nil:
		// 企微的 markdown 消息有两种形态：
		//   1) 真正的 Markdown 纯文本 —— 直接用
		//   2) 内嵌整段 HTML 的富文本（常见于机器人推送），可能带 base64 图片
		//      —— 直接写进会话文件会变成一堆 <!DOCTYPE html> 和 base64 乱码
		// 这里检测到 HTML 就剥离标签只留文本，base64 图片替换成占位符。
		return sanitizeMarkdown(m.Markdown.Content)
	case m.Image != nil:
		if m.MsgID != "" {
			dateFolder := time.UnixMilli(m.MsgTime).Format("2006-01-02")
			if dateFolder == "" || m.MsgTime <= 0 {
				dateFolder = time.Now().Format("2006-01-02")
			}
			return fmt.Sprintf("![图片](images/%s/image_%s.jpg)", dateFolder, m.MsgID)
		}
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
		title := m.ChatRecord.Title
		if title == "" {
			title = "聊天记录"
		}
		if len(m.ChatRecord.Item) == 0 {
			return fmt.Sprintf("[聊天记录：%s]", title)
		}
		var itemTexts []string
		for _, it := range m.ChatRecord.Item {
			text := parseChatRecordItemContent(it.Type, it.Content)
			if text != "" {
				itemTexts = append(itemTexts, text)
			}
		}
		if len(itemTexts) == 0 {
			return fmt.Sprintf("[聊天记录：%s]", title)
		}
		return fmt.Sprintf("[聊天记录：%s]（共 %d 条）：%s", title, len(itemTexts), strings.Join(itemTexts, " ｜ "))
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

// parseChatRecordItemContent 格式化合并转发（chatrecord）中的各个子项内容。
func parseChatRecordItemContent(itemType, rawContent string) string {
	rawContent = strings.TrimSpace(rawContent)
	if rawContent == "" {
		return "[" + itemType + "]"
	}

	switch itemType {
	case "ChatRecordText":
		var body struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(rawContent), &body); err == nil && body.Content != "" {
			return sanitizeMarkdown(body.Content)
		}
		return sanitizeMarkdown(rawContent)

	case "ChatRecordFile":
		var body struct {
			FileName string `json:"filename"`
		}
		if err := json.Unmarshal([]byte(rawContent), &body); err == nil && body.FileName != "" {
			return fmt.Sprintf("[文件：%s]", body.FileName)
		}
		return "[文件]"

	case "ChatRecordImage":
		return "[图片]"

	case "ChatRecordVoice":
		return "[语音]"

	case "ChatRecordVideo":
		return "[视频]"

	case "ChatRecordLink":
		var body struct {
			Title   string `json:"title"`
			LinkURL string `json:"link_url"`
		}
		if err := json.Unmarshal([]byte(rawContent), &body); err == nil {
			if body.LinkURL != "" && body.Title != "" {
				return fmt.Sprintf("[链接：%s](%s)", body.Title, body.LinkURL)
			} else if body.Title != "" {
				return fmt.Sprintf("[链接：%s]", body.Title)
			}
		}
		return "[链接]"

	case "ChatRecordLocation":
		var body struct {
			Title   string `json:"title"`
			Address string `json:"address"`
		}
		if err := json.Unmarshal([]byte(rawContent), &body); err == nil {
			return fmt.Sprintf("[位置：%s %s]", body.Title, body.Address)
		}
		return "[位置]"

	default:
		var fallback map[string]any
		if err := json.Unmarshal([]byte(rawContent), &fallback); err == nil {
			if title, ok := fallback["title"].(string); ok && title != "" {
				return fmt.Sprintf("[%s：%s]", itemType, title)
			}
			if c, ok := fallback["content"].(string); ok && c != "" {
				return sanitizeMarkdown(c)
			}
		}
		return "[" + itemType + "]"
	}
}

// sanitizeMarkdown 把企微 markdown 消息的内容转成适合写进会话文件的单行文本。
//
// 企微允许 markdown 消息里塞整段 HTML（机器人推送常见），甚至内嵌
// data:image/*;base64 图片。原样写进 .md 会得到几百 KB 的乱码。
// 处理规则：
//   - 纯 Markdown 文本原样保留
//   - 含 HTML 时剥离标签，只留可读文本
//   - data URI / base64 图片替换成 [图片]
//   - 折叠多余空白，压成一行
func sanitizeMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	lower := strings.ToLower(s)
	isHTML := strings.Contains(lower, "<!doctype") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<body") ||
		strings.Contains(lower, "<div") ||
		strings.Contains(lower, "<img") ||
		strings.Contains(lower, "<style") ||
		strings.Contains(lower, "<script") ||
		strings.Contains(lower, "<meta") ||
		strings.Contains(lower, "<p") ||
		strings.Contains(lower, "<br") ||
		strings.Contains(lower, "<table") ||
		strings.Contains(lower, "<section") ||
		strings.Contains(lower, "<iframe")

	if !isHTML {
		// 纯 Markdown：只把换行压成空格，保留原有格式符号
		return collapseSpace(s)
	}

	out := s
	// 1) 去掉 <style>/<script> 整块（含规则文本，对读者无意义）
	out = stripBlockRe(out, "style")
	out = stripBlockRe(out, "script")
	out = stripBlockRe(out, "head")
	// 2) img 标签替换为 [图片]（必须在去掉其余 HTML 标签前处理）
	out = imgTagPat.ReplaceAllString(out, " [图片] ")
	// 3) 裸露的 data URI 图片 → [图片]（必须先去，否则 base64 会被当普通文本留下）
	out = dataURIPat.ReplaceAllString(out, "[图片]")
	// 4) 其余标签直接去掉
	out = htmlTagPat.ReplaceAllString(out, " ")
	// 5) 常见的 HTML 实体
	out = htmlEntityReplacer.Replace(out)
	return collapseSpace(out)
}

// stripBlockRe 移除 <tag ...>...</tag> 整块（大小写不敏感，支持跨行）。
func stripBlockRe(s, tag string) string {
	pat := regexp.MustCompile(`(?is)<` + tag + `\b[^>]*>.*?</` + tag + `\s*>`)
	return pat.ReplaceAllString(s, " ")
}

var (
	// img 标签（可能含 base64 或外部链接）
	imgTagPat = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	// data:image/xxx;base64,....（base64 段可能非常长）
	dataURIPat = regexp.MustCompile(`(?is)data:image/[a-z0-9.+-]+;base64,[A-Za-z0-9+/=\s]+`)
	// 任意 HTML 标签。用 [^>]* 而不是 {0,N}：Go 的 regexp 重复次数上限是 1000，
	// 超了会在 init 阶段 panic。base64 内容前面已剥离，这里不会遇到超长标签。
	htmlTagPat = regexp.MustCompile(`(?s)<[^>]*>`)
	// 常见实体
	htmlEntityReplacer = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'",
	)
	spacePat = regexp.MustCompile(`[ \t\r\n]+`)
)

// collapseSpace 把连续空白压成单个空格并去首尾空白。
func collapseSpace(s string) string {
	return strings.TrimSpace(spacePat.ReplaceAllString(s, " "))
}
