package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// The MCP endpoint exposes Gemini Web's Google Search grounding as a tool that
// coding agents (Claude Code, Cursor, OMP, ...) can call. It rides the same
// daemon, account pool, quota accounting and failover as the OpenAI-compatible
// endpoints, so a search costs the same quota and survives an exhausted
// account exactly like a chat request.
//
// The protocol surface is small (initialize / tools/list / tools/call / ping),
// so it is implemented directly as JSON-RPC 2.0 over Streamable HTTP instead of
// pulling in an MCP SDK.

const (
	mcpProtocolVersion = "2025-06-18"
	mcpMaxBodyBytes    = 8 << 20
	mcpToolWebSearch   = "web_search"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// HandleMCP serves the MCP Streamable HTTP transport.
//
// The transport is request/response only: no server-initiated SSE stream, so
// GET answers 405 and notifications (messages without an id) answer 202 with an
// empty body, per the Streamable HTTP specification.
func (s *Server) HandleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, Mcp-Session-Id, MCP-Protocol-Version")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP endpoint is POST-only (no server-initiated stream)", http.StatusMethodNotAllowed)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	scope := s.ResolveAuthScope(r)
	if !scope.OK {
		s.writeMCPError(w, nil, 401, "invalid api key")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBodyBytes+1))
	if err != nil {
		s.writeMCPError(w, nil, -32700, "read error")
		return
	}
	if len(raw) > mcpMaxBodyBytes {
		s.writeMCPError(w, nil, -32600, "request body exceeds 8MB limit")
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeMCPError(w, nil, -32700, "parse error")
		return
	}

	resp := s.dispatchMCP(r.Context(), scope, &req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) writeMCPError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: msg},
	})
}

// dispatchMCP routes one JSON-RPC message. A nil result means the message was a
// notification, which gets no response body.
func (s *Server) dispatchMCP(ctx context.Context, scope AuthScope, req *mcpRequest) *mcpResponse {
	if len(req.ID) == 0 {
		return nil
	}
	ok := func(result any) *mcpResponse {
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string) *mcpResponse {
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: code, Message: msg}}
	}

	switch req.Method {
	case "initialize":
		version := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
		return ok(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gsb-mcp", "version": config.Version},
		})

	case "ping":
		return ok(map[string]any{})

	case "tools/list":
		return ok(map[string]any{"tools": []any{mcpWebSearchToolDef()}})

	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Query string `json:"query"`
				Model string `json:"model"`
			} `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			return fail(-32602, "invalid params")
		}
		if p.Name != mcpToolWebSearch {
			return fail(-32602, "unknown tool: "+p.Name)
		}
		query := strings.TrimSpace(p.Arguments.Query)
		if query == "" {
			return ok(mcpToolText("query is required"))
		}
		text, err := s.mcpWebSearch(ctx, scope, query, strings.TrimSpace(p.Arguments.Model))
		if err != nil {
			return ok(mcpToolText("search failed: " + err.Error()))
		}
		return ok(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		})

	default:
		return fail(-32601, "method not found: "+req.Method)
	}
}

func mcpToolText(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

// mcpWebSearchToolDef describes the web_search tool and its JSON Schema.
func mcpWebSearchToolDef() map[string]any {
	return map[string]any{
		"name": mcpToolWebSearch,
		"description": "Search the web via Google Gemini and return a synthesized answer " +
			"with source links. Use for current events, facts, or anything needing " +
			"up-to-date information from the internet.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query or question to research.",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override (gemini-flash, gemini-flash-lite, gemini-pro, gemini-argon).",
				},
			},
			"required": []any{"query"},
		},
	}
}

// searchModelPreferences lists the models that can ground a web search, most
// capable first. A caller-supplied model wins when the account serves it.
var searchModelPreferences = []string{"gemini-flash", "gemini-flash-lite", "gemini-pro", "gemini-argon"}

// resolveSearchModel picks the model used for a web search on one account.
func resolveSearchModel(a *AccountState, requested string) *gemini.ModelInfo {
	if requested != "" {
		return resolveModelOnAccount(a, requested)
	}
	for _, name := range searchModelPreferences {
		if m := resolveModelOnAccount(a, name); m != nil {
			return m
		}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.Models) == 0 {
		return nil
	}
	m := a.Models[0]
	return &m
}

// mcpWebSearch runs one grounded search and returns the answer plus its
// citations.
//
// The search runs as a temporary chat so the user's Gemini conversation list
// stays clean, and it is not stored in the session-continuation cache, so a
// later coding-agent turn cannot inherit a half-written search thread.
func (s *Server) mcpWebSearch(ctx context.Context, scope AuthScope, query, requestedModel string) (string, error) {
	accts := s.scopedAccounts(scope)
	if len(accts) == 0 {
		return "", transport.NewUpstreamError("no accounts available for this api key", 503, "auth_unavailable", nil, nil)
	}

	// The pool picks the account the same way a chat request does, so a search
	// follows the rotation cursor and the configured strategy instead of
	// always hitting the first loaded account.
	ordered := s.orderSearchAccounts(accts, scope, requestedModel)

	var lastErr error
	for _, acct := range ordered {
		model := resolveSearchModel(acct, requestedModel)
		if model == nil {
			lastErr = transport.NewUpstreamError(
				fmt.Sprintf("account %s cannot serve a web search", acct.ID),
				404, "model_not_found", nil, nil,
			)
			continue
		}
		text, sources, err := s.runSearchOnAccount(ctx, acct, model, query)
		if err != nil {
			lastErr = err
			continue
		}
		return formatSearchResult(text, sources), nil
	}
	if lastErr == nil {
		lastErr = transport.NewUpstreamError("no account could serve the search", 503, "auth_unavailable", nil, nil)
	}
	return "", lastErr
}

// orderSearchAccounts returns the accounts to try, best first. The pool's own
// selection decides the head of the list; the remaining accounts follow as
// failover targets.
func (s *Server) orderSearchAccounts(accts []*AccountState, scope AuthScope, requestedModel string) []*AccountState {
	ordered := make([]*AccountState, 0, len(accts))
	seen := make(map[string]bool, len(accts))

	preferred := s.resolveChatAccount(accts, scope, requestedModel, nil)
	if preferred != nil {
		ordered = append(ordered, preferred)
		seen[preferred.ID] = true
	}
	for _, a := range accts {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		ordered = append(ordered, a)
	}
	return ordered
}

// runSearchOnAccount performs the grounded search on a single account.
func (s *Server) runSearchOnAccount(ctx context.Context, acct *AccountState, model *gemini.ModelInfo, query string) (string, []gemini.GroundingSource, error) {
	msgs := []map[string]any{{"role": "user", "content": query}}
	body := map[string]any{
		"model":     model.ModelName,
		"messages":  []any{msgs[0]},
		"temporary": true,
	}
	accountScope := AuthScope{OK: true, AccountID: acct.ID}
	prep, err := s.prepareGeneration(ctx, body, accountScope, msgs, nil, nil)
	if err != nil {
		return "", nil, err
	}

	opts := prep.opts
	// A grounded search needs the same payload shape as a plain browser
	// session: consumer tools enabled (inner[11]=0) so the Google Search
	// extension is reachable, and a temporary chat so the research thread does
	// not land in the user's Gemini conversation list.
	opts.Model = model
	opts.Temporary = true
	opts.Metadata = nil
	opts.Files = nil
	opts.HasTools = false
	opts.DisableTools = false
	opts.ProtocolMode = "web"

	var (
		text        string
		sources     []gemini.GroundingSource
		candidateID string
	)
	client := s.clientFor(prep.account)
	upstream, streamErr := client.StreamGenerateLive(ctx, query, opts, func(update *gemini.LineParseResult) error {
		sources = gemini.MergeGroundingSources(sources, update.GroundingSources)
		for _, f := range update.Frames {
			if candidateID == "" {
				candidateID = f.RCID
			}
			if f.RCID != candidateID {
				continue
			}
			if f.Text != "" {
				text = f.Text
			}
		}
		return nil
	})
	if streamErr != nil {
		return "", nil, streamErr
	}
	if upstream != nil && upstream.Status != 200 {
		return "", nil, transport.NewUpstreamError(
			fmt.Sprintf("upstream HTTP %d during web search", upstream.Status),
			502, "upstream_http_error", upstream.Headers, nil,
		)
	}
	if upstream != nil {
		sources = gemini.MergeGroundingSources(sources, upstream.GroundingSources)
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, transport.NewUpstreamError("upstream returned no content for the search", 502, "empty_upstream_response", nil, nil)
	}
	// Gemini Web answers a blocked request with a canned apology and HTTP 200.
	// Treating that as a result would hand the caller a useless answer instead
	// of failing over to an account that can actually search.
	if reply := gemini.GeminiErrorReply(text); reply != "" {
		return "", nil, transport.NewUpstreamError(
			fmt.Sprintf("upstream returned an error message instead of a search result: %q", reply),
			502, "upstream_error_reply", upstreamHeaders(upstream), nil,
		)
	}
	return text, sources, nil
}

// upstreamHeaders returns the response headers of a stream, if any.
func upstreamHeaders(upstream *gemini.StreamResponse) map[string]string {
	if upstream == nil {
		return nil
	}
	return upstream.Headers
}

// formatSearchResult renders the answer followed by its numbered citations.
func formatSearchResult(text string, sources []gemini.GroundingSource) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	if len(sources) == 0 {
		return b.String()
	}
	b.WriteString("\n\n---\nSources:\n")
	for i, s := range sources {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = s.URL
		}
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, title, s.URL)
		if sn := strings.TrimSpace(s.Snippet); sn != "" {
			fmt.Fprintf(&b, "   %s\n", sn)
		}
	}
	return b.String()
}
