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
		ps := r.URL.Query().Get("page_size")
		if ps != "20" {
			t.Fatalf("page_size 应限制不超过 20, 实际为 %s", ps)
		}
		return `{"code":0,"data":{"has_more":false,"items":["m1","m2"]}}`
	})}

	items, err := c.ListMailboxMessages("ut", 50) // 传入 50 应自动限制为 20
	if err != nil {
		t.Fatalf("ListMailboxMessages: %v", err)
	}
	if len(items) != 2 || items[0] != "m1" || items[1] != "m2" {
		t.Fatalf("解析邮件 ID 列表不符: %v", items)
	}
}

func TestListMailboxMessagesObjectArray(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		return `{"code":0,"data":{"has_more":false,"items":[{"id":"obj_m1"},{"id":"obj_m2"}]}}`
	})}

	items, err := c.ListMailboxMessages("ut", 20)
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

	items, err := c.ListMailboxMessages("ut", 20)
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
					"internal_date": "1700000000000",
					"attachments": [
						{"id": "att_1", "filename": "合同.pdf", "size": 10240, "is_inline": false},
						{"id": "att_2", "filename": "签名.png", "size": 2048, "is_inline": true}
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
	if msg.HeadFrom != "vendor@example.com" {
		t.Fatalf("发件人解析错误: %q", msg.HeadFrom)
	}
	if len(msg.Attachments) != 2 {
		t.Fatalf("附件数量错误: %d", len(msg.Attachments))
	}
	if msg.Attachments[0].Name != "合同.pdf" || msg.Attachments[0].IsInline != false {
		t.Fatalf("附件 1 错误: %+v", msg.Attachments[0])
	}
	if msg.Attachments[1].Name != "签名.png" || msg.Attachments[1].IsInline != true {
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
