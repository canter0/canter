package controlplane

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func officeArtifactFixture(t *testing.T, kind string) []byte {
	t.Helper()
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	main, root, contentType := "xl/workbook.xml", "workbook", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
	if kind == "docx" {
		main, root, contentType = "word/document.xml", "document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
	}
	if kind == "pptx" {
		main, root, contentType = "ppt/presentation.xml", "presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
	}
	files := map[string]string{"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/` + main + `" ContentType="` + contentType + `"/></Types>`, main: `<?xml version="1.0"?><` + root + `/>`}

	files["_rels/.rels"] = `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="` + main + `"/></Relationships>`
	if kind == "xlsx" {
		files[main] = `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`
		files["xl/_rels/workbook.xml.rels"] = `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`
		files["xl/worksheets/sheet1.xml"] = `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Actual cell</t></is></c></row></sheetData></worksheet>`
	}
	for name, content := range files {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func TestWrenArtifactFormatValidation(t *testing.T) {
	for _, test := range []struct {
		name, media string
		content     []byte
	}{
		{"leads.csv", "text/csv", []byte("name,source\nExample,https://example.com\n")},
		{"data.json", "application/json", []byte(`{"name":"Example"}`)},
		{"report.pdf", "application/pdf", []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n")},
		{"work.xlsx", "application/octet-stream", officeArtifactFixture(t, "xlsx")},
		{"report.docx", "application/octet-stream", officeArtifactFixture(t, "docx")},
		{"slides.pptx", "application/octet-stream", officeArtifactFixture(t, "pptx")},
		{"raw.bin", "application/octet-stream", []byte{0, 1, 2, 255}},
		{"empty.txt", "text/plain", nil},
		{"untrusted.html", "text/html", []byte(`<script>alert("not executed")</script>`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := WrenArtifactInput{ID: "artifact_format_test", Name: test.name, MediaType: test.media, Content: test.content}
			if err := validateWrenArtifact(&input); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(test.name, "xlsx") && input.MediaType == "application/octet-stream" {
				t.Fatal("actual Office media type not inferred")
			}
		})
	}
	valid := WrenArtifactInput{ID: "artifact_validation", Name: "notes.txt", MediaType: "text/plain", Content: []byte("hello")}
	cases := []func(*WrenArtifactInput){
		func(v *WrenArtifactInput) { v.ID = "artifact_invalid/path" },
		func(v *WrenArtifactInput) { v.Name = "../../secret.txt" },
		func(v *WrenArtifactInput) { v.Name = "bad\r\nheader.txt" },
		func(v *WrenArtifactInput) {
			v.Name = "looks.xlsx"
			v.MediaType = "application/octet-stream"
			v.Content = []byte("this is not a spreadsheet")
		},
		func(v *WrenArtifactInput) {
			v.Name = "looks.xlsx"
			v.MediaType = "application/octet-stream"
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			w, _ := z.Create("notes.txt")
			_, _ = w.Write([]byte("not OOXML"))
			_ = z.Close()
			v.Content = b.Bytes()
		},
		func(v *WrenArtifactInput) {
			v.Name = "report.pdf"
			v.MediaType = "application/pdf"
			v.Content = []byte("not a pdf")
		},
		func(v *WrenArtifactInput) {
			v.Name = "leads.csv"
			v.MediaType = "text/csv"
			v.Content = []byte("name,source\n\"unterminated")
		},
		func(v *WrenArtifactInput) {
			v.Name = "data.json"
			v.MediaType = "application/json"
			v.Content = []byte("invalid")
		},
		func(v *WrenArtifactInput) { v.MediaType = "text/plain; charset=utf-8" },
		func(v *WrenArtifactInput) { v.Content = bytes.Repeat([]byte("x"), WrenArtifactMaxBytes+1) },
		func(v *WrenArtifactInput) {
			v.Provenance.Sources = []string{"https://user:password@example.com/private"}
		},
		func(v *WrenArtifactInput) { v.Provenance.Path = "/workspace/secret.txt" },
	}
	for i, mutate := range cases {
		v := valid
		mutate(&v)
		if err := validateWrenArtifact(&v); err == nil {
			t.Fatalf("accepted invalid artifact case %d", i)
		}
	}
}

func TestWrenArtifactsRevisionsPrivacyAndComputerLifetime(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	member, otherWorkspace, _, err := s.Signup(ctx, "artifact-member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, member.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	wren, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_artifact_test", Name: "Research", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_artifact_test", Kind: "group", Name: "Shared research", WrenIDs: []string{wren.ID}, MemberIDs: []string{member.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueOperator(ctx, shared, "artifact-root-request", "Make a file", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	computer := "computer_artifact_test"
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_computers(id,workspace_id,conversation_id,owner_id,scope_kind,name,phase,plan,ssh_key_version,ssh_key_ciphertext,monthly_cents,expires_at) VALUES($1,$2,$3,$4,'room','Test computer','ready','{}','test',$5,0,now()+interval '1 hour')`, computer, c.WorkspaceID, shared.ID, c.AccountID, []byte("not-a-real-key")); err != nil {
		t.Fatal(err)
	}
	input := WrenArtifactInput{ID: "artifact_shared_leads", ConversationID: shared.ID, Name: "leads.csv", MediaType: "text/csv", Content: []byte("company,source\nExample,https://example.com\n"), Provenance: WrenArtifactProvenance{RunID: run.ID, ComputerID: computer, Path: "/workspace/leads.csv", Sources: []string{"https://example.com"}}}
	created, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input.Content)
	if created.Revision != 1 || created.LatestRevision != 1 || created.SHA256 != hex.EncodeToString(digest[:]) || !bytes.Equal(created.Content, input.Content) || created.Size != len(input.Content) {
		t.Fatalf("bad saved artifact: %#v", created)
	}
	foreign, err := s.ListWrenArtifacts(ctx, c.WorkspaceID, member.ID, shared.ID)
	if err != nil || len(foreign) != 1 || len(foreign[0].Content) != 0 {
		t.Fatalf("member metadata: %#v %v", foreign, err)
	}
	if _, err = s.GetWrenArtifact(ctx, otherWorkspace.ID, member.ID, created.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace bytes: %v", err)
	}
	changed := input
	changed.ExpectedRevision = 1
	changed.Content = []byte("company,source,note\nExample,https://example.com,Human correction\n")
	updated, err := s.PutWrenArtifact(ctx, c.WorkspaceID, member.ID, changed)
	if err != nil || updated.Revision != 2 || updated.AuthorID != member.ID {
		t.Fatalf("human edit: %#v %v", updated, err)
	}
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale overwrite accepted: %v", err)
	}
	original, err := s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, created.ID, 1)
	if err != nil || !bytes.Equal(original.Content, input.Content) || original.LatestRevision != 2 {
		t.Fatalf("original revision lost: %#v %v", original, err)
	}
	versions, err := s.WrenArtifactRevisions(ctx, c.WorkspaceID, c.AccountID, created.ID)
	if err != nil || len(versions) != 2 || versions[0].Revision != 2 || len(versions[0].Content) != 0 {
		t.Fatalf("version metadata: %#v %v", versions, err)
	}
	// Concurrent stale edits serialize on the room; exactly one revision appends.
	edits := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			edit := changed
			edit.ExpectedRevision = 2
			_, e := s.PutWrenArtifact(ctx, c.WorkspaceID, member.ID, edit)
			edits <- e
		}()
	}
	first, second := <-edits, <-edits
	if !((first == nil && errors.Is(second, ErrConflict)) || (second == nil && errors.Is(first, ErrConflict))) {
		t.Fatalf("concurrent revision outcomes: %v %v", first, second)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_computers WHERE id=$1`, computer); err != nil {
		t.Fatal(err)
	}
	durable, err := s.GetWrenArtifact(ctx, c.WorkspaceID, member.ID, created.ID, 0)
	if err != nil || !bytes.Equal(durable.Content, changed.Content) {
		t.Fatalf("computer deletion removed result: %#v %v", durable, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE account_id=$1 AND workspace_id=$2`, member.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	viewerEdit := changed
	viewerEdit.ExpectedRevision = 3
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, member.ID, viewerEdit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer changed artifact: %v", err)
	}
	if _, err = s.GetWrenArtifact(ctx, c.WorkspaceID, member.ID, created.ID, 0); err != nil {
		t.Fatalf("viewer lost read access: %v", err)
	}
	if err = s.cancelOperator(ctx, shared.ID); err != nil {
		t.Fatal(err)
	}
	room.MemberIDs = nil
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetWrenArtifact(ctx, c.WorkspaceID, member.ID, created.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked download: %v", err)
	}
	if _, err = s.ListWrenArtifacts(ctx, c.WorkspaceID, member.ID, shared.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked list: %v", err)
	}
	if _, err = s.WrenArtifactRevisions(ctx, c.WorkspaceID, member.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked revisions: %v", err)
	}
	privateRun, err := s.EnqueueOperator(ctx, c, "artifact-private-run", "Private request", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	forged := input
	forged.ID = "artifact_forged_source"
	forged.Provenance = WrenArtifactProvenance{RunID: privateRun.ID}
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, forged); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated private run accepted as source: %v", err)
	}
	if _, err = s.GetWrenArtifact(ctx, c.WorkspaceID, "unknown-account", created.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown requester reads bytes: %v", err)
	}
}

func TestWrenArtifactHTTPDownloadsAreAuthenticatedAttachments(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	artifact, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ID: "artifact_http_download", ConversationID: c.ID, Name: "résumé.html", MediaType: "text/html", Content: []byte(`<html><script>fetch('/api/private')</script></html>`)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	request := func(method, suffix, session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-artifacts/"+artifact.ID+suffix, nil)
		w := httptest.NewRecorder()
		r.Header.Set("Origin", "http://canter.test")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
		}
		h.ServeHTTP(w, r)
		return w
	}
	got := request("GET", "/content", token)
	if got.Code != 200 || got.Body.String() != string(artifact.Content) {
		t.Fatalf("download: %d %s", got.Code, got.Body)
	}
	disposition, params, err := mime.ParseMediaType(got.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != artifact.Name {
		t.Fatalf("unsafe disposition: %s %v", got.Header().Get("Content-Disposition"), err)
	}
	if got.Header().Get("Content-Type") != "text/html" || got.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(got.Header().Get("Content-Security-Policy"), "sandbox") || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unsafe download headers: %v", got.Header())
	}
	head := request("HEAD", "/content", token)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != got.Header().Get("Content-Length") {
		t.Fatalf("HEAD: %d %s", head.Code, head.Body)
	}
	meta := request("GET", "", token)
	if meta.Code != 200 || strings.Contains(meta.Body.String(), "fetch(") || strings.Contains(meta.Body.String(), `"content"`) {
		t.Fatalf("metadata leaked active bytes: %d %s", meta.Code, meta.Body)
	}
	if missing := request("GET", "/content", ""); missing.Code != 401 {
		t.Fatalf("unauthenticated content %d", missing.Code)
	}
	if bad := request("GET", "/content?revision=-1", token); bad.Code != 400 {
		t.Fatalf("invalid revision %d", bad.Code)
	}
	if bad := request("GET", "/content?revision=2", token); bad.Code != 404 {
		t.Fatalf("missing revision %d", bad.Code)
	}

	upload := WrenArtifactInput{ID: "artifact_http_uploaded", ConversationID: c.ID, Name: "data.csv", MediaType: "text/csv", Content: []byte("value\n1\n")}
	write := func(method, suffix string, input WrenArtifactInput) *httptest.ResponseRecorder {
		body, e := json.Marshal(input)
		if e != nil {
			t.Fatal(e)
		}
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-artifacts"+suffix, bytes.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if result := write("POST", "", upload); result.Code != 200 {
		t.Fatalf("HTTP upload: %d %s", result.Code, result.Body)
	}
	upload.ExpectedRevision = 1
	upload.Content = []byte("value\n2\n")
	if result := write("PATCH", "/"+upload.ID, upload); result.Code != 200 {
		t.Fatalf("HTTP revision: %d %s", result.Code, result.Body)
	}
	if result := write("PATCH", "/"+upload.ID, upload); result.Code != 409 {
		t.Fatalf("HTTP stale edit: %d %s", result.Code, result.Body)
	}
	// Marshal metadata never serializes the file, including binary output.
	encoded, err := json.Marshal(artifact)
	if err != nil || bytes.Contains(encoded, artifact.Content) {
		t.Fatalf("metadata marshal leaked bytes: %s %v", encoded, err)
	}
	if _, err = io.Copy(io.Discard, got.Result().Body); err != nil {
		t.Fatal(err)
	}
}

func TestWrenArtifactDelegatedSourceReferencesStayWithinOrigin(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	wren, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_artifact_job", Name: "Lead", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, wren.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EnqueueOperator(ctx, origin, "artifact-job-parent", "Create an artifact through a worker", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim lead: %v %v", ok, err)
	}
	job, err := s.CreateWrenJob(ctx, parent, origin, "artifact-worker", "Write a file", "Write the requested report", "test")
	if err != nil {
		t.Fatal(err)
	}
	computer := "computer_artifact_child"
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_computers(id,workspace_id,conversation_id,owner_id,scope_kind,name,phase,plan,ssh_key_version,ssh_key_ciphertext,monthly_cents,expires_at) VALUES($1,$2,$3,$4,'private','Child computer','ready','{}','test',$5,0,now()+interval '1 hour')`, computer, c.WorkspaceID, job.ChildConversationID, c.AccountID, []byte("not-a-real-key")); err != nil {
		t.Fatal(err)
	}
	input := WrenArtifactInput{ID: "artifact_child_result", ConversationID: origin.ID, Name: "worker.txt", MediaType: "text/plain", Content: []byte("A saved worker file."), Provenance: WrenArtifactProvenance{RunID: job.ChildRunID, JobID: job.ID, ComputerID: computer, Path: "/workspace/worker.txt"}}
	saved, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil || saved.Provenance.JobID != job.ID {
		t.Fatalf("authorized child source: %#v %v", saved, err)
	}
	missingGrant := input
	missingGrant.ID = "artifact_missing_grant"
	missingGrant.Provenance.JobID = ""
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, missingGrant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child computer shared without explicit job: %v", err)
	}
	forged := input
	forged.ID = "artifact_forged_origin"
	forged.ConversationID = c.ID
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, forged); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source copied into unrelated conversation: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM operator_conversations WHERE id=$1`, job.ChildConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, saved.ID, 0); err != nil {
		t.Fatalf("published result depended on child/computer lifetime: %v", err)
	}
	forged.ID = "artifact_deleted_child"
	forged.ConversationID = origin.ID
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, forged); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan retained computer granted fresh publication: %v", err)
	}
}
