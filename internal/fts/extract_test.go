package fts

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

func TestExtractPlainText(t *testing.T) {
	got := extractAttachmentText("note.txt", "text/plain", []byte("hello 世界\nsecond line"))
	if !strings.Contains(got, "hello") || !strings.Contains(got, "世界") {
		t.Fatalf("text = %q", got)
	}
}

func TestExtractDocx(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><w:document><w:body><w:p><w:r><w:t>季度财务报表</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	got := extractAttachmentText("report.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", buf.Bytes())
	if !strings.Contains(got, "季度财务报表") {
		t.Fatalf("docx text = %q", got)
	}
}

func TestExtractXlsxSharedStrings(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("xl/sharedStrings.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><sst><si><t>预算批复金额</t></si></sst>`))
	zw.Close()
	got := extractAttachmentText("budget.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buf.Bytes())
	if !strings.Contains(got, "预算批复金额") {
		t.Fatalf("xlsx text = %q", got)
	}
}

func TestExtractPptxSlides(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ppt/slides/slide1.xml")
	_, _ = w.Write([]byte(`<a:t>汇报提纲</a:t>`))
	zw.Close()
	got := extractAttachmentText("deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", buf.Bytes())
	if !strings.Contains(got, "汇报提纲") {
		t.Fatalf("pptx text = %q", got)
	}
}

func TestExtractPDF(t *testing.T) {
	content := "BT /F1 12 Tf (涉密会议纪要) Tj ET"
	var fl bytes.Buffer
	zw := zlib.NewWriter(&fl)
	_, _ = zw.Write([]byte(content))
	zw.Close()
	pdf := []byte("4 0 obj << /Length 100 /Filter /FlateDecode >> stream\r\n" +
		string(fl.Bytes()) + "\r\nendstream endobj")
	got := extractAttachmentText("minute.pdf", "application/pdf", pdf)
	if !strings.Contains(got, "涉密会议纪要") {
		t.Fatalf("pdf text = %q", got)
	}
}

func TestExtractPDFTJArray(t *testing.T) {
	content := "BT [(涉) 12 (密) -3 (文件)] TJ ET"
	var fl bytes.Buffer
	zw := zlib.NewWriter(&fl)
	_, _ = zw.Write([]byte(content))
	zw.Close()
	pdf := []byte("stream\r\n" + string(fl.Bytes()) + "\r\nendstream")
	got := pdfText(pdf)
	if !strings.Contains(got, "涉密文件") {
		t.Fatalf("pdf tj text = %q", got)
	}
}

func TestExtractUnknown(t *testing.T) {
	if got := extractAttachmentText("bin.dat", "application/octet-stream", []byte{0, 1, 2}); got != "" {
		t.Fatalf("unknown type returned %q", got)
	}
}
