package extract

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

// 可直接按文本读取的扩展名
var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true,
	".json": true, ".jsonl": true, ".csv": true, ".tsv": true,
	".log": true, ".xml": true, ".yaml": true, ".yml": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".java": true,
	".c": true, ".h": true, ".cpp": true, ".rs": true, ".sh": true,
	".html": true, ".htm": true, ".css": true, ".sql": true,
	".ini": true, ".conf": true, ".cfg": true, ".env": true,
}

// IsText 判断是否可直接按文本读取。
func IsText(path string) bool {
	return textExts[strings.ToLower(filepath.Ext(path))]
}

// Text 提取文件的纯文本内容，供 FastGPT 索引。
// 文本类直接读；PDF/Office 文档解析为纯文本；其余类型返回占位说明。
func Text(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))

	if IsText(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	switch ext {
	case ".pdf":
		return pdfText(path)
	case ".docx":
		return officeXMLText(path, "word/document.xml")
	case ".pptx":
		return pptxText(path)
	case ".xlsx":
		return xlsxText(path)
	}

	return placeholder(path), nil
}

// placeholder 对不支持的类型返回可读说明。
func placeholder(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	return fmt.Sprintf("[unsupported file: %s, ext=%s, size=%d bytes]", filepath.Base(path), ext, size)
}

// clean 去掉控制字符（如 \x00），并压缩多余空白。
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 0 || (r < 32 && r != '\n' && r != '\t') {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pdfText 用 ledongthuc/pdf 提取文本型 PDF 的文字。
func pdfText(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	total := r.NumPage()
	for i := 1; i <= total; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			continue
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	out := strings.TrimSpace(clean(b.String()))
	if out == "" {
		return placeholder(path) + " [无可提取文本层，疑似扫描件/图片型 PDF，需 OCR]", nil
	}
	return out, nil
}

// officeXMLText 解析 docx 等 OOXML：zip 里指定 XML 中提取所有 <w:t> 文本。
func officeXMLText(path, mainXML string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != mainXML {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		text := extractWT(rc)
		out := strings.TrimSpace(text)
		if out == "" {
			return placeholder(path), nil
		}
		return out, nil
	}
	return "", fmt.Errorf("%s not found in archive", mainXML)
}

// extractWT 从 XML 流中抓取 <w:t> 与 <a:t> 的文本，段落边界补换行。
func extractWT(r io.Reader) string {
	dec := xml.NewDecoder(r)
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			// <w:t>、<a:t> 文本节点；<w:p>/<a:p> 段落结束补换行
			if t.Name.Local == "t" {
				var s string
				if err := dec.DecodeElement(&s, &t); err == nil {
					b.WriteString(s)
				}
			} else if t.Name.Local == "p" {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// pptxText 遍历 ppt/slides/slideN.xml 提取每页文本。
func pptxText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	var b strings.Builder
	slideIdx := 0
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "ppt/slides/slide") && strings.HasSuffix(f.Name, ".xml") {
			slideIdx++
			rc, err := f.Open()
			if err != nil {
				continue
			}
			txt := strings.TrimSpace(extractWT(rc))
			rc.Close()
			if txt != "" {
				b.WriteString(fmt.Sprintf("# 幻灯片 %d\n", slideIdx))
				b.WriteString(txt)
				b.WriteString("\n\n")
			}
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return placeholder(path), nil
	}
	return out, nil
}

// xlsxText 用 excelize 把 Excel 解析为「工作表 + 行」结构。
func xlsxText(path string) (string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return "", fmt.Errorf("open xlsx: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	for _, sheet := range f.GetSheetList() {
		b.WriteString("# 工作表: ")
		b.WriteString(sheet)
		b.WriteString("\n")
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		for _, row := range rows {
			var cells []string
			for _, c := range row {
				cells = append(cells, strings.TrimSpace(c))
			}
			line := strings.Join(cells, " | ")
			if strings.Trim(line, "| ") != "" {
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return placeholder(path), nil
	}
	return out, nil
}
