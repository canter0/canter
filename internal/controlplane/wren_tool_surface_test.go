package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrenCanSendWhileToolsViewIsOpen(t *testing.T) {
	s, c, token := operatorFixture(t)
	wren, err := s.SaveWren(context.Background(), c.WorkspaceID, c.AccountID, Wren{ID: "wren_tools_surface", Name: "Reading", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", BaseURL: "http://unused", Model: "platform/managed"}})
	send := func(kind string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/conversations/"+wren.ConversationID+"/messages", strings.NewReader(fmt.Sprintf(`{"requestId":"tools-view-message-%s","message":"Use the tool I added here","surface":{"kind":%q}}`, kind, kind)))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		result := httptest.NewRecorder()
		h.ServeHTTP(result, r)
		return result
	}
	if result := send("wren-extensions"); result.Code != http.StatusAccepted {
		t.Fatalf("tools view blocked sending: %d %s", result.Code, result.Body)
	}
	if result := send("unknown-private-view"); result.Code != http.StatusBadRequest {
		t.Fatalf("unknown view accepted: %d %s", result.Code, result.Body)
	}
}
