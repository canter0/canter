package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

func TestOperatorRepositoryNamesCannotChooseAnArbitraryHost(t *testing.T) {
	for _, value := range []string{"@repo", "https://github.com.evil.test/o/r", "https://secret@github.com/o/r", "https://github.com/o/r?token=secret", "o/../r", "http://127.0.0.1/a", "o/r/extra"} {
		if _, err := normalizeRepository(value); err == nil {
			t.Errorf("accepted invalid repository %q", value)
		}
	}
	for _, value := range []string{"@mdn/beginner-html-site-styled", "https://github.com/mdn/beginner-html-site-styled.git"} {
		repo, err := normalizeRepository(value)
		if err != nil || repo != "mdn/beginner-html-site-styled" {
			t.Errorf("valid repository: %s %v", repo, err)
		}
	}
}
func TestOperatorStaticArchiveValidation(t *testing.T) {
	archive := func(extra string, kind byte) []byte {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		for _, entry := range []struct {
			name    string
			kind    byte
			content string
		}{{"root/index.html", tar.TypeReg, "<h1>real site</h1>"}, {extra, kind, "x"}} {
			if entry.name == "" {
				continue
			}
			h := &tar.Header{Name: entry.name, Mode: 0644, Typeflag: entry.kind}
			if entry.kind == tar.TypeReg {
				h.Size = int64(len(entry.content))
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if h.Size > 0 {
				if _, err := tw.Write([]byte(entry.content)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	for _, entry := range []struct {
		name string
		kind byte
	}{{"root/../outside", tar.TypeReg}, {"root/linked", tar.TypeSymlink}, {"root/package.json", tar.TypeReg}, {"root/index.html", tar.TypeReg}} {
		if _, err := staticRepositoryFiles(archive(entry.name, entry.kind), ""); err == nil {
			t.Errorf("unsafe/static-incompatible entry accepted: %s", entry.name)
		}
	}
	files, err := staticRepositoryFiles(archive("root/.env", tar.TypeReg), "")
	if err != nil || len(files) != 1 || string(files["index.html"]) != "<h1>real site</h1>" {
		t.Fatalf("wrong static payload: %v %v", files, err)
	}
	if _, err := staticRepositoryFiles(archive("", tar.TypeReg), "dist"); err == nil {
		t.Fatal("missing build output accepted")
	}
	valid := archive("", tar.TypeReg)
	badChecksum := append([]byte(nil), valid...)
	badChecksum[len(badChecksum)-8] ^= 0xff
	secondMember := append(append([]byte(nil), valid...), valid...)
	trailing := append(append([]byte(nil), valid...), 0x00)
	compressedReader, err := gzip.NewReader(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(compressedReader)
	if err != nil {
		t.Fatal(err)
	}
	if err := compressedReader.Close(); err != nil {
		t.Fatal(err)
	}
	plain = append(plain, 0x41)
	var tailed bytes.Buffer
	taileder := gzip.NewWriter(&tailed)
	if _, err := taileder.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := taileder.Close(); err != nil {
		t.Fatal(err)
	}
	for name, malformed := range map[string][]byte{"checksum": badChecksum, "second gzip member": secondMember, "trailing byte": trailing, "nonzero tar tail": tailed.Bytes()} {
		if _, err := staticRepositoryFiles(malformed, ""); err == nil {
			t.Errorf("accepted repository archive with invalid %s", name)
		}
	}
}
