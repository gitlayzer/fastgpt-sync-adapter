package wecom

import (
	"encoding/json"
	"testing"
)

func TestExtractAttachments(t *testing.T) {
	// 1. 普通文件类型
	fileJSON := `{"sdkfileid":"fid_normal","filename":"财务报表.xlsx","filesize":2048,"md5sum":"abc1"}`
	rawFileMsg := `{"msgtype":"file","file":` + fileJSON + `}`
	msgFile := &Message{
		MsgID:   "m1",
		MsgType: "file",
		Raw:     json.RawMessage(rawFileMsg),
	}
	atts := extractAttachments(msgFile)
	if len(atts) != 1 {
		t.Fatalf("应提取 1 个文件附件, 实际: %d", len(atts))
	}
	if atts[0].Name != "财务报表.xlsx" || atts[0].FileID != "fid_normal" {
		t.Fatalf("附件属性不匹配: %+v", atts[0])
	}

	// 2. 合并转发类型（含文档和图片，只提取合规办公文档）
	chatrecordJSON := `{
		"msgtype": "chatrecord",
		"chatrecord": {
			"title": "项目资料转发",
			"item": [
				{
					"type": "ChatRecordText",
					"content": "{\"content\":\"这是相关资料\"}"
				},
				{
					"type": "ChatRecordFile",
					"content": "{\"sdkfileid\":\"fid_cr1\",\"filename\":\"设计方案.docx\",\"filesize\":5000,\"md5sum\":\"md5_1\"}"
				},
				{
					"type": "ChatRecordFile",
					"content": "{\"sdkfileid\":\"fid_cr2\",\"filename\":\"项目说明.pdf\",\"filesize\":3000,\"md5sum\":\"md5_2\"}"
				},
				{
					"type": "ChatRecordFile",
					"content": "{\"sdkfileid\":\"fid_cr3\",\"filename\":\"tool.exe\",\"filesize\":1000}"
				},
				{
					"type": "ChatRecordImage",
					"content": "{\"sdkfileid\":\"fid_img\",\"filesize\":800}"
				}
			]
		}
	}`
	msgCR := &Message{
		MsgID:   "m2",
		MsgType: "chatrecord",
		Raw:     json.RawMessage(chatrecordJSON),
	}
	attsCR := extractAttachments(msgCR)
	if len(attsCR) != 2 {
		t.Fatalf("应提取 2 个合规文档附件（排除 exe 和图片）, 实际: %d", len(attsCR))
	}
	if attsCR[0].Name != "设计方案.docx" || attsCR[0].FileID != "fid_cr1" {
		t.Fatalf("第一个附件属性不匹配: %+v", attsCR[0])
	}
	if attsCR[1].Name != "项目说明.pdf" || attsCR[1].FileID != "fid_cr2" {
		t.Fatalf("第二个附件属性不匹配: %+v", attsCR[1])
	}

	// 3. 图片消息类型（提取为 image_<msgid>.jpg）
	imgJSON := `{"msgtype":"image","image":{"sdkfileid":"fid_img_123","filesize":54321,"md5sum":"imgmd5"}}`
	msgImg := &Message{
		MsgID:   "msg_img_999",
		MsgType: "image",
		Raw:     json.RawMessage(imgJSON),
	}
	attsImg := extractAttachments(msgImg)
	if len(attsImg) != 1 {
		t.Fatalf("应提取 1 个图片附件, 实际: %d", len(attsImg))
	}
	if attsImg[0].Name != "image_msg_img_999.jpg" || attsImg[0].FileID != "fid_img_123" || attsImg[0].Size != 54321 {
		t.Fatalf("图片附件属性不匹配: %+v", attsImg[0])
	}
}
