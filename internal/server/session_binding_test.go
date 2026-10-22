package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// TestExplicitSessionBindingReusesContinuation proves that a stable
// X-GSB-Session id binds turn 2 to the account/cid saved by turn 1 and sends
// only the delta prompt, without relying on the content-derived cache key.
func TestExplicitSessionBindingReusesContinuation(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var mu sync.Mutex
	var prompts []string
	var metas [][]any
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		metas = append(metas, append([]any(nil), opts.Metadata...))
		turn := len(prompts)
		mu.Unlock()
		if err := onFrame(&gemini.LineParseResult{
			CID: "cid-1", RID: "rid-1", ContextToken: "ctx-1",
			AnsweredModelID: testModel.ModelID,
			Frames:          []gemini.Frame{makeFrame(fmt.Sprintf("answer %d", turn), "", true, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		return &gemini.StreamResponse{Status: 200, CID: "cid-1", RID: "rid-1"}, nil
	}}
	acct.Client = client
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "sess-1"}

	first := map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
		},
	}
	rec1 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec1, first, scope)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}
	if len(metas[0]) != 0 {
		t.Fatalf("first turn must not reuse a continuation, got %#v", metas[0])
	}
	bindingKey := sessionBindingKey(scope, scope.SessionID)
	if b := srv.getSessionBinding(bindingKey); b == nil || b.NextLen != 2 {
		t.Fatalf("expected binding with NextLen=2, got %+v", b)
	}

	second := map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "assistant", "content": "answer 1"},
			map[string]any{"role": "user", "content": "two"},
		},
	}
	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, second, scope)
	if rec2.Code != 200 {
		t.Fatalf("turn 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if len(metas[1]) == 0 {
		t.Fatalf("expected turn 2 to reuse the bound metadata, got none")
	}
	if !strings.Contains(prompts[1], "two") || strings.Contains(prompts[1], "one") {
		t.Fatalf("expected turn 2 delta prompt to contain only the new message, got %q", prompts[1])
	}
}

// TestExplicitSessionBindingSurvivesModelChange preserves the same upstream
// conversation when the caller switches models; an upstream rejection still
// falls back to the full prompt through the normal retry path.
func TestExplicitSessionBindingSurvivesModelChange(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	alt := testModel
	alt.ModelName = "gemini-flash-alt"
	alt.ModelID = "cccccccccccccccc"
	alt.DisplayName = "Alt"
	alt.ModelNumber = 2
	acct.Models = []gemini.ModelInfo{testModel, alt}

	var mu sync.Mutex
	var metas [][]any
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		mu.Lock()
		metas = append(metas, append([]any(nil), opts.Metadata...))
		mu.Unlock()
		_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", ContextToken: "ctx", AnsweredModelID: opts.Model.ModelID, Frames: []gemini.Frame{makeFrame("answer", "", true, opts.Model.ModelID)}})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	acct.Client = client
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "sess-model"}

	first := map[string]any{"model": testModel.ModelName, "messages": []any{map[string]any{"role": "user", "content": "one"}}}
	rec1 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec1, first, scope)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d", rec1.Code)
	}
	second := map[string]any{
		"model": alt.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "assistant", "content": "answer"},
			map[string]any{"role": "user", "content": "two"},
		},
	}
	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, second, scope)
	if rec2.Code != 200 {
		t.Fatalf("turn 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if len(metas[1]) == 0 {
		t.Fatal("model change must reuse the old continuation instead of starting a new session")
	}
}

func TestTemporaryRequestDoesNotReuseSessionBinding(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var mu sync.Mutex
	var metas [][]any
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		mu.Lock()
		metas = append(metas, append([]any(nil), opts.Metadata...))
		mu.Unlock()
		_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", ContextToken: "ctx", AnsweredModelID: testModel.ModelID, Frames: []gemini.Frame{makeFrame("answer", "", true, testModel.ModelID)}})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	acct.Client = client
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "sess-temp"}
	first := map[string]any{"model": testModel.ModelName, "messages": []any{map[string]any{"role": "user", "content": "one"}}}
	rec1 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec1, first, scope)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d", rec1.Code)
	}
	second := map[string]any{
		"model":     testModel.ModelName,
		"temporary": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "assistant", "content": "answer"},
			map[string]any{"role": "user", "content": "two"},
		},
	}
	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, second, scope)
	if rec2.Code != 200 {
		t.Fatalf("turn 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if len(metas[1]) != 0 {
		t.Fatalf("temporary request must not reuse the session binding, got %#v", metas[1])
	}
}

func TestSessionHeaderValidation(t *testing.T) {
	srv, _ := newTestHarness(false, false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("X-GSB-Session", "bad id!")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid X-GSB-Session") {
		t.Fatalf("expected 400 invalid X-GSB-Session, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSessionBindingPersistsAcrossRestart(t *testing.T) {
	srv, _ := newTestHarness(false, true)
	srv.Cfg.HomeDir = t.TempDir()
	key := "sid:test-restart"
	srv.putSessionBinding(sessionBinding{
		Key: key, AccountID: "acct", Model: testModel.ModelName,
		Meta:    []any{"cid", "rid", "rcid", nil, nil, nil, nil, nil, nil, "ctx"},
		NextLen: 2, HistoryHash: "hash",
	})
	restarted := New(srv.Cfg)
	got := restarted.getSessionBinding(key)
	if got == nil || got.AccountID != "acct" || got.NextLen != 2 || got.HistoryHash != "hash" {
		t.Fatalf("binding did not survive restart: %+v", got)
	}
}

func TestDefaultModelFallsBackWhenUnavailable(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.DefaultModel = "missing-model"
	var got string
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		got = opts.Model.ModelName
		_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", AnsweredModelID: opts.Model.ModelID, Frames: []gemini.Frame{makeFrame("ok", "", true, opts.Model.ModelID)}})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	acct.Client = client
	rec := runChat(t, srv, acct, client, map[string]any{"model": ""})
	if rec.Code != 200 {
		t.Fatalf("default-model fallback failed: %d %s", rec.Code, rec.Body.String())
	}
	if got != testModel.ModelName {
		t.Fatalf("expected fallback to %q, got %q", testModel.ModelName, got)
	}
}

func TestDefaultModelUsedWhenRequestOmitsModel(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	alt := testModel
	alt.ModelName = "gemini-flash-alt"
	alt.ModelID = "cccccccccccccccc"
	alt.DisplayName = "Alt"
	alt.ModelNumber = 2
	acct.Models = []gemini.ModelInfo{testModel, alt}
	srv.Cfg.DefaultModel = alt.ModelName

	var got string
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		got = opts.Model.ModelName
		_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", AnsweredModelID: opts.Model.ModelID, Frames: []gemini.Frame{makeFrame("ok", "", true, opts.Model.ModelID)}})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	acct.Client = client
	rec := runChat(t, srv, acct, client, map[string]any{"model": ""})
	if rec.Code != 200 {
		t.Fatalf("default-model request failed: %d %s", rec.Code, rec.Body.String())
	}
	if got != alt.ModelName {
		t.Fatalf("expected default model %q, got %q", alt.ModelName, got)
	}
}
