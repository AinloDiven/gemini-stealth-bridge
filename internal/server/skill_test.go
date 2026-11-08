package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

type skillUpstream struct {
	*mockUpstream
	handle  string
	handles []string
	err     error
	name    string
	calls   int
}

func (m *skillUpstream) BindSkillRuntime(ctx context.Context, name string) (string, error) {
	m.name = name
	idx := m.calls
	m.calls++
	if idx < len(m.handles) {
		return m.handles[idx], m.err
	}
	return m.handle, m.err
}

func TestFormatSkillPromptIsIdempotentAndKeepsByteZero(t *testing.T) {
	cases := []struct{ text, want string }{
		{"hi", "@research hi"},
		{"@research hi", "@research hi"},
		{"@research\nhi", "@research\nhi"},
		{"@researchhi", "@research hi"},
		{"@research", "@research"},
		{"", "@research"},
		{"【用户消息】\nhi", "@research 【用户消息】\nhi"},
		{"<|im_start|>user\nhi", "@research <|im_start|>user\nhi"},
	}
	for _, tc := range cases {
		if got := formatSkillPrompt(tc.text, "research"); got != tc.want {
			t.Fatalf("formatSkillPrompt(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	if got := formatSkillPrompt("hi", ""); got != "hi" {
		t.Fatalf("empty skill must not change text, got %q", got)
	}
}

func TestSkillTargetForPrecedence(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "global-skill"
	srv.Cfg.SkillPerKey = map[string]string{"audit": "key-skill"}
	acct.Skill = "account-skill"

	if got := srv.skillTargetFor(AuthScope{Token: "audit"}, acct); got != "key-skill" {
		t.Fatalf("expected per-key override, got %q", got)
	}
	if got := srv.skillTargetFor(AuthScope{Token: "other"}, acct); got != "account-skill" {
		t.Fatalf("expected account override, got %q", got)
	}
	acct.Skill = ""
	if got := srv.skillTargetFor(AuthScope{Token: "other"}, acct); got != "global-skill" {
		t.Fatalf("expected global fallback, got %q", got)
	}
	srv.Cfg.APIKey = "public"
	srv.Cfg.SkillPerKey["public"] = "pool-skill"
	if got := srv.skillTargetFor(AuthScope{Pool: true, Token: ""}, acct); got != "pool-skill" {
		t.Fatalf("expected pool-key override for unauthenticated public request, got %q", got)
	}
}

func TestSkillBindingArmsSlotAndPrefixesPrompt(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "@research"
	var prompt, handle string
	base := &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		prompt, handle = p, opts.SkillRuntime
		_ = onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
		})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	sm := &skillUpstream{mockUpstream: base, handle: "0123456789abcdef0123456789abcdef"}
	acct.Client = sm
	if rec := runChat(t, srv, acct, sm, nil); rec.Code != 200 {
		t.Fatalf("skill turn failed: %d %s", rec.Code, rec.Body.String())
	}
	if sm.name != "@research" || handle != sm.handle {
		t.Fatalf("expected skill %q handle %q, got name=%q handle=%q", "@research", sm.handle, sm.name, handle)
	}
	if !strings.HasPrefix(prompt, "@research synthetic") {
		t.Fatalf("expected skill prefix at byte 0, got %q", prompt)
	}
}

func TestSkillBindingFailureDoesNotFailTurn(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "@research"
	var prompt, handle string
	base := &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		prompt, handle = p, opts.SkillRuntime
		_ = onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
		})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	sm := &skillUpstream{mockUpstream: base, err: context.DeadlineExceeded}
	acct.Client = sm
	if rec := runChat(t, srv, acct, sm, nil); rec.Code != 200 {
		t.Fatalf("skill failure must not fail the turn: %d %s", rec.Code, rec.Body.String())
	}
	if handle != "" {
		t.Fatalf("failed skill binding must leave slot 81 empty, got %q", handle)
	}
	if prompt != "@research synthetic" {
		t.Fatalf("failed slot must keep the configured text projection, got %q", prompt)
	}
}

func TestSkillFailoverSyncsHandleAndPromptPerAccount(t *testing.T) {
	srv, acct1 := newTestHarness(false, false)
	srv.Cfg.AccountTokens = nil
	srv.Cfg.MaxAttempts = 3
	acct1.Skill = "@SkillA"

	var gotAcct2Prompt, gotAcct2Handle string
	sm1 := &skillUpstream{
		handle: "handle-a",
		mockUpstream: &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			return nil, transport.NewUpstreamError("rate limited", 429, "rate_limit_error", nil, nil)
		}},
	}
	sm2 := &skillUpstream{
		handle: "handle-b",
		mockUpstream: &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			gotAcct2Prompt, gotAcct2Handle = p, opts.SkillRuntime
			_ = onFrame(&gemini.LineParseResult{
				CID: "c2", RID: "r2", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
		}},
	}
	acct1.Client = sm1
	acct2 := &AccountState{
		ID:       "acct2",
		Email:    "acct2@example.com",
		Client:   sm2,
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct1, acct2})

	// Subcase 1: fallback account has no skill — must clear SkillRuntime and strip @SkillA.
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "synthetic"}},
	}, AuthScope{OK: true})
	if rec.Code != 200 {
		t.Fatalf("failover chat failed: %d %s", rec.Code, rec.Body.String())
	}
	if gotAcct2Handle != "" || sm2.calls != 0 {
		t.Fatalf("expected unconfigured fallback account to clear SkillRuntime without RPC, got handle=%q calls=%d", gotAcct2Handle, sm2.calls)
	}
	if gotAcct2Prompt != "synthetic" {
		t.Fatalf("expected clean prompt on fallback account, got %q", gotAcct2Prompt)
	}

	// Subcase 2: fallback account has its own skill — must bind @SkillB and prefix @SkillB.
	acct1.QuotaNextAllowAt = time.Time{}
	acct1.QuotaBackoffStep = 0
	acct1.Fails = 0
	acct1.LastFail = time.Time{}
	acct2.LastUsed = time.Time{}
	acct2.Skill = "@SkillB"
	rec2 := httptest.NewRecorder()
	srv.HandleResponses(context.Background(), rec2, map[string]any{
		"model": testModel.ModelName,
		"input": "synthetic",
	}, AuthScope{OK: true})
	if rec2.Code != 200 {
		t.Fatalf("failover responses failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if gotAcct2Handle != "handle-b" || sm2.name != "@SkillB" {
		t.Fatalf("expected fallback account to bind @SkillB handle-b, got name=%q handle=%q", sm2.name, gotAcct2Handle)
	}
	if gotAcct2Prompt != "@SkillB synthetic" {
		t.Fatalf("expected @SkillB prefix on fallback account, got %q", gotAcct2Prompt)
	}
}

func TestSkillRebootstrapAndCompactionRetryPreservesSkill(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "@research"
	srv.Cfg.MaxAttempts = 2

	var events []string
	var retryPrompt, retryHandle string
	attempt := 0
	base := &mockUpstream{
		bootFn: func(ctx context.Context) error {
			events = append(events, "bootstrap")
			return nil
		},
		streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			attempt++
			if attempt == 1 {
				return nil, transport.NewUpstreamError("soft error reply", 502, "upstream_error_reply", nil, nil)
			}
			retryPrompt, retryHandle = p, opts.SkillRuntime
			_ = onFrame(&gemini.LineParseResult{
				CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("recovered", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
		},
	}
	sm := &skillUpstream{mockUpstream: base, handles: []string{"handle-1", "handle-2"}}
	acct.Client = sm

	longMsg := strings.Repeat("A", 130000)
	rec := runChat(t, srv, acct, sm, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longMsg}},
	})
	if rec.Code != 200 {
		t.Fatalf("compaction retry failed: %d %s", rec.Code, rec.Body.String())
	}
	if sm.calls != 2 || len(events) != 1 || retryHandle != "handle-2" {
		t.Fatalf("expected re-bootstrap + re-bind to handle-2, got calls=%d events=%v handle=%q", sm.calls, events, retryHandle)
	}
	if !strings.HasPrefix(retryPrompt, "@research ") {
		t.Fatalf("expected compacted retry prompt to retain @research at byte 0, got prefix %q", retryPrompt[:min(32, len(retryPrompt))])
	}
}

func TestMCPWebSearchSkipsSkillBinding(t *testing.T) {
	var captured gemini.StreamOptions
	var capturedQuery string
	base := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			captured = opts
			capturedQuery = prompt
			_ = onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1", AnsweredModelID: testModel.ModelID,
				Frames: []gemini.Frame{makeFrame("search result", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	sm := &skillUpstream{mockUpstream: base, handle: "should-not-be-used"}
	srv := mcpTestServer(t, base)
	srv.Cfg.Skill = "@research"
	srv.Accounts()[0].Client = sm

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]any{"query": "golang 1.24 release"},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("mcp web_search failed: %d %s", rec.Code, rec.Body.String())
	}
	if sm.calls != 0 || captured.SkillRuntime != "" {
		t.Fatalf("expected MCP web_search to skip skill binding, got calls=%d SkillRuntime=%q", sm.calls, captured.SkillRuntime)
	}
	if capturedQuery != "golang 1.24 release" {
		t.Fatalf("expected raw search query without @skill prefix, got %q", capturedQuery)
	}
}

func TestTemporaryChatSkipsSkillBinding(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "@research"
	acct.Skill = "@account-skill"

	var gotPrompt, gotHandle string
	var gotTemp bool
	base := &mockUpstream{streamFn: func(ctx context.Context, p string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		gotPrompt, gotHandle, gotTemp = p, opts.SkillRuntime, opts.Temporary
		_ = onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
		})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	sm := &skillUpstream{mockUpstream: base, handle: "0123456789abcdef0123456789abcdef"}
	acct.Client = sm

	rec := runChat(t, srv, acct, sm, map[string]any{
		"temporary": true,
		"messages":  []any{map[string]any{"role": "user", "content": "1+1"}},
	})
	if rec.Code != 200 {
		t.Fatalf("temporary chat failed: %d %s", rec.Code, rec.Body.String())
	}
	if !gotTemp {
		t.Fatalf("expected opts.Temporary = true")
	}
	if sm.calls != 0 || gotHandle != "" {
		t.Fatalf("expected temporary chat to skip skill binding, got calls=%d handle=%q", sm.calls, gotHandle)
	}
	if gotPrompt != "1+1" {
		t.Fatalf("expected temporary chat prompt without fixed @skill header, got %q", gotPrompt)
	}
}
