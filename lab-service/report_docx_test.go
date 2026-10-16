package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func unzipEntry(t *testing.T, data []byte, name string) (string, bool) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b), true
	}
	return "", false
}

func TestSplitTemplateBodyRealTemplate(t *testing.T) {
	docXML, ok := unzipEntry(t, standardTemplateDocx, "word/document.xml")
	if !ok {
		t.Fatal("template word/document.xml missing")
	}
	prefix, suffix, err := splitTemplateBody(docXML)
	if err != nil {
		t.Fatalf("splitTemplateBody: %v", err)
	}
	if !strings.Contains(prefix, "<w:body>") {
		t.Fatal("prefix must end with <w:body>")
	}
	if !strings.HasPrefix(suffix, "<w:sectPr") {
		t.Fatalf("suffix must start with <w:sectPr>, got %.40q", suffix)
	}
}

func TestDeclaredExtensions(t *testing.T) {
	ct := `<?xml version="1.0"?><Types xmlns="x"><Default Extension="rels" ContentType="a"/><Default Extension="png" ContentType="b"/><Override PartName="/word/document.xml" ContentType="c"/></Types>`
	got := declaredExtensions(ct)
	if !got["rels"] || !got["png"] {
		t.Fatalf("expected rels/png declared: %+v", got)
	}
	if got["jpg"] {
		t.Fatal("jpg must not be declared")
	}
}

func TestStaticTableDocxHasTblGrid(t *testing.T) {
	ctx := &placeholderCtx{}
	rows := [][][]InlineNode{
		{{textNode("A")}, {textNode("B")}},
		{{textNode("1")}, {textNode("2")}},
	}
	out := renderStaticTableDocx(ctx, rows)
	if !strings.Contains(out, "<w:tblGrid><w:gridCol/><w:gridCol/></w:tblGrid>") {
		t.Fatalf("tblGrid missing/wrong (Word needs it — CT_Tbl): %s", out)
	}
	if strings.Index(out, "<w:tblGrid>") > strings.Index(out, "<w:tr>") {
		t.Fatal("tblGrid must come after tblPr and before rows")
	}
}

func TestReportDocxBuildsValidPackage(t *testing.T) {
	ctx := &placeholderCtx{}
	p := &protocolData{
		Methods: []protocolMethod{{
			Ctx: ctx,
			Blocks: []DocumentBlock{{
				ID:             "b1",
				ShowInProtocol: true,
				Content: []RichNode{{
					Type:     "paragraph",
					Children: []InlineNode{{Type: "text", Text: "Проверка отчёта"}},
				}},
			}},
		}},
	}

	data, err := (&Server{}).reportDocx(context.Background(), p)
	if err != nil {
		t.Fatalf("reportDocx: %v", err)
	}

	docXML, ok := unzipEntry(t, data, "word/document.xml")
	if !ok {
		t.Fatal("report word/document.xml missing")
	}
	if !strings.Contains(docXML, "Проверка отчёта") {
		t.Fatal("report body content missing")
	}
	if !strings.Contains(docXML, "<w:body>") {
		t.Fatal("report must keep template <w:body>")
	}
	// Бланк сохранён: ссылка на шапку приходит из суффикса шаблона.
	if !strings.Contains(docXML, "headerReference") {
		t.Fatal("template headerReference lost — letterhead not kept")
	}
	if _, ok := unzipEntry(t, data, "_rels/.rels"); !ok {
		t.Fatal("_rels/.rels missing (invalid OPC package)")
	}
	if ct, ok := unzipEntry(t, data, "[Content_Types].xml"); !ok || !strings.Contains(ct, "</Types>") {
		t.Fatal("[Content_Types].xml missing/invalid")
	}
}
