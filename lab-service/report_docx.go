package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ---- Отчёт на бланке организации (2026-10-16) ----
//
// Отчёт = сгенерированное содержимое (протокол метода + ИИ-разделы + приложение
// статистики, см. report.go), вставленное в ТЕЛО корпоративного бланка
// standard.docx. Шапка (логотип), футер (реквизиты/QR/водяной знак), тема, шрифты
// и медиа бланка копируются без изменений; заменяется только содержимое между
// <w:body> и <w:sectPr> (splitTemplateBody). Приём перенесён из
// sbe-agent/agent-service/files.go renderDocx (2026-09-08) — доказал себя.

const imageRelationshipType = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/image"

// splitTemplateBody разрезает document.xml шаблона на «всё до содержимого тела»
// (включая открывающий <w:body> и полный набор xmlns на корневом <w:document>)
// и «<w:sectPr>...</w:document>» (поля страницы + ссылки на шапку/футер) —
// сгенерированное содержимое подставляется МЕЖДУ ними.
func splitTemplateBody(docXML string) (prefix, suffix string, err error) {
	bodyOpen := strings.Index(docXML, "<w:body>")
	if bodyOpen < 0 {
		return "", "", fmt.Errorf("<w:body> not found in template document.xml")
	}
	bodyContentStart := bodyOpen + len("<w:body>")
	sectPr := strings.Index(docXML, "<w:sectPr")
	if sectPr < bodyContentStart {
		return "", "", fmt.Errorf("<w:sectPr> not found (or precedes <w:body>) in template document.xml")
	}
	return docXML[:bodyContentStart], docXML[sectPr:], nil
}

var contentTypeDefaultRe = regexp.MustCompile(`<Default[^>]*Extension="([^"]+)"`)

// declaredExtensions — расширения, уже объявленные в [Content_Types].xml бланка
// (чтобы не дублировать Default-записи для медиа содержимого).
func declaredExtensions(contentTypes string) map[string]bool {
	out := map[string]bool{}
	for _, m := range contentTypeDefaultRe.FindAllStringSubmatch(contentTypes, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// reportDocx собирает DOCX отчёта на бланке: тело из buildDocxBodyInner (тот же
// рендер узлов/таблиц/графиков/фото, что у протокола — с общим docxPhotoRegistry,
// поэтому графики/фото встраиваются как word/media/*), поверх — части бланка.
func (s *Server) reportDocx(ctx context.Context, p *protocolData) ([]byte, error) {
	photoRegistry := &docxPhotoRegistry{ctx: ctx, s3: s.s3}
	for i := range p.Methods {
		if p.Methods[i].Ctx != nil {
			p.Methods[i].Ctx.photoRegistry = photoRegistry
		}
	}
	bodyInner := buildDocxBodyInner(p)

	zr, err := zip.NewReader(bytes.NewReader(standardTemplateDocx), int64(len(standardTemplateDocx)))
	if err != nil {
		return nil, fmt.Errorf("open docx template: %w", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("read template entry %s: %w", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read template entry %s: %w", f.Name, err)
		}
		files[f.Name] = b
	}

	docXML, ok := files["word/document.xml"]
	if !ok {
		return nil, fmt.Errorf("docx template: word/document.xml missing")
	}
	prefix, suffix, err := splitTemplateBody(string(docXML))
	if err != nil {
		return nil, err
	}

	relsXML := string(files["word/_rels/document.xml.rels"])
	// Регистр фото/графиков использует Id вида rIdPhotoN — не должен пересечься
	// с уже занятыми бланком rId1..rIdN.
	if strings.Contains(relsXML, `Id="rIdPhoto`) {
		return nil, fmt.Errorf("docx template already uses rIdPhoto ids")
	}
	if relsXML == "" {
		relsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`
	}
	ctXML := string(files["[Content_Types].xml"])
	ctSeen := declaredExtensions(ctXML)

	var relEntries, ctExtra strings.Builder
	for _, img := range photoRegistry.images {
		ext := img.mediaName[strings.LastIndex(img.mediaName, ".")+1:]
		files["word/media/"+img.mediaName] = img.data
		fmt.Fprintf(&relEntries, `<Relationship Id="%s" Type="%s" Target="media/%s"/>`,
			img.relID, imageRelationshipType, img.mediaName)
		if !ctSeen[ext] {
			ctSeen[ext] = true
			fmt.Fprintf(&ctExtra, `<Default Extension="%s" ContentType="%s"/>`, ext, img.contentType)
		}
	}

	files["word/document.xml"] = []byte(prefix + bodyInner + suffix)
	files["word/_rels/document.xml.rels"] = []byte(strings.Replace(relsXML, "</Relationships>", relEntries.String()+"</Relationships>", 1))
	if ctXML != "" {
		files["[Content_Types].xml"] = []byte(strings.Replace(ctXML, "</Types>", ctExtra.String()+"</Types>", 1))
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
