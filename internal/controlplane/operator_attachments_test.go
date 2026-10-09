package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/png"
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

func TestOperatorImageDimensionLimits(t *testing.T) {
	// This is a valid 1x1 PNG. DecodeConfig reads only its header, so the
	// oversized cases exercise limits without allocating image pixels.
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aN2kAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	for name, dimensions := range map[string][2]uint32{
		"pixel budget":  {5000, 5000},
		"dimension cap": {8193, 1},
	} {
		t.Run(name, func(t *testing.T) {
			imageData := pngHeaderWithDimensions(t, data, dimensions[0], dimensions[1])
			config, err := png.DecodeConfig(bytes.NewReader(imageData))
			if err != nil || config.Width != int(dimensions[0]) || config.Height != int(dimensions[1]) {
				t.Fatalf("test image header is invalid: %#v (%v)", config, err)
			}
			attachment := OperatorAttachment{ID: "oversized", Name: "large.png", MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(imageData), Size: len(imageData)}
			if validateOperatorAttachments([]OperatorAttachment{attachment}) == nil {
				t.Fatal("oversized image dimensions accepted")
			}
		})
	}
	boundary := image.NewGray(image.Rect(0, 0, 5000, 4000))
	var boundaryBuffer bytes.Buffer
	if err := png.Encode(&boundaryBuffer, boundary); err != nil {
		t.Fatal(err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(boundaryBuffer.Bytes()))
	if err != nil || config.Width != 5000 || config.Height != 4000 {
		t.Fatalf("boundary PNG is invalid: %#v (%v)", config, err)
	}
	attachment := OperatorAttachment{ID: "boundary", Name: "boundary.png", MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(boundaryBuffer.Bytes()), Size: boundaryBuffer.Len()}
	if attachment.Size > 2<<20 || validateOperatorAttachments([]OperatorAttachment{attachment}) != nil {
		t.Fatal("valid image at the 20 megapixel boundary was rejected")
	}

	webp := testWebP("VP8X", webPExtendedHeader(100, 200, false), "VP8 ", testWebPLossyFrame(100, 200))
	width, height, format, err := operatorImageDimensions(webp)
	if err != nil || width != 100 || height != 200 || format != "image/webp" {
		t.Fatalf("WebP dimensions were not read correctly: %dx%d %s (%v)", width, height, format, err)
	}
	attachment = OperatorAttachment{ID: "webp", Name: "frame.webp", MediaType: "image/webp", DataBase64: base64.StdEncoding.EncodeToString(webp), Size: len(webp)}
	if err := validateOperatorAttachments([]OperatorAttachment{attachment}); err != nil {
		t.Fatalf("valid bounded WebP headers were rejected: %v", err)
	}
	simpleWebP := testWebP("VP8 ", testWebPLossyFrame(100, 200))
	if width, height, format, err = operatorImageDimensions(simpleWebP); err != nil || width != 100 || height != 200 || format != "image/webp" {
		t.Fatalf("simple WebP frame was rejected: %dx%d %s (%v)", width, height, format, err)
	}
	webpOnlyHeader := testWebP("VP8X", webPExtendedHeader(100, 200, false))
	if _, _, _, err := operatorImageDimensions(webpOnlyHeader); err == nil {
		t.Fatal("WebP extended header without image data was accepted")
	}
	badFrame := append([]byte(nil), webp...)
	binary.LittleEndian.PutUint16(badFrame[44:46], 5000)
	if _, _, _, err := operatorImageDimensions(badFrame); err == nil {
		t.Fatal("WebP frame larger than its canvas was accepted")
	}
	badRIFFLength := append([]byte(nil), webp...)
	binary.LittleEndian.PutUint32(badRIFFLength[4:8], 39)
	if _, _, _, err := operatorImageDimensions(badRIFFLength); err == nil {
		t.Fatal("mismatched WebP RIFF length was accepted")
	}

	animation := testWebP(
		"VP8X", webPExtendedHeader(5000, 2000, true),
		"ANIM", make([]byte, 6),
		"ANMF", testWebPAnimationFrame(5000, 2000),
		"ANMF", testWebPAnimationFrame(5000, 2000),
	)
	width, height, format, err = operatorImageDimensions(animation)
	if err != nil || width != 5000 || height != 2000 || format != "image/webp" {
		t.Fatalf("valid WebP animation was rejected: %dx%d %s (%v)", width, height, format, err)
	}
	tooManyFrames := testWebP(
		"VP8X", webPExtendedHeader(5000, 2000, true),
		"ANIM", make([]byte, 6),
		"ANMF", testWebPAnimationFrame(5000, 2000),
		"ANMF", testWebPAnimationFrame(5000, 2000),
		"ANMF", testWebPAnimationFrame(5000, 2000),
	)
	if _, _, _, err := operatorImageDimensions(tooManyFrames); err == nil {
		t.Fatal("WebP animation exceeding the total pixel budget was accepted")
	}
}

func pngHeaderWithDimensions(t *testing.T, source []byte, width, height uint32) []byte {
	t.Helper()
	data := append([]byte(nil), source...)
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func webPExtendedHeader(width, height int, animated bool) []byte {
	payload := make([]byte, 10)
	if animated {
		payload[0] = 0x02
	}
	writeWebPUint24(payload[4:7], width-1)
	writeWebPUint24(payload[7:10], height-1)
	return payload
}

func testWebPLossyFrame(width, height int) []byte {
	payload := make([]byte, 10)
	payload[3], payload[4], payload[5] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(payload[6:8], uint16(width))
	binary.LittleEndian.PutUint16(payload[8:10], uint16(height))
	return payload
}

func testWebPAnimationFrame(frameWidth, frameHeight int) []byte {
	payload := make([]byte, 16)
	writeWebPUint24(payload[6:9], frameWidth-1)
	writeWebPUint24(payload[9:12], frameHeight-1)
	return append(payload, testWebPChunk("VP8 ", testWebPLossyFrame(frameWidth, frameHeight))...)
}

func testWebP(chunks ...any) []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	for index := 0; index < len(chunks); index += 2 {
		body.Write(testWebPChunk(chunks[index].(string), chunks[index+1].([]byte)))
	}
	data := append([]byte("RIFF"), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(data[4:8], uint32(body.Len()))
	return append(data, body.Bytes()...)
}

func testWebPChunk(name string, payload []byte) []byte {
	chunk := append([]byte(name), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(payload)))
	chunk = append(chunk, payload...)
	if len(payload)&1 != 0 {
		chunk = append(chunk, 0)
	}
	return chunk
}

func writeWebPUint24(data []byte, value int) {
	data[0] = byte(value)
	data[1] = byte(value >> 8)
	data[2] = byte(value >> 16)
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
	call := modelToolCall{ID: "call-one", Type: "function"}
	call.Function.Name = "inspect"
	call.Function.Arguments = `{"path":"README.md"}`
	withMetadata := operatorModelPayload(modelMessage{
		Role: "assistant", Content: "working", ToolCalls: []modelToolCall{call}, ToolCallID: "call-parent",
		ReasoningDetails: json.RawMessage(`[ {"type":"reasoning.encrypted","data":"signature"} ]`),
		Usage:            &operatorModelUsage{PromptTokens: 123}, Attachments: []OperatorAttachment{testTextAttachment()},
	})
	if _, ok := withMetadata["usage"]; ok {
		t.Fatal("usage accounting leaked into provider input")
	}
	if withMetadata["tool_call_id"] != "call-parent" || len(withMetadata["tool_calls"].([]modelToolCall)) != 1 {
		t.Fatal("tool call fields were not preserved")
	}
	if !json.Valid(withMetadata["reasoning_details"].(json.RawMessage)) {
		t.Fatal("reasoning details were not preserved")
	}
}

func TestOperatorAttachmentsPersistAndStayOutOfEvents(t *testing.T) {
	s, c, _ := operatorFixture(t)
	attachment := testTextAttachment()
	_, err := s.EnqueueOperator(context.Background(), c, "attached_request", "Read this", "test", nil, attachment)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.OperatorMessages(context.Background(), c.ID, "")
	if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Attachments) != 1 || page.Messages[0].Attachments[0].DataBase64 != attachment.DataBase64 {
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
