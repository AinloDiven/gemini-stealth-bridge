package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestChatAndResponsesUploadVideoAttachments(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var uploadedTypes []string
	var uploadedNames []string
	up := &mockUpstream{
		uploadFn: func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
			uploadedTypes = append(uploadedTypes, contentType)
			uploadedNames = append(uploadedNames, filename)
			return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
		},
		streamFn: func(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{
					{RCID: "rc1", Text: "Parsed video", Done: true, AnsweredModelID: testModel.ModelID},
				},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	acct.Client = up

	mp4B64 := base64.StdEncoding.EncodeToString([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 1})
	mp4B64Resp := base64.StdEncoding.EncodeToString([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 2})
	chatRec := runChat(t, srv, acct, up, map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Describe clip"},
					map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:video/mp4;base64," + mp4B64}},
				},
			},
		},
	})
	if chatRec.Code != 200 {
		t.Fatalf("chat video upload failed: %d %s", chatRec.Code, chatRec.Body.String())
	}

	respRec := httptest.NewRecorder()
	srv.HandleResponses(context.Background(), respRec, map[string]any{
		"model": testModel.ModelName,
		"input": []any{
			map[string]any{"type": "input_text", "text": "Describe clip"},
			map[string]any{"type": "input_video", "filename": "demo.mp4", "video_data": mp4B64Resp},
		},
	}, AuthScope{OK: true, AccountID: "acct"})
	if respRec.Code != 200 {
		t.Fatalf("responses video upload failed: %d %s", respRec.Code, respRec.Body.String())
	}
	if len(uploadedTypes) != 2 || uploadedTypes[0] != "video/mp4" || uploadedTypes[1] != "video/mp4" {
		t.Fatalf("unexpected uploaded video MIME types: %v", uploadedTypes)
	}
	if len(uploadedNames) != 2 || uploadedNames[0] != "video_1.mp4" || uploadedNames[1] != "demo.mp4" {
		t.Fatalf("unexpected uploaded video filenames: %v", uploadedNames)
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
						Text:            "First half ",
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
						Text:            "First half second half",
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
	if !strings.Contains(bodyStr, "second half") || !strings.Contains(bodyStr, "/v1/media/") {
		t.Fatalf("expected streamed body to include continuation text and proxied /v1/media/ link, got:\n%s", bodyStr)
	}
}

func TestStreamingImageGenerationHoldsIntermediateProseAndStripsUpstreamPlaceholder(t *testing.T) {
	longProse := strings.Repeat("Intermediate image generation prose held back while tool is active. ", 4)
	srv, acct := newTestHarness(false, true)
	up := &mockUpstream{
		streamFn: func(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			if err := onFrame(&gemini.LineParseResult{
				CID:             "c_img2",
				RID:             "r_img2",
				MediaToolActive: true,
				Action:          &gemini.ActionStatus{ToolID: "image_generation", BranchTitle: "Creating image"},
			}); err != nil {
				return nil, err
			}
			candStreaming := []any{"rc_img2", []any{longProse}, nil, nil, nil, nil, nil, nil, []any{float64(1)}}
			streamParsed, err := gemini.ParseFrameLine(mustMarshalWrbFrame(t, "c_img2", "r_img2", candStreaming))
			if err != nil {
				return nil, err
			}
			if err := onFrame(streamParsed); err != nil {
				return nil, err
			}
			candDone := []any{
				"rc_img2",
				[]any{longProse + "Ready.\nhttps://googleusercontent.com/image_generation_content/0"},
				nil, nil, nil, nil, nil, nil,
				[]any{float64(2)},
				nil, nil, nil,
				[]any{
					nil, nil, nil, nil, nil, nil, nil,
					[]any{[]any{[]any{
						[]any{nil, nil, nil, []any{nil, float64(1024), float64(1024), "https://lh3.googleusercontent.com/gg-dl/poster=s512"}},
						nil, nil,
						[]any{nil, nil, nil, nil, nil, []any{"Poster"}, "Artwork"},
					}}},
				},
			}
			doneParsed, err := gemini.ParseFrameLine(mustMarshalWrbFrame(t, "c_img2", "r_img2", candDone))
			if err != nil {
				return nil, err
			}
			if err := onFrame(doneParsed); err != nil {
				return nil, err
			}
			return &gemini.StreamResponse{Status: 200, CID: "c_img2", RID: "r_img2"}, nil
		},
	}

	rec := runChat(t, srv, acct, up, map[string]any{"stream": true})
	if rec.Code != 200 {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, "image_generation_content/") || strings.Contains(bodyStr, "upstream_revision") {
		t.Fatalf("unexpected placeholder or revision error in SSE stream:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "Ready.") || !strings.Contains(bodyStr, "/v1/media/") {
		t.Fatalf("expected final prose and proxied /v1/media/ link, got:\n%s", bodyStr)
	}
	if strings.Count(bodyStr, `"content":"Intermediate image generation prose`) != 1 {
		t.Fatalf("expected held intermediate prose to emit in single finalized delta:\n%s", bodyStr)
	}
}

func mustMarshalWrbFrame(t *testing.T, cid, rid string, cand []any) string {
	t.Helper()
	pj := []any{nil, []any{cid, rid}, nil, nil, []any{cand}}
	inner, err := json.Marshal(pj)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(inner)}})
	if err != nil {
		t.Fatal(err)
	}
	return string(outer)
}

func TestStructuredMediaExtensionAndHeaderMode(t *testing.T) {
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			frame := gemini.Frame{
				RCID: "rc_media_1",
				Text: "",
				Done: true,
				GeneratedImages: []gemini.GeneratedImage{
					{URL: "https://example.com/gen.png", Title: "Poster", Alt: "Sample poster", Width: 512, Height: 512},
				},
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c_media",
				RID:             "r_media",
				MediaToolActive: true,
				Frames:          []gemini.Frame{frame},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	srv := mcpTestServer(t, client)

	doChat := func(mediaMode string, stream bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"model":    testModel.ModelName,
			"stream":   stream,
			"messages": []any{map[string]any{"role": "user", "content": "draw poster"}},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer audit")
		if mediaMode != "" {
			req.Header.Set("X-GSB-Media-Mode", mediaMode)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Default markdown mode emits both x_gemini_media and fallback Markdown links.
	recDefault := doChat("", true)
	if recDefault.Code != 200 {
		t.Fatalf("expected 200 default stream, got %d: %s", recDefault.Code, recDefault.Body.String())
	}
	defaultBody := recDefault.Body.String()
	if !strings.Contains(defaultBody, `"x_gemini_media":[{"alt":"Sample poster"`) || !strings.Contains(defaultBody, "![Poster](http://example.com/v1/media/") {
		t.Fatalf("expected both x_gemini_media and markdown in default mode, got:\n%s", defaultBody)
	}

	// Structured mode omits Markdown injection from content even when prose is empty, delivering media via x_gemini_media.
	recStructured := doChat("structured", true)
	if recStructured.Code != 200 {
		t.Fatalf("expected 200 structured stream, got %d: %s", recStructured.Code, recStructured.Body.String())
	}
	structBody := recStructured.Body.String()
	if !strings.Contains(structBody, `"x_gemini_media":[{"alt":"Sample poster"`) || !strings.Contains(structBody, `"url":"http://example.com/v1/media/`) {
		t.Fatalf("expected x_gemini_media in structured mode, got:\n%s", structBody)
	}
	if strings.Contains(structBody, "![Poster]") || strings.Contains(structBody, "[Download Image") {
		t.Fatalf("did not expect Markdown media links in structured mode, got:\n%s", structBody)
	}

	// Non-streaming JSON in structured mode includes x_gemini_media and keeps message.content clean.
	recJSON := doChat("structured", false)
	if recJSON.Code != 200 {
		t.Fatalf("expected 200 structured JSON, got %d: %s", recJSON.Code, recJSON.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recJSON.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if media, ok := payload["x_gemini_media"].([]any); !ok || len(media) != 1 {
		t.Fatalf("expected 1 x_gemini_media item in JSON payload, got %#v", payload["x_gemini_media"])
	}
	choices := payload["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "" {
		t.Fatalf("expected empty content without injected markdown, got %#v", msg["content"])
	}

	// Invalid X-GSB-Media-Mode fails fast with 400.
	recBad := doChat("invalid_mode", true)
	if recBad.Code != 400 {
		t.Fatalf("expected 400 for invalid X-GSB-Media-Mode, got %d: %s", recBad.Code, recBad.Body.String())
	}
}

func TestCitationsExtensionAndSourcesFiltering(t *testing.T) {
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			sources := []gemini.GroundingSource{
				{ID: 9, Title: "attachment.mp4", MimeType: "video/mp4"},
				{URL: "https://example.com/a", Title: "Web A"},
			}
			frame := gemini.Frame{
				RCID:               "rc_cite_1",
				Text:               "Check [cite: 9] and [cite: 1].",
				Done:               true,
				AnsweredModelID:    testModel.ModelID,
				AnsweredModelLabel: testModel.ModelName,
				GroundingSources:   sources,
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:              "c_cite",
				RID:              "r_cite",
				GroundingSources: sources,
				Frames:           []gemini.Frame{frame},
			})
			return &gemini.StreamResponse{Status: 200, GroundingSources: sources}, nil
		},
	}
	srv := mcpTestServer(t, client)
	srv.Cfg.IncludeGrounding = true

	doReq := func(stream bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"model":    testModel.ModelName,
			"stream":   stream,
			"messages": []any{map[string]any{"role": "user", "content": "describe media"}},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer audit")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	recStream := doReq(true)
	if recStream.Code != 200 {
		t.Fatalf("expected 200 stream, got %d: %s", recStream.Code, recStream.Body.String())
	}
	streamBody := recStream.Body.String()
	citeIdx := strings.Index(streamBody, `"x_gemini_citations"`)
	deltaIdx := strings.Index(streamBody, "Check [cite: 9] and [cite: 1].")
	if citeIdx < 0 || deltaIdx < 0 || citeIdx >= deltaIdx {
		t.Fatalf("expected x_gemini_citations before content delta, got:\n%s", streamBody)
	}
	if !strings.Contains(streamBody, `"title":"attachment.mp4"`) || !strings.Contains(streamBody, `"mime_type":"video/mp4"`) {
		t.Fatalf("expected file citation metadata in SSE, got:\n%s", streamBody)
	}
	if strings.Contains(streamBody, "2. attachment.mp4") || !strings.Contains(streamBody, "1. Web A — https://example.com/a") {
		t.Fatalf("expected Sources footer only for web URL, got:\n%s", streamBody)
	}

	recJSON := doReq(false)
	if recJSON.Code != 200 {
		t.Fatalf("expected 200 JSON, got %d: %s", recJSON.Code, recJSON.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recJSON.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	cits, ok := payload["x_gemini_citations"].([]any)
	if !ok || len(cits) != 2 {
		t.Fatalf("expected 2 x_gemini_citations entries in JSON, got %#v", payload["x_gemini_citations"])
	}
	if fileOnly := appendGroundingCitations("Plain [cite: 9]", []gemini.GroundingSource{{ID: 9, Title: "attachment.mp4"}}); fileOnly != "Plain [cite: 9]" {
		t.Fatalf("expected file-only grounding not to append Sources section, got %q", fileOnly)
	}
}

func TestResponsesEmitsActionStatusAndCitations(t *testing.T) {
	sources := []gemini.GroundingSource{{URL: "https://example.com/ref", Title: "Ref Doc"}}
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1",
				Action: &gemini.ActionStatus{
					ToolID:         "skills",
					ToolTitle:      "Skill",
					StatusTemplate: "{tool_logo_placeholder} Skills: Research",
				},
			})
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Action:           &gemini.ActionStatus{ThinkingTitle: "Checking sources"},
				GroundingSources: sources,
				Frames: []gemini.Frame{{
					RCID: "rc1", Text: "Answer [cite: 1]", Done: true,
					AnsweredModelID: testModel.ModelID, GroundingSources: sources,
				}},
			})
			return &gemini.StreamResponse{Status: 200, GroundingSources: sources}, nil
		},
	}
	srv := mcpTestServer(t, client)
	srv.Cfg.IncludeGrounding = true

	doResp := func(stream bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"model":  testModel.ModelName,
			"stream": stream,
			"input":  "search ref",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer audit")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	recStream := doResp(true)
	if recStream.Code != 200 {
		t.Fatalf("expected 200 responses stream, got %d: %s", recStream.Code, recStream.Body.String())
	}
	sb := recStream.Body.String()
	for _, want := range []string{
		"event: response.x_gemini_action_status",
		`"provider":"skills"`,
		`"progress_detail":"Checking sources"`,
		"event: response.x_gemini_citations",
		`"title":"Ref Doc"`,
	} {
		if !strings.Contains(sb, want) {
			t.Fatalf("expected %s in /v1/responses SSE, got:\n%s", want, sb)
		}
	}
	if strings.Index(sb, `"provider":"skills"`) >= strings.Index(sb, `"progress_detail":"Checking sources"`) {
		t.Fatalf("expected queued skills status before checking sources in /v1/responses SSE:\n%s", sb)
	}

	recJSON := doResp(false)
	if recJSON.Code != 200 {
		t.Fatalf("expected 200 responses JSON, got %d: %s", recJSON.Code, recJSON.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recJSON.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	cits, ok := payload["x_gemini_citations"].([]any)
	if !ok || len(cits) != 1 {
		t.Fatalf("expected 1 x_gemini_citations entry in /v1/responses JSON, got %#v", payload["x_gemini_citations"])
	}
}
