package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testTextAttachment() OperatorAttachment {
	return OperatorAttachment{ID: "file-one", Name: "notes.md", MediaType: "text/plain", DataBase64: base64.StdEncoding.EncodeToString([]byte("hello")), Size: 5}
}

func TestOperatorAttachmentValidation(t *testing.T) {
	valid := testTextAttachment()
	if err := validateOperatorAttachments([]OperatorAttachment{valid}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*OperatorAttachment){
		"wrong size":       func(a *OperatorAttachment) { a.Size++ },
		"forged image":     func(a *OperatorAttachment) { a.MediaType = "image/png" },
		"unsupported":      func(a *OperatorAttachment) { a.MediaType = "image/svg+xml" },
		"invalid encoding": func(a *OperatorAttachment) { a.DataBase64 = "!!!" },
		"binary text": func(a *OperatorAttachment) {
			a.DataBase64 = base64.StdEncoding.EncodeToString([]byte{0, 1, 2})
			a.Size = 3
		},
		"oversized text": func(a *OperatorAttachment) {
			a.DataBase64 = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 65537)))
			a.Size = 65537
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := valid
			mutate(&a)
			if validateOperatorAttachments([]OperatorAttachment{a}) == nil {
				t.Fatal("invalid attachment accepted")
			}
		})
	}
	if validateOperatorAttachments([]OperatorAttachment{valid, valid}) == nil {
		t.Fatal("duplicate attachment IDs accepted")
	}
	if validateOperatorAttachments(make([]OperatorAttachment, 5)) == nil {
		t.Fatal("too many attachments accepted")
	}
}

func TestOperatorAttachmentProviderPayloadAndCheckpoint(t *testing.T) {
	image := OperatorAttachment{ID: "image", Name: "pixel.png", MediaType: "image/png", DataBase64: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aN2kAAAAASUVORK5CYII=", Size: 68}
	decoded, _ := base64.StdEncoding.DecodeString(image.DataBase64)
	image.Size = len(decoded)
	if err := validateOperatorAttachments([]OperatorAttachment{image}); err != nil {
		t.Fatal(err)
	}
	message := modelMessage{Role: "user", Content: "Inspect these", Attachments: []OperatorAttachment{testTextAttachment(), image}}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var restored modelMessage
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	payload := operatorModelPayload(restored)
	if _, ok := payload["attachments"]; ok {
		t.Fatal("internal attachment field leaked into provider protocol")
	}
	parts := payload["content"].([]any)
	if len(parts) != 4 || !strings.Contains(parts[1].(map[string]any)["text"].(string), "hello") {
		t.Fatal("text attachment lost after checkpoint")
	}
	if parts[3].(map[string]any)["image_url"].(map[string]string)["url"] != "data:image/png;base64,"+image.DataBase64 {
		t.Fatal("image did not become multimodal content")
	}
	if operatorModelPayload(modelMessage{Role: "assistant", Content: "done"})["content"] != "done" {
		t.Fatal("text-only protocol changed")
	}
}

func TestOperatorAttachmentsPersistAndStayOutOfEvents(t *testing.T) {
	s, c, _ := operatorFixture(t)
	attachment := testTextAttachment()
	_, err := s.EnqueueOperator(context.Background(), c, "attached_request", "Read this", "test", nil, attachment)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.OperatorMessages(context.Background(), c.ID)
	if err != nil || len(messages) != 1 || len(messages[0].Attachments) != 1 || messages[0].Attachments[0].DataBase64 != attachment.DataBase64 {
		t.Fatalf("attachment not restored: %v", err)
	}
	events, err := s.OperatorEvents(context.Background(), c.ID, 0)
	if err != nil || len(events) != 1 || strings.Contains(string(events[0].Data), attachment.DataBase64) {
		t.Fatal("attachment data duplicated in event log")
	}
	if err := s.cancelOperator(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueOperator(context.Background(), c, "plain_request", "No attachments", "test", nil); err != nil {
		t.Fatal(err)
	}
}
