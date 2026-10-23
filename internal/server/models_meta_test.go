package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

func payloadFor(t *testing.T, cfg config.Config, model gemini.ModelInfo) map[string]any {
	t.Helper()
	s := &Server{Cfg: cfg}
	payload := s.BuildModelsPayload([]gemini.ModelInfo{model})
	data, ok := payload["data"].([]map[string]any)
	if !ok || len(data) != 1 {
		t.Fatalf("unexpected payload shape: %#v", payload)
	}
	return data[0]
}

func capsOf(t *testing.T, entry map[string]any) map[string]any {
	t.Helper()
	caps, ok := entry["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("missing capabilities: %#v", entry)
	}
	return caps
}

// The advertised default must equal what the daemon actually applies to a
// request that carries no reasoning_effort.
func TestModelsPayloadAdvertisesConfiguredDefault(t *testing.T) {
	for _, level := range []int{1, 4, 2} {
		want := ThinkingLevelName(level)
		if want == "" {
			t.Fatalf("no name for level %d", level)
		}
		cfg := config.Config{DefaultThinking: level}
		entry := payloadFor(t, cfg, gemini.ModelInfo{
			ModelName:      "gemini-argon",
			DisplayName:    "4 Argon",
			ThinkingLevels: []string{"low", "medium", "high"},
		})
		thinking := entry["thinking"].(map[string]any)
		if got := thinking["default"]; got != want {
			t.Fatalf("thinking.default = %v, want %v (level %d)", got, want, level)
		}
		if got := capsOf(t, entry)["defaultThinkingLevel"]; got != want {
			t.Fatalf("capabilities.defaultThinkingLevel = %v, want %v", got, want)
		}
	}
}

// A configured default the model does not support must not be advertised for it.
func TestModelsPayloadIgnoresUnsupportedConfiguredDefault(t *testing.T) {
	cfg := config.Config{DefaultThinking: 4} // medium
	entry := payloadFor(t, cfg, gemini.ModelInfo{
		ModelName:      "gemini-argon",
		DisplayName:    "4 Argon",
		ThinkingLevels: []string{"low", "high"},
	})
	thinking := entry["thinking"].(map[string]any)
	if got := thinking["default"]; got == "medium" {
		t.Fatal("advertised a default the model does not support")
	}
}

// Each model advertises its own registry levels, not one global list.
func TestModelsPayloadAdvertisesRegistryLevels(t *testing.T) {
	cfg := config.Config{DefaultThinking: 2}
	entry := payloadFor(t, cfg, gemini.ModelInfo{
		ModelName:      "gemini-flash-lite",
		DisplayName:    "3.5 Flash-Lite",
		ThinkingLevels: []string{"low", "high"},
	})
	caps := capsOf(t, entry)
	levels, _ := caps["thinkingLevels"].([]string)
	if len(levels) != 2 || levels[0] != "low" || levels[1] != "high" {
		t.Fatalf("thinkingLevels = %v, want the registry list", levels)
	}
	if caps["reasoning"] != true {
		t.Fatal("model with registry levels must be reasoning")
	}
}

func TestModelsPayloadUsesDocumentedLimits(t *testing.T) {
	cfg := config.Config{ContextWindow: 200000, MaxOutputTokens: 8192}
	entry := payloadFor(t, cfg, gemini.ModelInfo{ModelName: "gemini-argon", DisplayName: "4 Argon"})
	if got := entry["context_length"]; got != 1048576 {
		t.Fatalf("context_length = %v, want the documented 1048576", got)
	}
	if got := entry["max_completion_tokens"]; got != 65536 {
		t.Fatalf("max_completion_tokens = %v, want the documented 65536", got)
	}
	if got := capsOf(t, entry)["limitsSource"]; got != "documented" {
		t.Fatalf("limitsSource = %v, want documented", got)
	}
}

func TestModelsPayloadFallsBackToConfiguredLimits(t *testing.T) {
	cfg := config.Config{ContextWindow: 200000, MaxOutputTokens: 8192}
	entry := payloadFor(t, cfg, gemini.ModelInfo{ModelName: "gemini-mystery", DisplayName: "9.9 Mystery"})
	if got := entry["context_length"]; got != 200000 {
		t.Fatalf("context_length = %v, want the configured 200000", got)
	}
	if got := capsOf(t, entry)["limitsSource"]; got != "config" {
		t.Fatalf("limitsSource = %v, want config", got)
	}
}

func TestModelsPayloadAdvertisesMultimodalInputCapabilities(t *testing.T) {
	cfg := config.Config{}
	entry := payloadFor(t, cfg, gemini.ModelInfo{ModelName: "gemini-pro", DisplayName: "3.1 Pro"})
	arch, ok := entry["architecture"].(map[string]any)
	if !ok {
		t.Fatalf("missing architecture: %#v", entry)
	}
	mods, ok := arch["input_modalities"].([]string)
	if !ok || len(mods) != 5 {
		t.Fatalf("input_modalities = %#v, want [text image audio file video]", arch["input_modalities"])
	}
	caps := capsOf(t, entry)
	for _, k := range []string{"vision", "pdf", "audioInput", "videoInput"} {
		if caps[k] != true {
			t.Fatalf("capabilities.%s = %v, want true", k, caps[k])
		}
	}
}

func TestChatAndResponsesUploadPDFAndAudioAttachments(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var uploadedTypes []string
	var uploadedNames []string
	var lastOpts gemini.StreamOptions
	up := &mockUpstream{
		uploadFn: func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
			uploadedTypes = append(uploadedTypes, contentType)
			uploadedNames = append(uploadedNames, filename)
			return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
		},
		streamFn: func(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			lastOpts = opts
			_ = onFrame(&gemini.LineParseResult{
				AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{
					{RCID: "rc1", Text: "Parsed PDF and audio", Done: true, AnsweredModelID: testModel.ModelID},
				},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = up

	pdfB64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\nhello pdf\n"))
	oggB64 := base64.StdEncoding.EncodeToString([]byte("OggS\x00\x02voice\x00"))

	rec := runChat(t, srv, acct, up, map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Summarize both"},
					map[string]any{
						"type": "file",
						"file": map[string]any{
							"filename":  "paper.pdf",
							"file_data": "data:application/pdf;base64," + pdfB64,
						},
					},
					map[string]any{
						"type": "input_audio",
						"input_audio": map[string]any{
							"data":   oggB64,
							"format": "ogg",
						},
					},
				},
			},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(uploadedTypes) != 2 || uploadedTypes[0] != "application/pdf" || uploadedTypes[1] != "audio/ogg" {
		t.Fatalf("unexpected uploaded content types: %v", uploadedTypes)
	}
	if len(uploadedNames) != 2 || uploadedNames[0] != "paper.pdf" || uploadedNames[1] != "audio_2.ogg" {
		t.Fatalf("unexpected uploaded filenames: %v", uploadedNames)
	}
	if len(lastOpts.Files) != 2 || lastOpts.Files[0].Filename != "paper.pdf" || lastOpts.Files[1].Filename != "audio_2.ogg" {
		t.Fatalf("unexpected StreamOptions.Files: %+v", lastOpts.Files)
	}
}

func TestStreamingMediaInIntermediateFrameDoesNotTriggerUpstreamRevision(t *testing.T) {
	srv, acct := newTestHarness(true, false)
	up := &mockUpstream{
		streamFn: func(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			img := gemini.GeneratedImage{
				URL:   "https://lh3.googleusercontent.com/gg-dl/stream-cat=s0-d",
				Title: "Stream Cat",
			}
			// Frame 1 (done=false): emits initial text + structured GeneratedImages without markdown in Text
			if err := onFrame(&gemini.LineParseResult{
				CID:             "c_stream",
				RID:             "r_stream",
				AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{
					{
						RCID:            "rc_stream",
						Text:            "前半句 ",
						Done:            false,
						AnsweredModelID: testModel.ModelID,
						GeneratedImages: []gemini.GeneratedImage{img},
					},
				},
			}); err != nil {
				return nil, err
			}
			// Frame 2 (done=true): continues text after image frame
			if err := onFrame(&gemini.LineParseResult{
				CID:             "c_stream",
				RID:             "r_stream",
				AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{
					{
						RCID:            "rc_stream",
						Text:            "前半句 后半句",
						Done:            true,
						AnsweredModelID: testModel.ModelID,
					},
				},
			}); err != nil {
				return nil, err
			}
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	rec := runChat(t, srv, acct, up, map[string]any{
		"stream": true,
	})
	if rec.Code != 200 {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, "upstream_revision") {
		t.Fatalf("unexpected upstream_revision error in SSE stream:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "后半句") || !strings.Contains(bodyStr, "/v1/media/") {
		t.Fatalf("expected streamed body to include continuation text and proxied /v1/media/ link, got:\n%s", bodyStr)
	}
}
