package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

const maxDictationBytes = 8 << 20
const dictationModel = "nvidia/nemotron-3.5-transcribe"

type WrenAudioConfig struct {
	APIKey    string
	transport http.RoundTripper
}

// WrenAudioConfigFromEnv enables platform-funded transcription only when the
// operator has configured its provider credential. Audio is never auto-sent.
func WrenAudioConfigFromEnv() WrenAudioConfig {
	if strings.EqualFold(os.Getenv("CANTER_DICTATION_ENABLED"), "false") {
		return WrenAudioConfig{}
	}
	return WrenAudioConfig{APIKey: strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))}
}

// dictationWAVDuration validates the whole RIFF container, not client metadata.
// Only PCM16 mono/stereo is accepted, so duration is authoritative and bounded.
func dictationWAVDuration(b []byte) (float64, error) {
	invalid := errors.New("Dictation needs a valid PCM16 WAV recording of at most 60 seconds.")
	if len(b) < 44 || len(b) > maxDictationBytes || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return 0, invalid
	}
	var channels, align uint16
	var rate uint32
	var data uint64
	fmtSeen, dataSeen := false, false
	for pos := 12; pos < len(b); {
		if len(b)-pos < 8 {
			return 0, invalid
		}
		n := uint64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		start := pos + 8
		end := uint64(start) + n
		if end > uint64(len(b)) {
			return 0, invalid
		}
		switch string(b[pos : pos+4]) {
		case "fmt ":
			if fmtSeen || n != 16 {
				return 0, invalid
			}
			f := b[start:int(end)]
			channels = binary.LittleEndian.Uint16(f[2:4])
			rate = binary.LittleEndian.Uint32(f[4:8])
			align = binary.LittleEndian.Uint16(f[12:14])
			if binary.LittleEndian.Uint16(f[:2]) != 1 || (channels != 1 && channels != 2) || rate < 8000 || rate > 48000 || align != channels*2 || binary.LittleEndian.Uint16(f[14:16]) != 16 || binary.LittleEndian.Uint32(f[8:12]) != rate*uint32(align) {
				return 0, invalid
			}
			fmtSeen = true
		case "data":
			if dataSeen {
				return 0, invalid
			}
			dataSeen = true
			data = n
		}
		pos = int(end + n%2)
		if pos > len(b) {
			return 0, invalid
		}
	}
	if !fmtSeen || !dataSeen || data == 0 || data%uint64(align) != 0 {
		return 0, invalid
	}
	duration := float64(data) / float64(rate*uint32(align))
	if duration > 60 {
		return 0, invalid
	}
	return duration, nil
}

func (h *HTTPServer) wrenDictation(w http.ResponseWriter, r *http.Request, p Principal, c Conversation) {
	s := h.service.Store
	authorize := func() error {
		if p.Account == nil || p.Installation != nil {
			return ErrForbidden
		}
		if err := s.RequireWorkspaceWriter(r.Context(), c.WorkspaceID, p.Account.ID); err != nil {
			return err
		}
		_, err := s.Conversation(r.Context(), c.WorkspaceID, p.Account.ID, c.ID)
		return err
	}
	if err := authorize(); err != nil {
		writeStoreError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		enabled := h.config.Audio.APIKey != "" && h.WrenAIRequestAvailable(r.Context(), c.WorkspaceID, WrenAIRequestEstimate{Kind: "dictation", Model: dictationModel}) == nil
		writeJSON(w, 200, map[string]any{"enabled": enabled, "formats": []string{"audio/wav"}, "encoding": "pcm_s16le", "maxBytes": maxDictationBytes, "maxDurationSeconds": 60, "autoSend": false, "incremental": true, "maxSegmentSeconds": 6})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.config.Audio.APIKey == "" {
		writeError(w, 503, errors.New("Dictation is not configured."))
		return
	}
	segment := r.URL.Query().Get("segment") == "1"
	limits := []requestLimit{{bucket: "dictation-minute", max: 6, window: time.Minute}, {bucket: "dictation-hour", max: 120, window: time.Hour}}
	if segment {
		limits = []requestLimit{{bucket: "dictation-segment-minute", max: 60, window: time.Minute}, {bucket: "dictation-segment-hour", max: 600, window: time.Hour}}
	}
	for _, limit := range limits {
		if ok, _ := h.limits.allow(limit.bucket+"|"+p.Account.ID, limit); !ok {
			writeError(w, 429, ErrCapacity)
			return
		}
	}
	select {
	case h.audioUploads <- struct{}{}:
		defer func() { <-h.audioUploads }()
	default:
		writeError(w, 429, ErrCapacity)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (media != "audio/wav" && media != "audio/x-wav") {
		writeError(w, 415, errors.New("Dictation accepts PCM16 audio/wav recordings."))
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Second))
	r.Body = http.MaxBytesReader(w, r.Body, maxDictationBytes)
	audio, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 413, errors.New("Recording exceeds the 8 MiB limit."))
		return
	}
	defer clear(audio)
	duration, err := dictationWAVDuration(audio)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if segment && duration > 6 {
		writeError(w, 400, errors.New("A dictation segment must be at most six seconds."))
		return
	}
	if err = authorize(); err != nil {
		writeStoreError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 65*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": dictationModel, "input_audio": map[string]string{"data": base64.StdEncoding.EncodeToString(audio), "format": "wav"}})
	defer clear(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.config.Audio.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: h.config.Audio.transport, Timeout: 65 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	requestKey, keyErr := newID("dictation_request_")
	if keyErr != nil {
		writeStoreError(w, keyErr)
		return
	}
	paid, err := h.BeginWrenAIPaidRequest(ctx, WrenAIReservationInput{WorkspaceID: c.WorkspaceID, AccountID: p.Account.ID, ConversationID: c.ID, RequestKey: requestKey, Kind: "dictation", Model: dictationModel}, WrenAIRequestEstimate{Kind: "dictation", Model: dictationModel, InputBytes: len(body)})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var paidUsage json.RawMessage
	providerID := ""
	defer func() { _ = paid.Finish(ctx, paidUsage, providerID) }()
	if err = paid.MarkDispatched(ctx); err != nil {
		writeStoreError(w, err)
		return
	}
	response, err := client.Do(req)
	if err != nil {
		writeError(w, 502, errors.New("Transcription could not be completed. Your message was not sent."))
		return
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	defer clear(raw)
	var result struct {
		Text  string          `json:"text"`
		Usage json.RawMessage `json:"usage"`
	}
	if err != nil || response.StatusCode != 200 || json.Unmarshal(raw, &result) != nil || len(result.Text) > 64<<10 {
		writeError(w, 502, errors.New("The transcription provider could not transcribe this recording. Your message was not sent."))
		return
	}
	paidUsage = result.Usage
	providerID = response.Header.Get("X-Generation-Id")
	if err = authorize(); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"text": result.Text, "durationSeconds": duration, "editable": true, "sent": false})
}
