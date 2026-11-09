package controlplane

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func officeFixture(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for n, text := range parts {
		f, e := w.Create(n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write([]byte(text)); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestOfficeAttachmentsExtractRealContentAndRejectMalformed(t *testing.T) {
	doc := officeFixture(t, map[string]string{"word/document.xml": `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Clinic opening checklist</w:t></w:r></w:p><w:p><w:r><w:t>Check booking </w:t></w:r><w:r><w:t>workflow.</w:t></w:r></w:p></w:body></w:document>`})
	text, e := documentAttachmentText(doc, docxMedia)
	if e != nil || text != "Clinic opening checklist\nCheck booking workflow." {
		t.Fatalf("docx %q %v", text, e)
	}
	xlsx := officeFixture(t, map[string]string{"xl/workbook.xml": `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Launch" r:id="rId1"/></sheets></workbook>`, "xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`, "xl/sharedStrings.xml": `<sst><si><t>Task</t></si><si><r><t>Test </t></r><r><t>booking</t></r></si></sst>`, "xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="A1" t="s"><v>0</v></c><c r="C1" t="inlineStr"><is><t>Status</t></is></c></row><row><c r="A2" t="s"><v>1</v></c><c r="C2" t="b"><v>1</v></c></row><row><c r="A3"><f>1+2</f><v>3</v></c></row></sheetData></worksheet>`})
	text, e = documentAttachmentText(xlsx, xlsxMedia)
	if e != nil || !strings.Contains(text, "Task,,Status\nTest booking,,TRUE") || !strings.Contains(text, "saved value: 3") {
		t.Fatalf("xlsx %q %v", text, e)
	}
	for _, tc := range []struct {
		data  []byte
		media string
	}{{doc, docxMedia}, {xlsx, xlsxMedia}} {
		a := OperatorAttachment{ID: "office_actual", Name: "test", MediaType: tc.media, DataBase64: base64.StdEncoding.EncodeToString(tc.data), Size: len(tc.data)}
		if e := validateOperatorAttachments([]OperatorAttachment{a}); e != nil {
			t.Fatal(e)
		}
		parts := operatorModelPayload(modelMessage{Role: "user", Content: "Read this", Attachments: []OperatorAttachment{a}})["content"].([]any)
		if !strings.Contains(parts[1].(map[string]any)["text"].(string), "booking") {
			t.Fatal("model did not receive extracted text")
		}
	}
	for _, data := range [][]byte{[]byte("not an Office file"), officeFixture(t, map[string]string{"word/document.xml": "<document><bad>"}), officeFixture(t, map[string]string{"word/document.xml": "<document><p><t>" + strings.Repeat("a", 65537) + "</t></p></document>"})} {
		if _, e = documentAttachmentText(data, docxMedia); e == nil {
			t.Fatal("bad/oversize Office input accepted")
		}
	}
}
