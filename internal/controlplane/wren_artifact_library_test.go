package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func libraryArtifact(t *testing.T, s *Store, c Conversation, name, media, content string) WrenArtifact {
	t.Helper()
	v, err := s.PutWrenArtifact(context.Background(), c.WorkspaceID, c.AccountID, WrenArtifactInput{ConversationID: c.ID, Name: name, MediaType: media, Content: []byte(content)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestWrenArtifactLibraryPinsPaginationAndScope(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	hidden := libraryArtifact(t, s, private, "private-secret.csv", "text/csv", "name\nPrivate\n")
	ids := map[string]bool{}
	for i := 0; i < 5; i++ {
		v := libraryArtifact(t, s, shared, fmt.Sprintf("team-%d.csv", i), "text/csv", "name\nShared\n")
		ids[v.ID] = true
	}
	first, err := s.WrenArtifactLibrary(ctx, private.WorkspaceID, member, WrenArtifactLibraryQuery{Limit: 2})
	if err != nil || len(first.Artifacts) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page %+v %v", first, err)
	}
	seen := map[string]bool{}
	page := first
	for {
		for _, v := range page.Artifacts {
			if seen[v.ID] || v.ID == hidden.ID || !ids[v.ID] || v.AuthorName == "" || v.ConversationTitle == "" || len(v.Content) != 0 {
				t.Fatalf("bad library metadata %+v", v)
			}
			seen[v.ID] = true
		}
		if !page.HasMore {
			break
		}
		page, err = s.WrenArtifactLibrary(ctx, private.WorkspaceID, member, WrenArtifactLibraryQuery{Limit: 2, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pagination lost artifacts %d", len(seen))
	}
	selected := first.Artifacts[0]
	pin, err := s.PinWrenArtifact(ctx, private.WorkspaceID, member, selected.ID, true)
	if err != nil || !pin.Pinned || pin.PinnedAt == nil {
		t.Fatalf("pin %+v %v", pin, err)
	}
	again, err := s.PinWrenArtifact(ctx, private.WorkspaceID, member, selected.ID, true)
	if err != nil || !again.PinnedAt.Equal(*pin.PinnedAt) {
		t.Fatalf("pin replay %+v %v", again, err)
	}
	pinned, err := s.WrenArtifactLibrary(ctx, private.WorkspaceID, member, WrenArtifactLibraryQuery{PinnedOnly: true})
	if err != nil || len(pinned.Artifacts) != 1 || pinned.Artifacts[0].ID != selected.ID {
		t.Fatalf("pinned filter %+v %v", pinned, err)
	}
	ownerPins, err := s.WrenArtifactLibrary(ctx, private.WorkspaceID, private.AccountID, WrenArtifactLibraryQuery{PinnedOnly: true})
	if err != nil || len(ownerPins.Artifacts) != 0 {
		t.Fatalf("pins shared across people %+v %v", ownerPins, err)
	}
	literal, err := s.WrenArtifactLibrary(ctx, private.WorkspaceID, member, WrenArtifactLibraryQuery{Query: "%"})
	if err != nil || len(literal.Artifacts) != 0 {
		t.Fatalf("wildcard search broadened %+v %v", literal, err)
	}
	if _, err = s.WrenArtifactLibrary(ctx, private.WorkspaceID, member, WrenArtifactLibraryQuery{ConversationID: private.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private scoped library %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PinWrenArtifact(ctx, private.WorkspaceID, member, selected.ID, false); err != nil {
		t.Fatalf("viewer cannot organize own pins %v", err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenArtifactLibrary(ctx, private.WorkspaceID, member, WrenArtifactLibraryQuery{}); err == nil {
		t.Fatal("revoked person read library")
	}
	if _, err = s.PinWrenArtifact(ctx, private.WorkspaceID, member, selected.ID, true); err == nil {
		t.Fatal("revoked person pinned file")
	}
}
func TestWrenArtifactLibraryCSVEditPreservesHumanCorrectionAndSource(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	run, err := s.EnqueueOperator(ctx, shared, "library-file-source", "Produce a leads file", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	computer := "computer_library_deleted"
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_computers(id,workspace_id,conversation_id,owner_id,scope_kind,name,phase,plan,ssh_key_version,ssh_key_ciphertext,monthly_cents,expires_at) VALUES($1,$2,$3,$4,'room','File computer','ready','{}','test',$5,0,now()+interval '1 hour')`, computer, private.WorkspaceID, shared.ID, private.AccountID, []byte("fixture-not-a-key")); err != nil {
		t.Fatal(err)
	}
	original := []byte("\xef\xbb\xbfname,note\r\n\"Dental,Clinic\",\"Line one\r\nLine two\"\r\n")
	artifact, err := s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ConversationID: shared.ID, Name: "clinic-leads.csv", MediaType: "text/csv", Content: original, Provenance: WrenArtifactProvenance{RunID: run.ID, ComputerID: computer, Path: "/workspace/leads.csv", Sources: []string{"https://example.com/leads"}}})
	if err != nil {
		t.Fatal(err)
	}
	document, err := s.WrenArtifactDocument(ctx, private.WorkspaceID, member, artifact.ID, 0)
	if err != nil || document.Kind != "csv" || !document.Editable || document.Rows == nil || !document.HasBOM || document.LineEnding != "crlf" {
		t.Fatalf("actual CSV document %+v %v", document, err)
	}
	rows := *document.Rows
	if rows[1][0] != "Dental,Clinic" || rows[1][1] != "Line one\nLine two" {
		t.Fatalf("CSV parser lost quoted values %+v", rows)
	}
	rows[1][1] = "Human correction, with \"quotes\"\nand a newline"
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_computers WHERE id=$1`, computer); err != nil {
		t.Fatal(err)
	}
	edited, err := s.EditWrenArtifact(ctx, private.WorkspaceID, member, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 1, Kind: "csv", Rows: &rows})
	if err != nil || edited.Artifact.Revision != 2 || edited.Artifact.AuthorID != member || !reflect.DeepEqual(*edited.Rows, rows) {
		t.Fatalf("human CSV edit %+v %v", edited, err)
	}
	if !reflect.DeepEqual(edited.Artifact.Provenance, artifact.Provenance) {
		t.Fatalf("edit lost original source %+v", edited.Artifact.Provenance)
	}
	latest, err := s.GetWrenArtifact(ctx, private.WorkspaceID, member, artifact.ID, 0)
	if err != nil || !bytes.HasPrefix(latest.Content, []byte{0xef, 0xbb, 0xbf}) || !bytes.Contains(latest.Content, []byte("\r\n")) {
		t.Fatalf("CSV encoding lost %v", err)
	}
	savedOriginal, err := s.GetWrenArtifact(ctx, private.WorkspaceID, member, artifact.ID, 1)
	if err != nil || !bytes.Equal(savedOriginal.Content, original) {
		t.Fatalf("original revision altered %v", err)
	}
	// An agent refreshing from a stale pre-correction version cannot erase it.
	_, err = s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ID: artifact.ID, ConversationID: shared.ID, ExpectedRevision: 1, Name: artifact.Name, MediaType: artifact.MediaType, Content: original, Provenance: artifact.Provenance})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale agent refresh overwrote human correction %v", err)
	}
	forged := artifact.Provenance
	forged.RunID = "run_forged_other_source"
	_, err = s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ID: artifact.ID, ConversationID: shared.ID, ExpectedRevision: 2, Name: artifact.Name, MediaType: artifact.MediaType, Content: original, Provenance: forged})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("changed forged source accepted %v", err)
	}
}
func TestWrenArtifactLibraryConcurrentTextEditAndReadonly(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	artifact := libraryArtifact(t, s, shared, "report.md", "text/markdown", "Original sourced report")
	outcomes := make(chan error, 2)
	var group sync.WaitGroup
	for _, text := range []string{"Human A correction", "Human B correction"} {
		group.Add(1)
		go func(text string) {
			defer group.Done()
			_, e := s.EditWrenArtifact(ctx, private.WorkspaceID, member, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 1, Kind: "text", Text: &text})
			outcomes <- e
		}(text)
	}
	group.Wait()
	close(outcomes)
	success, conflict := 0, 0
	for e := range outcomes {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent overwrite %d %d", success, conflict)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	document, err := s.WrenArtifactDocument(ctx, private.WorkspaceID, member, artifact.ID, 0)
	if err != nil || document.Editable || document.Text == nil || document.Reason == "" {
		t.Fatalf("readonly document %+v %v", document, err)
	}
	text := "Viewer edit"
	if _, err = s.EditWrenArtifact(ctx, private.WorkspaceID, member, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 2, Kind: "text", Text: &text}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer edited file %v", err)
	}
}
func TestWrenArtifactDocumentFormatsAndBoundedEditing(t *testing.T) {
	metadata := WrenArtifact{Name: "data.tsv", MediaType: "text/tab-separated-values", Content: []byte("name\tnote\nDental\t=SUM(1,2)\n")}
	doc := artifactDocument(metadata)
	if !doc.Editable || doc.Delimiter != "\t" || doc.Rows == nil || (*doc.Rows)[1][1] != "=SUM(1,2)" {
		t.Fatalf("TSV literal data changed %+v", doc)
	}
	for _, v := range []WrenArtifact{{Name: "large.txt", MediaType: "text/plain", Content: bytes.Repeat([]byte("x"), WrenArtifactEditMaxBytes+1)}, {Name: "raw.bin", MediaType: "application/octet-stream", Content: []byte{0, 1, 2}}, {Name: "sheet.xlsx", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Content: officeArtifactFixture(t, "xlsx")}, {Name: "too-many-rows.csv", MediaType: "text/csv", Content: []byte(strings.Repeat("record\n", WrenArtifactEditMaxRows+1))}} {
		got := artifactDocument(v)
		if got.Editable || got.Text != nil || got.Rows != nil || got.Reason == "" {
			t.Fatalf("partial/unsupported file editable %+v", got)
		}
	}
	text := "<script>not executed</script>"
	doc = artifactDocument(WrenArtifact{Name: "page.html", MediaType: "text/html", Content: []byte(text)})
	data, err := json.Marshal(doc)
	if err != nil || !doc.Editable || doc.Text == nil || *doc.Text != text || bytes.Contains(data, []byte("<script>")) {
		t.Fatalf("active HTML not escaped in JSON %v", err)
	}
}

func TestWrenArtifactLibraryHTTPContentSafetyEditConflictAndDownload(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	_, _, token, err := s.Signin(ctx, "knowledge-member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	artifact := libraryArtifact(t, s, shared, "page.html", "text/html", "<script>original()</script>")
	pdf := libraryArtifact(t, s, shared, "report.pdf", "application/pdf", "%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+private.WorkspaceID+path, bytes.NewReader(data))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	got := request("GET", "/wren-library?conversationId="+shared.ID, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), artifact.ID) || strings.Contains(got.Body.String(), "original()") {
		t.Fatalf("library metadata leaked content %d %s", got.Code, got.Body)
	}
	got = request("PUT", "/wren-library/"+artifact.ID+"/pin", map[string]bool{"pinned": true})
	if got.Code != 200 {
		t.Fatalf("pin HTTP %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-library/"+artifact.ID+"/edit", nil)
	if got.Code != 200 || !strings.HasPrefix(got.Header().Get("Content-Type"), "application/json") || strings.Contains(got.Body.String(), "<script>") {
		t.Fatalf("unsafe edit GET %d %s", got.Code, got.Body)
	}
	updated := "<h1>A human correction</h1><script>dataOnly()</script>"
	got = request("PUT", "/wren-library/"+artifact.ID+"/edit", WrenArtifactEditInput{ExpectedRevision: 1, Kind: "text", Text: &updated})
	if got.Code != 200 || strings.Contains(got.Body.String(), "<script>") {
		t.Fatalf("edit HTTP %d %s", got.Code, got.Body)
	}
	got = request("PUT", "/wren-library/"+artifact.ID+"/edit", WrenArtifactEditInput{ExpectedRevision: 1, Kind: "text", Text: &updated})
	if got.Code != 409 {
		t.Fatalf("stale edit silently overwrote %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-library/"+artifact.ID+"/edit?revision=1", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"editable":false`) || !strings.Contains(got.Body.String(), "original()") {
		t.Fatalf("historical read view %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-library/"+pdf.ID+"/edit", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"unsupported"`) || !strings.Contains(got.Body.String(), `"editable":false`) {
		t.Fatalf("unsupported pretended editable %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-artifacts/"+artifact.ID+"/content", nil)
	if got.Code != 200 || got.Body.String() != updated || !strings.Contains(got.Header().Get("Content-Security-Policy"), "sandbox") || !strings.HasPrefix(got.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("edited content download %d %s", got.Code, got.Body)
	}
	if err = s.UpdateWorkspaceMember(ctx, private.WorkspaceID, private.AccountID, member, "viewer", false); err != nil {
		t.Fatal(err)
	}
	got = request("GET", "/wren-library/"+artifact.ID+"/edit", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"editable":false`) {
		t.Fatalf("viewer editor %d %s", got.Code, got.Body)
	}
	got = request("PUT", "/wren-library/"+artifact.ID+"/edit", WrenArtifactEditInput{ExpectedRevision: 2, Kind: "text", Text: &updated})
	if got.Code != 403 {
		t.Fatalf("viewer edited %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-library?cursor=invalid", nil)
	if got.Code != 400 {
		t.Fatalf("invalid library cursor %d %s", got.Code, got.Body)
	}
}
func TestWrenArtifactLibraryCSVRoundtripSingleEmptyCellAndLimits(t *testing.T) {
	s, private, _, shared, _ := knowledgeFixture(t)
	ctx := context.Background()
	artifact := libraryArtifact(t, s, shared, "empty-row.csv", "text/csv", "name\nOriginal\n")
	rows := [][]string{{""}}
	edited, err := s.EditWrenArtifact(ctx, private.WorkspaceID, private.AccountID, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 1, Kind: "csv", Rows: &rows})
	if err != nil || edited.Rows == nil || !reflect.DeepEqual(*edited.Rows, rows) {
		t.Fatalf("single empty cell lost %+v %v", edited, err)
	}
	invalid := [][]string{{}}
	if _, err = s.EditWrenArtifact(ctx, private.WorkspaceID, private.AccountID, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 2, Kind: "csv", Rows: &invalid}); err == nil {
		t.Fatal("unrepresentable zero-cell row accepted")
	}
	tooWide := [][]string{make([]string, WrenArtifactEditMaxColumns+1)}
	if _, err = s.EditWrenArtifact(ctx, private.WorkspaceID, private.AccountID, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 2, Kind: "csv", Rows: &tooWide}); err == nil {
		t.Fatal("oversize table accepted")
	}
	literalCR := [][]string{{"source\rdata"}}
	if _, err = s.EditWrenArtifact(ctx, private.WorkspaceID, private.AccountID, artifact.ID, WrenArtifactEditInput{ExpectedRevision: 2, Kind: "csv", Rows: &literalCR}); err == nil {
		t.Fatal("CSV writer could silently drop lone carriage return")
	}
}
