package controlplane

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func audioTestWAV(seconds int) []byte {
	n := seconds * 16000 * 2
	b := make([]byte, 44+n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	return b
}

type audioRoundTrip func(*http.Request) (*http.Response, error)

func (f audioRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDictationWAVValidatesActualDurationAndContainer(t *testing.T) {
	for _, sec := range []int{1, 60} {
		got, e := dictationWAVDuration(audioTestWAV(sec))
		if e != nil || got != float64(sec) {
			t.Fatalf("duration %v %v", got, e)
		}
	}
	bad := audioTestWAV(1)
	binary.LittleEndian.PutUint32(bad[28:], 1)
	for _, b := range [][]byte{audioTestWAV(61), bad, append(audioTestWAV(1), 0), []byte("not audio"), audioTestWAV(0)} {
		if _, e := dictationWAVDuration(b); e == nil {
			t.Fatal("accepted malformed or unbounded WAV")
		}
	}
}
func TestWrenAudioHTTPAuthorizationLimitsAndEditableTranscript(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	calls := 0
	transport := audioRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://openrouter.ai/api/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer secret-audio-key" {
			t.Fatal("wrong provider or credential")
		}
		var body struct {
			Model string `json:"model"`
			Audio struct {
				Data   string `json:"data"`
				Format string `json:"format"`
			} `json:"input_audio"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "nvidia/nemotron-3.5-transcribe" || body.Audio.Format != "wav" || body.Audio.Data == "" {
			t.Fatal("wrong fixedmodel/audio")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":"Draft a website review."}`)), Header: make(http.Header)}, nil
	})
	call := func(config WrenAudioConfig, tok, id, media, method string, b []byte) *httptest.ResponseRecorder {
		h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", Audio: config})
		r := httptest.NewRequest(method, "/v1/workspaces/"+c.WorkspaceID+"/conversations/"+id+"/dictation", bytes.NewReader(b))
		if tok != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: tok})
		}
		r.Header.Set("Origin", "https://canter.test")
		r.Header.Set("Content-Type", media)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	cfg := WrenAudioConfig{APIKey: "secret-audio-key", transport: transport}
	for _, tc := range []struct {
		name, token, id, media string
		body                   []byte
		want                   int
	}{{"unauthenticated", "", c.ID, "audio/wav", audioTestWAV(1), 401}, {"missingconversation", token, "foreign", "audio/wav", audioTestWAV(1), 404}, {"wrongmedia", token, c.ID, "audio/webm", audioTestWAV(1), 415}, {"invalid", token, c.ID, "audio/wav", []byte("bad"), 400}, {"duration", token, c.ID, "audio/wav", audioTestWAV(61), 400}, {"size", token, c.ID, "audio/wav", make([]byte, maxDictationBytes+1), 413}} {
		t.Run(tc.name, func(t *testing.T) {
			w := call(cfg, tc.token, tc.id, tc.media, "POST", tc.body)
			if w.Code != tc.want {
				t.Fatalf("HTTP%d %s", w.Code, w.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatal("unauthorized/invalid audio reached paid provider")
	}
	if w := call(WrenAudioConfig{}, token, c.ID, "audio/wav", "POST", audioTestWAV(1)); w.Code != 503 {
		t.Fatalf("missingconfig %d", w.Code)
	}
	w := call(cfg, token, c.ID, "audio/wav", "POST", audioTestWAV(1))
	var result struct {
		Text            string
		Editable, Sent  bool
		DurationSeconds float64
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Text != "Draft a website review." || !result.Editable || result.Sent || result.DurationSeconds != 1 || calls != 1 {
		t.Fatalf("transcript %d %s calls%d", w.Code, w.Body.String(), calls)
	}
	var messages int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages WHERE conversation_id=$1`, c.ID).Scan(&messages)
	if messages != 0 {
		t.Fatal("dictation auto-sent conversation content")
	}
	other, _, otherToken, e := s.Signup(ctx, "audio-other@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role)VALUES($1,$2,'operator')`, other.ID, c.WorkspaceID); e != nil {
		t.Fatal(e)
	}
	if w := call(cfg, otherToken, c.ID, "audio/wav", "POST", audioTestWAV(1)); w.Code != 404 {
		t.Fatalf("private scope leaked %d", w.Code)
	}
	s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE account_id=$1 AND workspace_id=$2`, c.AccountID, c.WorkspaceID)
	if w := call(cfg, token, c.ID, "audio/wav", "POST", audioTestWAV(1)); w.Code != 403 {
		t.Fatalf("viewer paid use %d", w.Code)
	}
	if calls != 1 {
		t.Fatal("unauthorized call billed")
	}
}
func TestWrenAudioUpstreamFailureDoesNotExposeSecrets(t *testing.T) {
	s, c, token := operatorFixture(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", Audio: WrenAudioConfig{APIKey: "upstream-private-key", transport: audioRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"upstream-private-key provider diagnostic"}`))}, nil
	})}})
	r := httptest.NewRequest("POST", "/v1/workspaces/"+c.WorkspaceID+"/conversations/"+c.ID+"/dictation", bytes.NewReader(audioTestWAV(1)))
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	r.Header.Set("Origin", "https://canter.test")
	r.Header.Set("Content-Type", "audio/wav")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 502 || strings.Contains(w.Body.String(), "upstream-private-key") || strings.Contains(w.Body.String(), "diagnostic") {
		t.Fatalf("unsafe error %d %s", w.Code, w.Body.String())
	}
}

func TestWrenAudioRechecksRevocationBeforeReturningTranscript(t *testing.T) {
	s, c, token := operatorFixture(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", Audio: WrenAudioConfig{APIKey: "test", transport: audioRoundTrip(func(*http.Request) (*http.Response, error) {
		_, e := s.pool.Exec(context.Background(), `UPDATE memberships SET role='viewer' WHERE account_id=$1 AND workspace_id=$2`, c.AccountID, c.WorkspaceID)
		if e != nil {
			t.Fatal(e)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":"private transcript"}`))}, nil
	})}})
	r := httptest.NewRequest("POST", "/v1/workspaces/"+c.WorkspaceID+"/conversations/"+c.ID+"/dictation", bytes.NewReader(audioTestWAV(1)))
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	r.Header.Set("Origin", "https://canter.test")
	r.Header.Set("Content-Type", "audio/wav")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), "private transcript") {
		t.Fatalf("revoked writer got transcript %d %s", w.Code, w.Body.String())
	}
}

func TestWrenAudioBoundedCallsAndConcurrentUploads(t *testing.T) {
	s, c, token := operatorFixture(t)
	calls := 0
	handler := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", Audio: WrenAudioConfig{APIKey: "test", transport: audioRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":"draft"}`))}, nil
	})}}).(*HTTPServer)
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/workspaces/"+c.WorkspaceID+"/conversations/"+c.ID+"/dictation", bytes.NewReader(audioTestWAV(1)))
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		r.Header.Set("Origin", "https://canter.test")
		r.Header.Set("Content-Type", "audio/wav")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	handler.audioUploads <- struct{}{}
	handler.audioUploads <- struct{}{}
	if w := call(); w.Code != 429 || calls != 0 {
		t.Fatal("over-capacity retained or billed audio")
	}
	<-handler.audioUploads
	<-handler.audioUploads
	handler.limits = newRequestLimiter()
	for i := 0; i < 6; i++ {
		if w := call(); w.Code != 200 {
			t.Fatalf("allowed call %d got%d", i, w.Code)
		}
	}
	if w := call(); w.Code != 429 || calls != 6 {
		t.Fatalf("request cap failed %d calls%d", w.Code, calls)
	}
}

func TestWrenAudioIncrementalSegmentsKeepDurationAndDraftBoundaries(t *testing.T) {
	s, c, token := operatorFixture(t)
	calls := 0
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", Audio: WrenAudioConfig{APIKey: "test-only", transport: audioRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":"A short spoken phrase."}`)), Header: make(http.Header)}, nil
	})}})
	call := func(method string, seconds int) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/workspaces/"+c.WorkspaceID+"/conversations/"+c.ID+"/dictation?segment=1", bytes.NewReader(audioTestWAV(seconds)))
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		r.Header.Set("Origin", "https://canter.test")
		r.Header.Set("Content-Type", "audio/wav")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ready := call("GET", 1)
	var capabilities struct {
		Incremental       bool
		MaxSegmentSeconds int
		AutoSend          bool
	}
	if json.Unmarshal(ready.Body.Bytes(), &capabilities) != nil || ready.Code != 200 || !capabilities.Incremental || capabilities.MaxSegmentSeconds != 6 || capabilities.AutoSend {
		t.Fatalf("bad readiness: %d", ready.Code)
	}
	if w := call("POST", 7); w.Code != 400 || calls != 0 {
		t.Fatalf("oversize segment reached provider: %d calls=%d", w.Code, calls)
	}
	// More than six phrases must work in one recording without borrowing the
	// much larger whole-recording allowance to evade the six-second audio cap.
	for i := 0; i < 7; i++ {
		w := call("POST", 1)
		var value struct {
			Text           string
			Editable, Sent bool
		}
		if json.Unmarshal(w.Body.Bytes(), &value) != nil || w.Code != 200 || !value.Editable || value.Sent {
			t.Fatalf("phrase %d: %d", i, w.Code)
		}
	}
	if calls != 7 {
		t.Fatalf("provider calls=%d", calls)
	}
	var messages int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM operator_messages WHERE conversation_id=$1`, c.ID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("dictation sent a message: %d %v", messages, err)
	}
}
