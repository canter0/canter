package controlplane

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
)

type screenSourceFunc func(context.Context, func([]byte) error) error

func (f screenSourceFunc) ScreenStream(ctx context.Context, frame func([]byte) error) error {
	return f(ctx, frame)
}
func TestComputerScreenStopsWhenViewerSessionIsRevoked(t *testing.T) {
	h, c, token, _ := computerFixture(t)
	ctx := context.Background()
	s := h.service.Store
	a, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_screen_test", Name: "Screen", Shape: "square"}, true)
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, a.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	v = readyComputer(t, h, v)
	request := httptest.NewRequest("GET", "http://canter.test/screen", nil)
	request.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	response := httptest.NewRecorder()
	stopped := false
	source := screenSourceFunc(func(_ context.Context, frame func([]byte) error) error {
		if e := frame([]byte{1, 2, 3}); e != nil {
			t.Fatal(e)
		}
		if e := s.RevokeHumanSession(ctx, token); e != nil {
			t.Fatal(e)
		}
		time.Sleep(1050 * time.Millisecond)
		e := frame([]byte{4, 5, 6})
		stopped = e != nil
		return e
	})
	h.streamComputerScreen(response, request, source, v, c.AccountID)
	raw := response.Body.Bytes()
	if !stopped || len(raw) != 7 || binary.BigEndian.Uint32(raw[:4]) != 3 || response.Header().Get("Content-Type") != wrencomputer.ScreenContentType {
		t.Fatalf("revoked viewer retained stream: stopped=%v bytes=%d", stopped, len(raw))
	}
}
func TestComputerScreenUnsupportedIsExplicitAndDoesNotStream(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "http://canter.test/screen", nil)
	h := &HTTPServer{}
	h.streamComputerScreen(response, request, screenSourceFunc(func(context.Context, func([]byte) error) error { return wrencomputer.ErrScreenUnsupported }), WrenComputer{}, "")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("fallback status=%d", response.Code)
	}
}
