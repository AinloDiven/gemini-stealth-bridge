package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// mcpSearchUpstream returns a mock upstream that answers a search with a
// grounded citation list.
func mcpSearchUpstream(answer string, sources []gemini.GroundingSource, captured *gemini.StreamOptions) *mockUpstream {
	return &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			if captured != nil {
				*captured = opts
			}
			_ = onFrame(&gemini.LineParseResult{
				CID:              "c1",
				RID:              "r1",
				AnsweredModelID:  testModel.ModelID,
				GroundingSources: sources,
				Frames: []gemini.Frame{
					{
						RCID:            "candidate",
						Text:            answer,
						Done:            true,
						AnsweredModelID: testModel.ModelID,
					},
				},
			})
			return &gemini.StreamResponse{
				Status:           200,
				CID:              "c1",
				RID:              "r1",
				GroundingSources: sources,
			}, nil
		},
	}
}

func mcpTestServer(t *testing.T, client *mockUpstream) *Server {
	t.Helper()
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountTokens = map[string]string{"audit": "acct"}
	srv := New(cfg)
	t.Cleanup(func() { srv.FlushSessionCacheNow() })
	acct := &AccountState{
		ID:       "acct",
		Email:    "acct@example.com",
		Client:   client,
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct})
	return srv
}

func postMCP(t *testing.T, srv *Server, path, token string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("unused", nil, nil))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-06-18"},
	})
	if rec.Code != 200 {
		t.Fatalf("initialize failed with %d: %s", rec.Code, rec.Body.String())
	}
	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("decode initialize: %v (%s)", err, rec.Body.String())
	}
	if initResp.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("unexpected protocol version %q", initResp.Result.ProtocolVersion)
	}
	if initResp.Result.Capabilities.Tools == nil {
		t.Fatalf("expected tools capability, got %+v", initResp.Result.Capabilities)
	}
	if initResp.Result.ServerInfo.Name != "gsb-mcp" {
		t.Fatalf("unexpected server name %q", initResp.Result.ServerInfo.Name)
	}

	recList := postMCP(t, srv, "/v1/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	if recList.Code != 200 {
		t.Fatalf("tools/list failed with %d: %s", recList.Code, recList.Body.String())
	}
	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, recList.Body.String())
	}
	if len(listResp.Result.Tools) != 1 || listResp.Result.Tools[0].Name != "web_search" {
		t.Fatalf("expected a single web_search tool, got %+v", listResp.Result.Tools)
	}
}

func TestMCPToolsCallReturnsAnswerWithCitations(t *testing.T) {
	var captured gemini.StreamOptions
	srv := mcpTestServer(t, mcpSearchUpstream("Rust 1.90 released.", []gemini.GroundingSource{
		{URL: "https://blog.rust-lang.org/1.90", Title: "Rust 1.90", Snippet: "release notes"},
	}, &captured))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]any{"query": "Rust 1.90 release"},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("tools/call failed with %d: %s", rec.Code, rec.Body.String())
	}
	var callResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &callResp); err != nil {
		t.Fatalf("decode tools/call: %v (%s)", err, rec.Body.String())
	}
	if callResp.Result.IsError {
		t.Fatalf("expected a successful tool result, got %+v", callResp.Result)
	}
	if len(callResp.Result.Content) != 1 {
		t.Fatalf("expected one content block, got %+v", callResp.Result.Content)
	}
	text := callResp.Result.Content[0].Text
	if !strings.Contains(text, "Rust 1.90 released.") {
		t.Fatalf("expected the answer in the tool result, got %q", text)
	}
	if !strings.Contains(text, "Sources:") || !strings.Contains(text, "https://blog.rust-lang.org/1.90") {
		t.Fatalf("expected the citation list in the tool result, got %q", text)
	}
	// The search must not pollute the user's Gemini conversation list.
	if !captured.Temporary {
		t.Fatalf("expected a temporary chat for web search, got opts %+v", captured)
	}
	if captured.HasTools || captured.DisableTools {
		t.Fatalf("expected consumer tools enabled for grounding, got opts %+v", captured)
	}
}

func TestMCPToolsCallRejectsEmptyQuery(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("unused", nil, nil))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]any{"query": "   "},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 with a tool error, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/call: %v (%s)", err, rec.Body.String())
	}
	if !resp.Result.IsError {
		t.Fatalf("expected isError for an empty query, got %+v", resp.Result)
	}
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, "query") {
		t.Fatalf("expected an explanatory error message, got %+v", resp.Result.Content)
	}
}

func TestMCPNotificationReturnsAcceptedAndGetIsNotAllowed(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("unused", nil, nil))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for a notification, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty notification body, got %q", rec.Body.String())
	}

	reqGet := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	recGet := httptest.NewRecorder()
	srv.ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET /mcp, got %d", recGet.Code)
	}
	if allow := recGet.Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Fatalf("expected Allow: POST, got %q", allow)
	}
}

func TestMCPRequiresValidAPIKey(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("unused", nil, nil))

	rec := postMCP(t, srv, "/mcp", "not-a-real-key", map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/list",
	})
	if rec.Code != 200 {
		t.Fatalf("expected a JSON-RPC error payload, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error *mcpError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error: %v (%s)", err, rec.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != 401 {
		t.Fatalf("expected a 401 JSON-RPC error, got %+v", resp.Error)
	}
}

func TestMCPToolsCallRejectsCannedUpstreamError(t *testing.T) {
	// Gemini Web answers a blocked request with a canned apology and HTTP 200.
	// The MCP path must not hand that back as a search result.
	srv := mcpTestServer(t, mcpSearchUpstream("I encountered an error doing what you asked. Could you try again?", nil, nil))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]any{"query": "anything"},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 with a tool error, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/call: %v (%s)", err, rec.Body.String())
	}
	if !resp.Result.IsError {
		t.Fatalf("expected a canned upstream reply to surface as an error, got %+v", resp.Result)
	}
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, "search failed") {
		t.Fatalf("expected a search-failed error message, got %+v", resp.Result.Content)
	}
}

func TestMCPSearchTimeoutClamps(t *testing.T) {
	cfg := config.Load()
	cfg.SearchTimeout = 180 * time.Second
	srv := New(cfg)

	cases := []struct {
		requested int
		want      time.Duration
	}{
		{0, 180 * time.Second},
		{-5, 180 * time.Second},
		{1, 10 * time.Second},
		{45, 45 * time.Second},
		{9999, 600 * time.Second},
	}
	for _, c := range cases {
		if got := srv.mcpSearchTimeout(c.requested); got != c.want {
			t.Fatalf("mcpSearchTimeout(%d) = %s, want %s", c.requested, got, c.want)
		}
	}

	// The operator's default applies when the caller does not pass one.
	cfg.SearchTimeout = 45 * time.Second
	srvOp := New(cfg)
	if got := srvOp.mcpSearchTimeout(0); got != 45*time.Second {
		t.Fatalf("expected the configured default of 45s, got %s", got)
	}
}

func TestMCPToolsCallHonorsTimeout(t *testing.T) {
	// The upstream never answers, so only the deadline can end the call.
	srv := mcpTestServer(t, &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "web_search",
			"arguments": map[string]any{
				"query":           "slow question",
				"timeout_seconds": 10,
			},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 with a tool error, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/call: %v (%s)", err, rec.Body.String())
	}
	if !resp.Result.IsError {
		t.Fatalf("expected a timeout to surface as an error, got %+v", resp.Result)
	}
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, "deadline") {
		t.Fatalf("expected an actionable deadline message, got %+v", resp.Result.Content)
	}
}

func TestMCPSuccessSetsDiagnosticHeaders(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("answer", []gemini.GroundingSource{
		{URL: "https://example.com/a", Title: "Example A"},
	}, nil))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]any{"query": "anything"},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-GSB-Account"); got != "acct" {
		t.Fatalf("expected X-GSB-Account acct, got %q", got)
	}
	if got := rec.Header().Get("X-GSB-Sources"); got != "1" {
		t.Fatalf("expected X-GSB-Sources 1, got %q", got)
	}
	if rec.Header().Get("X-GSB-Duration-Ms") == "" {
		t.Fatalf("expected X-GSB-Duration-Ms to be set")
	}
}

func TestMCPUnknownMethodReturnsMethodNotFound(t *testing.T) {
	srv := mcpTestServer(t, mcpSearchUpstream("unused", nil, nil))

	rec := postMCP(t, srv, "/mcp", "audit", map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "resources/list",
	})
	var resp struct {
		Error *mcpError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error: %v (%s)", err, rec.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("expected -32601 method not found, got %+v", resp.Error)
	}
}
