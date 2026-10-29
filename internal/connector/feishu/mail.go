package feishu

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	feishuapi "github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/model"
)

const (
	mailMaxPerRoundDefault = 100
	// 已补齐的文件夹平时只翻到“整页都存过”为止；每天完整翻一遍，兜住移进旧位置的邮件。
	mailFullScanEvery = int64(24 * 3600)
	// 列邮件 10 次/秒、邮件详情 100 次/分钟、附件下载链接 1 次/秒（官方文档）。
	mailListMinInterval   = 100 * time.Millisecond
	mailFolderMinInterval = 200 * time.Millisecond // 文件夹列表 5 次/秒
	mailGetMinInterval    = 600 * time.Millisecond
	mailURLMinInterval    = time.Second
	// 正文文件名里的主题最多保留的字数，避免文件名过长。
	mailSubjectMaxRunes = 60
)

// 要存的系统文件夹；草稿箱、垃圾邮件、已删除不存。用户自建文件夹另外从文件夹列表里取。
var mailSystemFolders = []string{"INBOX", "SENT", "ARCHIVED"}

// mailTZ 邮件时间统一按北京时间显示和命名（服务器容器里一般是 UTC）。
var mailTZ = time.FixedZone("CST", 8*3600)

// mailAck 一条邮件记录写成功后要记为已处理的邮件拷贝（同一封邮件在不同文件夹里 message_id 不同）。
type mailAck struct {
	smtpID string
	ids    []string
}

// scanMail 扫一个员工的邮箱：逐个文件夹翻页列邮件 ID，跳过已处理过的，
// 对没处理过的取详情、产出一条记录（全部非内联附件 + 正文 .html），每轮最多处理 MailMaxPerRound 封。
// 同一封邮件的多份拷贝（如发给自己的邮件在收件箱和已发送各有一份）按 RFC 邮件 ID 只存一份。
func (c *Connector) scanMail(st *scanState, userToken, userID string, now int64) {
	limit := c.cfg.MailMaxPerRound
	if limit <= 0 {
		limit = mailMaxPerRoundDefault
	}
	var perFolder [][]string // 每个文件夹里没处理过的邮件，从新到旧
	queued := map[string]bool{}
	for _, folder := range c.mailFolders(userToken, userID) {
		full := now-c.store.MailFolderScannedAt(userID, folder) >= mailFullScanEvery
		var list []string
		pageToken, unknown, complete := "", 0, false
		for {
			pace(&c.lastMailList, mailListMinInterval)
			ids, next, more, err := c.api.ListMailboxMessages(userToken, folder, pageToken)
			if err != nil {
				log.Printf("[feishu] 列 %s 邮箱文件夹 %s 失败: %v", userID, folder, err)
				break
			}
			fresh := 0
			for _, id := range ids {
				if id == "" || queued[id] || c.store.MailSeen(userID, id) {
					continue
				}
				queued[id] = true
				list = append(list, id)
				fresh++
			}
			unknown += fresh
			if !more || next == "" {
				complete = true
				break
			}
			if !full && fresh == 0 { // 列表从新到旧：整页都存过，后面的也存过了
				break
			}
			if len(list) >= limit { // 这个文件夹本轮处理不完更多了，后面的页下一轮再翻（不算完整核对）
				break
			}
			pageToken = next
		}
		// 完整翻完且一封没漏，才记为已补齐；否则下一轮继续完整翻
		if full && complete && unknown == 0 {
			if err := c.store.SetMailFolderScannedAt(userID, folder, now); err != nil {
				log.Printf("[feishu] 记录 %s 邮箱文件夹 %s 核对时间失败: %v", userID, folder, err)
			}
		}
		perFolder = append(perFolder, list)
	}
	// 各文件夹轮流取：收件箱补历史时，已发送等文件夹的邮件每轮也能处理到
	var todo []string
	for i := 0; ; i++ {
		added := false
		for _, list := range perFolder {
			if i < len(list) {
				todo = append(todo, list[i])
				added = true
			}
		}
		if !added {
			break
		}
	}

	n, dup := 0, 0
	bySMTP := map[string]*mailAck{} // 本轮已产出的邮件：RFC 邮件 ID → 记录
	for _, id := range todo {
		if n >= limit {
			break
		}
		n++
		pace(&c.lastMailGet, mailGetMinInterval)
		msg, err := c.api.GetMailMessage(userToken, id)
		if err != nil || msg == nil {
			log.Printf("[feishu] 获取邮件 %s 详情失败: %v", id, err)
			continue
		}
		if msg.SMTPID != "" {
			if ack := bySMTP[msg.SMTPID]; ack != nil { // 本轮已产出同一封邮件的另一份拷贝
				ack.ids = append(ack.ids, id)
				dup++
				continue
			}
			if c.store.MailSeenSMTP(userID, msg.SMTPID) { // 之前已存过另一份拷贝
				if err := c.store.AddMailSeen(userID, id, msg.SMTPID); err != nil {
					log.Printf("[feishu] 记录邮件 %s 已处理失败: %v", id, err)
				}
				dup++
				continue
			}
		}
		rec := mailToRecord(id, msg, userID)
		ack := &mailAck{smtpID: msg.SMTPID, ids: []string{id}}
		if msg.SMTPID != "" {
			bySMTP[msg.SMTPID] = ack
		}
		c.mailMu.Lock()
		if c.mailPending == nil {
			c.mailPending = map[string]*mailAck{}
		}
		c.mailPending[rec.ID] = ack
		c.mailMu.Unlock()
		c.cacheMail(id, msg)
		c.collect(st, rec, userID)
	}
	if len(todo) > 0 {
		log.Printf("[feishu] %s 邮件：待处理 %d 封，本轮处理 %d 封（其中重复拷贝 %d 封）", userID, len(todo), n, dup)
	}
}

// ackMail 邮件记录写成功后，把这封邮件的各份拷贝记为已处理；全部写成功才移出待确认。
// 写失败时这封邮件下一轮会重新处理（文件已在 NAS 上，重写同名文件）。
func (c *Connector) ackMail(recordID, personID string) error {
	c.mailMu.Lock()
	ack := c.mailPending[recordID]
	c.mailMu.Unlock()
	if ack == nil {
		return nil
	}
	for _, id := range ack.ids {
		if err := c.store.AddMailSeen(personID, id, ack.smtpID); err != nil {
			return err
		}
	}
	c.mailMu.Lock()
	delete(c.mailPending, recordID)
	c.mailMu.Unlock()
	return nil
}

// mailFolders 要扫的文件夹：收件箱、已发送、已归档 + 用户自建文件夹。
// 列文件夹失败（旧授权没有 mail:user_mailbox.folder:read）时只扫三个系统文件夹。
func (c *Connector) mailFolders(userToken, userID string) []string {
	out := append([]string(nil), mailSystemFolders...)
	pace(&c.lastMailList, mailFolderMinInterval)
	folders, err := c.api.ListMailFolders(userToken)
	if err != nil {
		log.Printf("[feishu] 列 %s 邮箱文件夹失败（本轮只扫收件箱、已发送、已归档）: %v", userID, err)
		return out
	}
	for _, f := range folders {
		if f.Type == 2 && f.ID != "" {
			out = append(out, f.ID)
		}
	}
	return out
}

// mailShortID 文件名后缀：邮件 ID 去掉末尾 = 补位后的最后 8 位，区分同名附件和同主题邮件。
func mailShortID(msgID string) string {
	s := strings.TrimRight(msgID, "=")
	if len(s) > 8 {
		s = s[len(s)-8:]
	}
	return s
}

// mailToRecord 一封邮件 → 一条记录：正文 .html 在前（副本状态里的文件名取第一个），附件沿用原有命名。
// 邮件 ID 统一用列表给的 msgID，不依赖详情响应里的字段。
func mailToRecord(msgID string, msg *feishuapi.MailMessage, userID string) model.Record {
	shortID := mailShortID(msgID)
	subject := sanitizeFilename(strings.TrimSpace(msg.Subject))
	if subject == "" {
		subject = "无主题"
	}
	if utf8.RuneCountInString(subject) > mailSubjectMaxRunes {
		subject = string([]rune(subject)[:mailSubjectMaxRunes])
	}
	day := time.UnixMilli(msg.CreatedTime).In(mailTZ).Format("2006-01-02")
	atts := []model.Attachment{{
		Name:   day + "_" + subject + "_" + shortID + ".html",
		FileID: "mailbody:" + msgID,
		UserID: userID,
	}}
	usedNames := map[string]int{} // 同一封邮件里的同名附件再加序号
	for _, ma := range msg.Attachments {
		if ma.ID == "" || ma.Name == "" || ma.IsInline {
			continue
		}
		// 附件名后加邮件 ID 短码，避免不同邮件的同名附件互相覆盖
		ext := path.Ext(ma.Name)
		attName := sanitizeFilename(strings.TrimSuffix(ma.Name, ext)) + "_" + shortID
		if usedNames[attName]++; usedNames[attName] > 1 {
			attName = fmt.Sprintf("%s_%d", attName, usedNames[attName])
		}
		atts = append(atts, model.Attachment{
			Name:   attName + ext,
			FileID: "mail:" + msgID + ":" + ma.ID,
			Size:   ma.Size,
			UserID: userID,
		})
	}
	return model.Record{
		Source:      "feishu",
		Kind:        "mail",
		ID:          "mail_" + msgID,
		Timestamp:   msg.CreatedTime,
		OwnerIDs:    []string{userID},
		SenderID:    userID,
		Attachments: atts,
	}
}

func (c *Connector) cacheMail(msgID string, msg *feishuapi.MailMessage) {
	c.mailMu.Lock()
	defer c.mailMu.Unlock()
	if c.mailCache == nil {
		c.mailCache = map[string]*feishuapi.MailMessage{}
	}
	c.mailCache[msgID] = msg
}

func (c *Connector) takeCachedMail(msgID string) *feishuapi.MailMessage {
	c.mailMu.Lock()
	defer c.mailMu.Unlock()
	msg := c.mailCache[msgID]
	delete(c.mailCache, msgID)
	return msg
}

// mailBody 生成邮件正文 .html：邮件头（主题、收发件人、时间、附件名）+ 原正文；
// 内联图片下载后以 data: 地址嵌进正文，文件单独打开也能看到图片。任一张图片下载失败就整体报错，下一轮重试。
func (c *Connector) mailBody(userToken, msgID string) (io.ReadCloser, error) {
	msg := c.takeCachedMail(msgID)
	if msg == nil {
		pace(&c.lastMailGet, mailGetMinInterval)
		m, err := c.api.GetMailMessage(userToken, msgID)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, fmt.Errorf("邮件 %s 详情为空", msgID)
		}
		msg = m
	}
	inline := map[string]string{} // cid → data: 地址
	for _, a := range msg.Attachments {
		if !a.IsInline || a.CID == "" || a.ID == "" {
			continue
		}
		pace(&c.lastMailURL, mailURLMinInterval)
		rc, err := c.api.DownloadMailAttachment(userToken, msgID, a.ID)
		if err != nil {
			return nil, fmt.Errorf("下载邮件 %s 的内联图片 %s: %w", msgID, a.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("读取邮件 %s 的内联图片 %s: %w", msgID, a.Name, err)
		}
		inline[a.CID] = "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return io.NopCloser(bytes.NewReader(renderMailHTML(msg, inline))), nil
}

// renderMailHTML 拼出正文文件。邮件原正文是外部内容，加 CSP：禁止脚本，也不加载任何外部资源
// （外部图片常是已读追踪像素），只放行已嵌入的 data: 图片和内联样式。
func renderMailHTML(msg *feishuapi.MailMessage, inline map[string]string) []byte {
	subject := msg.Subject
	if strings.TrimSpace(subject) == "" {
		subject = "无主题"
	}
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\">\n")
	b.WriteString("<meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; img-src data:; style-src 'unsafe-inline'\">\n")
	b.WriteString("<title>" + html.EscapeString(subject) + "</title></head><body>\n<table>\n")
	row := func(k, v string) {
		if v != "" {
			b.WriteString("<tr><th align=\"left\">" + k + "</th><td>" + html.EscapeString(v) + "</td></tr>\n")
		}
	}
	row("主题", subject)
	row("发件人", formatMailAddrs([]feishuapi.MailAddress{{Address: msg.HeadFrom, Name: msg.FromName}}))
	row("收件人", formatMailAddrs(msg.To))
	row("抄送", formatMailAddrs(msg.Cc))
	row("密送", formatMailAddrs(msg.Bcc))
	if msg.CreatedTime > 0 {
		row("时间", time.UnixMilli(msg.CreatedTime).In(mailTZ).Format("2006-01-02 15:04:05"))
	}
	var names []string
	for _, a := range msg.Attachments {
		if !a.IsInline && a.Name != "" {
			names = append(names, a.Name)
		}
	}
	row("附件", strings.Join(names, "；"))
	b.WriteString("</table>\n<hr>\n")
	body := msg.BodyHTML
	if body == "" {
		body = "<pre>" + html.EscapeString(msg.BodyPlain) + "</pre>"
	}
	for cid, uri := range inline {
		body = strings.ReplaceAll(body, "cid:"+cid, uri)
	}
	b.WriteString(body)
	b.WriteString("\n</body></html>\n")
	return []byte(b.String())
}

// formatMailAddrs “显示名 <地址>”，多个用分号隔开；没有地址的跳过。
func formatMailAddrs(list []feishuapi.MailAddress) string {
	var out []string
	for _, a := range list {
		switch {
		case a.Address == "":
			continue
		case a.Name != "":
			out = append(out, a.Name+" <"+a.Address+">")
		default:
			out = append(out, a.Address)
		}
	}
	return strings.Join(out, "; ")
}

// pace 两次调用之间至少间隔 gap（按接口频率限制减速）。
func pace(last *atomic.Int64, gap time.Duration) {
	now := time.Now().UnixNano()
	prev := last.Load()
	if wait := gap - time.Duration(now-prev); wait > 0 && prev > 0 {
		time.Sleep(wait)
	}
	last.Store(time.Now().UnixNano())
}
