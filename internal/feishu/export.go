// 飞书云文档导出：把 docx/doc/sheet/bitable 导出成源文件（.docx/.xlsx）。
//
// 为什么用导出而不是 raw_content：
//
//	raw_content 只返回纯文本，会丢掉图片、表格样式、内嵌附件。
//	导出得到的是完整源文件，可原样归档，不丢内容。
//
// 导出是异步三步：
//  1. POST /drive/v1/export_tasks          创建任务，拿 ticket
//  2. GET  /drive/v1/export_tasks/{ticket} 轮询，job_status=0 表示完成，拿 file_token
//  3. GET  /drive/v1/export_tasks/file/{file_token}/download  下载字节流
package feishu

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// 导出任务状态：0 成功，1 初始化，2 处理中，3 失败
const (
	exportStatusDone = 0
	exportStatusFail = 3
	exportPollTimes  = 60              // 最多轮询次数（120秒，适应大表格导出）
	exportPollEvery  = 2 * time.Second // 轮询间隔
)

// ExportFile 创建导出任务并等待完成，返回可下载的 file_token。
// docType 取 file 之外的云文档类型：docx / doc / sheet / bitable。
// tok 是调用身份（user_access_token 或 tenant_access_token）。
func (c *Client) ExportFile(tok, token, docType string) (string, error) {
	ext, ok := exportExt(docType)
	if !ok {
		return "", fmt.Errorf("类型 %s 不支持导出", docType)
	}
	body := map[string]any{
		"file_extension": ext,
		"token":          token,
		"type":           docType,
	}
	var created struct {
		apiResp
		Data struct {
			Ticket string `json:"ticket"`
		} `json:"data"`
	}
	if err := c.postJSON(baseURL+"/drive/v1/export_tasks", body, tok, &created); err != nil {
		return "", fmt.Errorf("创建导出任务: %w", err)
	}
	ticket := created.Data.Ticket
	if ticket == "" {
		return "", fmt.Errorf("创建导出任务未返回 ticket")
	}

	// 轮询直到完成
	for i := 0; i < exportPollTimes; i++ {
		time.Sleep(exportPollEvery)

		var st struct {
			apiResp
			Data struct {
				Result struct {
					JobStatus int    `json:"job_status"`
					JobError  string `json:"job_error_msg"`
					FileToken string `json:"file_token"`
				} `json:"result"`
			} `json:"data"`
		}
		// 查询接口必须带 token 查询参数（要导出的云文档 token），
		// 漏了会报 99992402 field validation failed
		q := url.Values{}
		q.Set("token", token)
		u := baseURL + "/drive/v1/export_tasks/" + ticket + "?" + q.Encode()
		if err := c.getWithToken(u, tok, &st); err != nil {
			return "", fmt.Errorf("查询导出任务: %w", err)
		}
		switch st.Data.Result.JobStatus {
		case exportStatusDone:
			if st.Data.Result.FileToken == "" {
				return "", fmt.Errorf("导出完成但未返回 file_token")
			}
			return st.Data.Result.FileToken, nil
		case exportStatusFail:
			return "", fmt.Errorf("导出失败: %s", st.Data.Result.JobError)
		}
	}
	return "", fmt.Errorf("导出超时（轮询 %d 次）", exportPollTimes)
}

// DownloadExport 下载导出完成后的文件字节流。
func (c *Client) DownloadExport(tok, fileToken string) (io.ReadCloser, error) {
	u := baseURL + "/drive/v1/export_tasks/file/" + fileToken + "/download"
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		return nil, fmt.Errorf("下载导出文件 HTTP %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// exportExt 文档类型 → 导出扩展名。
func exportExt(docType string) (string, bool) {
	switch docType {
	case "docx", "doc":
		return "docx", true
	case "sheet", "bitable":
		return "xlsx", true
	case "slides":
		return "pptx", true
	}
	return "", false
}
