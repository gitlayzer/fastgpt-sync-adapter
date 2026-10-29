package feishu

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListMailboxMessages(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		q := r.URL.Query()
		if q.Get("page_size") != "20" || q.Get("folder_id") != "SENT" || q.Get("page_token") != "p1" {
			t.Fatalf("请求参数不符: %s", r.URL.RawQuery)
		}
		return `{"code":0,"data":{"has_more":true,"page_token":"p2","items":["m1","m2"]}}`
	})}

	items, next, more, err := c.ListMailboxMessages("ut", "SENT", "p1")
	if err != nil {
		t.Fatalf("ListMailboxMessages: %v", err)
	}
	if len(items) != 2 || items[0] != "m1" || items[1] != "m2" || next != "p2" || !more {
		t.Fatalf("解析邮件 ID 列表不符: %v %q %v", items, next, more)
	}
}

// 列文件夹：系统文件夹 folder_type=1，用户自建 folder_type=2（结构以实测和官方 SDK 为准）。
func TestListMailFolders(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		return `{"code":0,"data":{"items":[{"folder_type":1,"id":"INBOX","name":"Inbox"},{"folder_type":2,"id":"7351","name":"客户"}]}}`
	})}
	fs, err := c.ListMailFolders("ut")
	if err != nil {
		t.Fatalf("ListMailFolders: %v", err)
	}
	if len(fs) != 2 || fs[0].ID != "INBOX" || fs[0].Type != 1 || fs[1].ID != "7351" || fs[1].Type != 2 || fs[1].Name != "客户" {
		t.Fatalf("解析文件夹不符: %+v", fs)
	}
}

func TestListMailboxMessagesObjectArray(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		return `{"code":0,"data":{"has_more":false,"items":[{"id":"obj_m1"},{"id":"obj_m2"}]}}`
	})}

	items, _, _, err := c.ListMailboxMessages("ut", "INBOX", "")
	if err != nil {
		t.Fatalf("ListMailboxMessages: %v", err)
	}
	if len(items) != 2 || items[0] != "obj_m1" || items[1] != "obj_m2" {
		t.Fatalf("解析对象数组 items 失败: %v", items)
	}
}

func TestListMailboxMessagesUnactivated(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		return `{"code":15180001,"msg":"user has no mailbox"}`
	})}

	items, _, _, err := c.ListMailboxMessages("ut", "INBOX", "")
	if err != nil {
		t.Fatalf("未开通邮箱时不应报错: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("未开通邮箱应返回空列表: %v", items)
	}
}

func TestGetMailMessage(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		return `{
			"code": 0,
			"data": {
				"message": {
					"message_id": "msg_123",
					"subject": "重要采购合同",
					"head_from": {"mail_address": "vendor@example.com", "name": "供应商"},
					"to": [{"mail_address": "a@example.com", "name": "张三"}],
					"cc": [{"mail_address": "b@example.com", "name": ""}],
					"internal_date": "1700000000000",
					"body_html": "PHA-5q2j5paHPC9wPg==",
					"body_plain_text": "5q2j5paH",
					"attachments": [
						{"id": "att_1", "filename": "合同.pdf", "size": 10240, "is_inline": false},
						{"id": "att_2", "filename": "签名.png", "size": 2048, "is_inline": true, "cid": "sig01"}
					]
				}
			}
		}`
	})}

	msg, err := c.GetMailMessage("ut", "msg_123")
	if err != nil {
		t.Fatalf("GetMailMessage: %v", err)
	}
	if msg.MessageID != "msg_123" || msg.Subject != "重要采购合同" || msg.CreatedTime != 1700000000000 {
		t.Fatalf("邮件详情解析错误: %+v", msg)
	}
	if msg.HeadFrom != "vendor@example.com" || msg.FromName != "供应商" {
		t.Fatalf("发件人解析错误: %q %q", msg.HeadFrom, msg.FromName)
	}
	if len(msg.To) != 1 || msg.To[0].Address != "a@example.com" || msg.To[0].Name != "张三" || len(msg.Cc) != 1 || msg.Cc[0].Address != "b@example.com" {
		t.Fatalf("收件人/抄送解析错误: %+v %+v", msg.To, msg.Cc)
	}
	if msg.BodyHTML != "<p>正文</p>" || msg.BodyPlain != "正文" {
		t.Fatalf("正文 base64url 解码错误: %q %q", msg.BodyHTML, msg.BodyPlain)
	}
	if len(msg.Attachments) != 2 {
		t.Fatalf("附件数量错误: %d", len(msg.Attachments))
	}
	if msg.Attachments[0].Name != "合同.pdf" || msg.Attachments[0].IsInline != false {
		t.Fatalf("附件 1 错误: %+v", msg.Attachments[0])
	}
	if msg.Attachments[1].Name != "签名.png" || msg.Attachments[1].IsInline != true || msg.Attachments[1].CID != "sig01" {
		t.Fatalf("附件 2 错误: %+v", msg.Attachments[1])
	}
}

func TestDownloadMailAttachment(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "download_url") {
			return `{
				"code": 0,
				"data": {
					"download_urls": [
						{"attachment_id": "att_1", "download_url": "https://open.feishu.cn/fake_dl_1"}
					],
					"failed_ids": [],
					"failed_reasons": []
				}
			}`
		}
		if r.URL.Path == "/fake_dl_1" {
			return "FILE_CONTENT_BYTES"
		}
		return `{"code":404}`
	})}

	rc, err := c.DownloadMailAttachment("ut", "msg_123", "att_1")
	if err != nil {
		t.Fatalf("DownloadMailAttachment: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "FILE_CONTENT_BYTES" {
		t.Fatalf("下载内容不符: %q", string(data))
	}
}

// 附件取链接失败：按 failed_reasons 数组找到对应原因放进报错（结构以官方 SDK 为准）。
func TestDownloadMailAttachmentFailedReason(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		return `{"code":0,"data":{"download_urls":[],"failed_ids":["att_1"],"failed_reasons":[{"attachment_id":"att_1","reason":"attachment expired"}]}}`
	})}
	_, err := c.DownloadMailAttachment("ut", "msg_123", "att_1")
	if err == nil || !strings.Contains(err.Error(), "attachment expired") {
		t.Fatalf("应返回带失败原因的错误，实际: %v", err)
	}
}
