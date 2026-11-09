package controlplane

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

const docxMedia = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
const xlsxMedia = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

func documentAttachmentText(data []byte, media string) (string, error) {
	z, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if e != nil {
		return "", fmt.Errorf("the Office file is corrupt or encrypted; upload an unencrypted DOCX or XLSX")
	}
	if len(z.File) > 256 {
		return "", fmt.Errorf("Office file has too many parts; export a smaller document")
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		if f.Flags&1 != 0 {
			return "", fmt.Errorf("encrypted Office files cannot be read; upload an unencrypted copy")
		}
		if f.UncompressedSize64 > 4<<20 {
			return "", fmt.Errorf("Office file expands beyond the safe part limit")
		}
		total += f.UncompressedSize64
		if total > 16<<20 {
			return "", fmt.Errorf("Office file expands beyond 16 MB; export a smaller document")
		}
		if files[f.Name] != nil {
			return "", fmt.Errorf("Office file has duplicate parts")
		}
		files[f.Name] = f
	}
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, fmt.Errorf("Office file is missing %s", name)
		}
		r, e := f.Open()
		if e != nil {
			return nil, e
		}
		defer r.Close()
		b, e := io.ReadAll(io.LimitReader(r, 4<<20+1))
		if e != nil || len(b) > 4<<20 {
			return nil, fmt.Errorf("Office file part cannot be read safely")
		}
		return b, nil
	}
	var out string
	if media == docxMedia {
		b, e := read("word/document.xml")
		if e != nil {
			return "", e
		}
		out, e = officeXMLText(b, true)
		if e != nil {
			return "", e
		}
	} else if media == xlsxMedia {
		shared := []string{}
		if files["xl/sharedStrings.xml"] != nil {
			b, e := read("xl/sharedStrings.xml")
			if e != nil {
				return "", e
			}
			var ss struct {
				Items []struct {
					Text string `xml:"t"`
					Runs []struct {
						Text string `xml:"t"`
					} `xml:"r"`
				} `xml:"si"`
			}
			if e = xml.Unmarshal(b, &ss); e != nil {
				return "", fmt.Errorf("invalid shared spreadsheet text")
			}
			for _, v := range ss.Items {
				s := v.Text
				for _, r := range v.Runs {
					s += r.Text
				}
				shared = append(shared, s)
			}
		}
		wb, e := read("xl/workbook.xml")
		if e != nil {
			return "", e
		}
		var book struct {
			Sheets []struct {
				Name string `xml:"name,attr"`
				RID  string `xml:"id,attr"`
			} `xml:"sheets>sheet"`
		}
		if e = xml.Unmarshal(wb, &book); e != nil || len(book.Sheets) == 0 || len(book.Sheets) > 32 {
			return "", fmt.Errorf("invalid workbook or more than 32 sheets")
		}
		rels, e := read("xl/_rels/workbook.xml.rels")
		if e != nil {
			return "", e
		}
		var rs struct {
			Items []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
				Mode   string `xml:"TargetMode,attr"`
			} `xml:"Relationship"`
		}
		if e = xml.Unmarshal(rels, &rs); e != nil {
			return "", fmt.Errorf("invalid workbook relationships")
		}
		targets := map[string]string{}
		for _, r := range rs.Items {
			if r.Mode == "External" {
				continue
			}
			p := path.Clean(path.Join("xl", r.Target))
			if strings.HasPrefix(r.Target, "/") {
				p = path.Clean(strings.TrimPrefix(r.Target, "/"))
			}
			if strings.HasPrefix(p, "xl/worksheets/") {
				targets[r.ID] = p
			}
		}
		var result strings.Builder
		for _, sheet := range book.Sheets {
			p := targets[sheet.RID]
			if p == "" {
				return "", fmt.Errorf("worksheet relationship is missing or unsupported")
			}
			b, e := read(p)
			if e != nil {
				return "", e
			}
			text, e := spreadsheetXMLText(b, shared)
			if e != nil {
				return "", e
			}
			result.WriteString("Sheet: " + sheet.Name + "\n")
			result.WriteString(text)
			result.WriteString("\n")
			if result.Len() > 64<<10 {
				return "", fmt.Errorf("extracted workbook exceeds 64 KB; upload fewer rows/sheets")
			}
		}
		out = result.String()
	} else {
		return "", fmt.Errorf("unsupported Office format")
	}
	if len(out) > 64<<10 {
		return "", fmt.Errorf("extracted document exceeds 64 KB; upload a smaller section")
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("this file has no readable text; export its content as text or an image")
	}
	return out, nil
}
func officeXMLText(b []byte, paragraphs bool) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	var out strings.Builder
	inText := false
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", fmt.Errorf("invalid document XML")
		}
		switch v := t.(type) {
		case xml.StartElement:
			if v.Name.Local == "t" {
				inText = true
			}
			if v.Name.Local == "tab" {
				out.WriteString("\t")
			}
			if v.Name.Local == "br" {
				out.WriteString("\n")
			}
		case xml.EndElement:
			if v.Name.Local == "t" {
				inText = false
			}
			if paragraphs && (v.Name.Local == "p" || v.Name.Local == "tr") {
				out.WriteString("\n")
			}
			if paragraphs && v.Name.Local == "tc" {
				out.WriteString("\t")
			}
		case xml.CharData:
			if inText {
				out.Write(v)
			}
		}
		if out.Len() > 64<<10 {
			return "", fmt.Errorf("extracted document exceeds 64 KB; upload a smaller section")
		}
	}
	return strings.TrimSpace(out.String()), nil
}
func spreadsheetXMLText(b []byte, shared []string) (string, error) {
	var sheet struct {
		Rows []struct {
			Cells []struct {
				Ref     string `xml:"r,attr"`
				Type    string `xml:"t,attr"`
				Value   string `xml:"v"`
				Formula string `xml:"f"`
				Inline  struct {
					Text string `xml:"t"`
					Runs []struct {
						Text string `xml:"t"`
					} `xml:"r"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if e := xml.Unmarshal(b, &sheet); e != nil {
		return "", fmt.Errorf("invalid worksheet XML")
	}
	if len(sheet.Rows) > 5000 {
		return "", fmt.Errorf("worksheet exceeds 5000 rows; upload a smaller range")
	}
	var out strings.Builder
	w := csv.NewWriter(&out)
	for _, row := range sheet.Rows {
		values := []string{}
		for _, c := range row.Cells {
			col := 0
			for _, r := range c.Ref {
				if r < 'A' || r > 'Z' {
					break
				}
				col = col*26 + int(r-'A'+1)
			}
			if col < 1 || col > 256 {
				return "", fmt.Errorf("worksheet has invalid or more than 256 columns")
			}
			for len(values) < col {
				values = append(values, "")
			}
			v := c.Value
			switch c.Type {
			case "s":
				i, e := strconv.Atoi(v)
				if e != nil || i < 0 || i >= len(shared) {
					return "", fmt.Errorf("invalid shared string index")
				}
				v = shared[i]
			case "inlineStr":
				v = c.Inline.Text
				for _, r := range c.Inline.Runs {
					v += r.Text
				}
			case "b":
				if v == "1" {
					v = "TRUE"
				} else if v == "0" {
					v = "FALSE"
				} else {
					return "", fmt.Errorf("invalid boolean cell")
				}
			}
			if c.Formula != "" {
				v = "[formula: " + c.Formula + "; saved value: " + v + "]"
			}
			values[col-1] = v
		}
		if e := w.Write(values); e != nil {
			return "", e
		}
		w.Flush()
		if out.Len() > 64<<10 {
			return "", fmt.Errorf("extracted worksheet exceeds 64 KB; upload a smaller range")
		}
	}
	return out.String(), w.Error()
}
