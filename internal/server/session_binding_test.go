package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

func recordingSessionClient(prompts *[]string, metas *[][]any) *mockUpstream {
	var mu sync.Mutex
	return &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		mu.Lock()
		if prompts != nil {
			*prompts = append(*prompts, prompt)
		}
		if metas != nil {
			*metas = append(*metas, append([]any(nil), opts.Metadata...))
		}
		turn := 1
		if prompts != nil {
			turn = len(*prompts)
		}
		mu.Unlock()
		ansID := testModel.ModelID
		if opts.Model != nil && opts.Model.ModelID != "" {
			ansID = opts.Model.ModelID
		}
		if err := onFrame(&gemini.LineParseResult{
			CID: "cid-1", RID: "rid-1", ContextToken: "ctx-1",
			AnsweredModelID: ansID,
			Frames:          []gemini.Frame{makeFrame(fmt.Sprintf("answer %d", turn), "", true, ansID)},
		}); err != nil {
			return nil, err
		}
		return &gemini.StreamResponse{Status: 200, CID: "cid-1", RID: "rid-1"}, nil
	}}
}

func TestExplicitSessionBindingReusesContinuation(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var prompts []string
	var metas [][]any
	acct.Client = recordingSessionClient(&prompts, &metas)
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "sess-1"}

	rec1 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec1, map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "one"}},
	}, scope)
	if rec1.Code != 200 || len(metas[0]) != 0 {
		t.Fatalf("turn 1 failed: code=%d meta=%#v", rec1.Code, metas)
	}
	if b := srv.getSessionBinding(sessionBindingKey(scope, scope.SessionID)); b == nil || b.NextLen != 2 {
		t.Fatalf("expected binding with NextLen=2, got %+v", b)
	}

	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "assistant", "content": "answer 1"},
			map[string]any{"role": "user", "content": "two"},
		},
	}, scope)
	if rec2.Code != 200 || len(metas[1]) == 0 {
		t.Fatalf("turn 2 expected bound metadata: code=%d metas=%#v", rec2.Code, metas)
	}
	if !strings.Contains(prompts[1], "two") || strings.Contains(prompts[1], "one") {
		t.Fatalf("expected delta prompt to contain only new message, got %q", prompts[1])
	}
}

func TestExplicitSessionBindingWithoutLocalHistory(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var prompts []string
	var metas [][]any
	acct.Client = recordingSessionClient(&prompts, &metas)
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "sess-no-history"}

	for i, msg := range []string{"one", "two"} {
		rec := httptest.NewRecorder()
		srv.HandleChat(context.Background(), rec, map[string]any{
			"model":    testModel.ModelName,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}, scope)
		if rec.Code != 200 {
			t.Fatalf("turn %d failed: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if len(metas[1]) == 0 || !strings.Contains(prompts[1], "two") || strings.Contains(prompts[1], "one") {
		t.Fatalf("expected continuation without local history, metas=%#v prompt=%q", metas[1], prompts[1])
	}
}

func TestExplicitSessionBindingDoesNotCrossAccounts(t *testing.T) {
	srv, acctA := newTestHarness(false, true)
	acctA.ID, acctA.Email = "acctA", "a@example.com"
	acctB := &AccountState{ID: "acctB", Email: "b@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2}
	srv.SetAccounts([]*AccountState{acctA, acctB})

	var metas [][]any
	client := recordingSessionClient(nil, &metas)
	acctA.Client, acctB.Client = client, client
	scope := AuthScope{OK: true, Admin: true, Token: "pool", SessionID: "sess-cross-account"}

	rec1 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec1, map[string]any{"model": testModel.ModelName, "messages": []any{map[string]any{"role": "user", "content": "one"}}}, scope)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d", rec1.Code)
	}
	acctA.mu.Lock()
	acctA.Disabled = true
	acctA.mu.Unlock()

	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, map[string]any{"model": testModel.ModelName, "messages": []any{map[string]any{"role": "user", "content": "two"}}}, scope)
	if rec2.Code != 200 || len(metas) < 2 || len(metas[1]) != 0 {
		t.Fatalf("must not send account A continuation on account B: code=%d metas=%#v", rec2.Code, metas)
	}
}

func TestSessionBindingKeyScopesByAPIKeyAndScope(t *testing.T) {
	base := AuthScope{OK: true, Token: "key-a", SessionID: "same-session"}
	key := sessionBindingKey(base, base.SessionID)
	for _, other := range []AuthScope{
		{OK: true, Token: "key-b", SessionID: base.SessionID},
		{OK: true, Token: "key-a", AccountID: "other", SessionID: base.SessionID},
		{OK: true, Token: "key-a", Group: "other", SessionID: base.SessionID},
	} {
		if sessionBindingKey(other, other.SessionID) == key {
			t.Fatalf("binding key must isolate %+v from %+v", other, base)
		}
	}
	if sessionBindingKey(base, "other-session") == key {
		t.Fatal("different session ids must not share a binding key")
	}
}

func TestSessionBindingTTLAndLRUEviction(t *testing.T) {
	srv, _ := newTestHarness(false, true)
	srv.Cfg.HomeDir = ""
	srv.Cfg.SessionBindingTTL = time.Hour
	cur := time.Now()
	srv.clock = func() time.Time { return cur }

	meta := []any{"cid", "rid", "rcid", nil, nil, nil, nil, nil, nil, "ctx"}
	srv.putSessionBinding(sessionBinding{Key: "sid:expired", AccountID: "acct", Model: testModel.ModelName, Meta: meta})
	cur = cur.Add(2 * time.Hour)
	if got := srv.getSessionBinding("sid:expired"); got != nil {
		t.Fatalf("expired binding must not be returned, got %+v", got)
	}

	srv.Cfg.SessionBindingMax = 2
	for _, key := range []string{"sid:a", "sid:b", "sid:c", "sid:d"} {
		cur = cur.Add(time.Second)
		srv.putSessionBinding(sessionBinding{Key: key, AccountID: "acct", Model: testModel.ModelName, Meta: meta})
	}
	if srv.getSessionBinding("sid:a") != nil || srv.getSessionBinding("sid:b") != nil || srv.getSessionBinding("sid:d") == nil {
		t.Fatal("expected oldest bindings evicted and newest retained")
	}
}

func TestResponsesExplicitSessionBindingReusesContinuation(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var metas [][]any
	acct.Client = recordingSessionClient(nil, &metas)
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "responses-session"}

	for i, input := range []string{"one", "two"} {
		rec := httptest.NewRecorder()
		srv.HandleResponses(context.Background(), rec, map[string]any{"model": testModel.ModelName, "input": input}, scope)
		if rec.Code != 200 {
			t.Fatalf("responses turn %d failed: %d", i+1, rec.Code)
		}
	}
	if len(metas) != 2 || len(metas[1]) == 0 {
		t.Fatalf("responses turn 2 must reuse session binding, metas=%#v", metas)
	}
}

func TestExplicitSessionBindingSurvivesModelChangeAndSkipsTemporary(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	alt := testModel
	alt.ModelName, alt.ModelID, alt.DisplayName, alt.ModelNumber = "gemini-flash-alt", "cccccccccccccccc", "Alt", 2
	acct.Models = []gemini.ModelInfo{testModel, alt}

	var prompts []string
	var metas [][]any
	acct.Client = recordingSessionClient(&prompts, &metas)
	scope := AuthScope{OK: true, AccountID: "acct", Token: "acct", SessionID: "sess-model"}

	rec1 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec1, map[string]any{"model": testModel.ModelName, "messages": []any{map[string]any{"role": "user", "content": "one"}}}, scope)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d", rec1.Code)
	}
	history := []any{
		map[string]any{"role": "user", "content": "one"},
		map[string]any{"role": "assistant", "content": "answer 1"},
		map[string]any{"role": "user", "content": "two"},
	}
	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, map[string]any{"model": alt.ModelName, "messages": history}, scope)
	if rec2.Code != 200 || len(metas[1]) == 0 {
		t.Fatalf("model change must reuse continuation: code=%d metas=%#v", rec2.Code, metas)
	}
	recTemp := httptest.NewRecorder()
	srv.HandleChat(context.Background(), recTemp, map[string]any{"model": testModel.ModelName, "temporary": true, "messages": history}, scope)
	if recTemp.Code != 200 || len(metas[2]) != 0 {
		t.Fatalf("temporary request must not reuse session binding: code=%d metas=%#v", recTemp.Code, metas)
	}
}

func TestSessionHeaderValidationAndPersistenceAcrossRestart(t *testing.T) {
	srv, _ := newTestHarness(false, true)
	srv.Cfg.HomeDir = t.TempDir()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("X-GSB-Session", "bad id!")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid X-GSB-Session") {
		t.Fatalf("expected 400 invalid X-GSB-Session, got %d: %s", rec.Code, rec.Body.String())
	}

	key := "sid:test-restart"
	srv.putSessionBinding(sessionBinding{
		Key: key, AccountID: "acct", Model: testModel.ModelName,
		Meta: []any{"cid", "rid", "rcid", nil, nil, nil, nil, nil, nil, "ctx"}, NextLen: 2, HistoryHash: "hash",
	})
	srv.FlushSessionCacheNow()
	got := New(srv.Cfg).getSessionBinding(key)
	if got == nil || got.AccountID != "acct" || got.NextLen != 2 || got.HistoryHash != "hash" {
		t.Fatalf("binding did not survive restart: %+v", got)
	}
}

func TestDefaultModelSelectionAndFallback(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	alt := testModel
	alt.ModelName, alt.ModelID, alt.DisplayName, alt.ModelNumber = "gemini-flash-alt", "cccccccccccccccc", "Alt", 2
	acct.Models = []gemini.ModelInfo{testModel, alt}

	var got string
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		got = opts.Model.ModelName
		_ = onFrame(&gemini.LineParseResult{CID: "c", RID: "r", AnsweredModelID: opts.Model.ModelID, Frames: []gemini.Frame{makeFrame("ok", "", true, opts.Model.ModelID)}})
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}
	acct.Client = client

	srv.Cfg.DefaultModel = alt.ModelName
	if rec := runChat(t, srv, acct, client, map[string]any{"model": ""}); rec.Code != 200 || got != alt.ModelName {
		t.Fatalf("expected configured default %q, got code=%d model=%q", alt.ModelName, rec.Code, got)
	}

	srv.Cfg.DefaultModel = "missing-model"
	if rec := runChat(t, srv, acct, client, map[string]any{"model": ""}); rec.Code != 200 || got != testModel.ModelName {
		t.Fatalf("expected fallback to %q, got code=%d model=%q", testModel.ModelName, rec.Code, got)
	}
}

func TestPoolKeySessionBindingSurvivesPoolGrowthAndLeavesRotateCursorIntact(t *testing.T) {
	srv, acct1 := newTestHarness(false, true)
	srv.Cfg.WebUI = true
	srv.Cfg.APIKey = "public"
	srv.Cfg.AdminKey = "adm-secret"
	srv.Cfg.AccountTokens = nil
	srv.Cfg.AccountStrategy = "rotate"

	var acct1Metas, acct2Metas [][]any
	acct1.Client = recordingSessionClient(nil, &acct1Metas)
	acct2 := &AccountState{
		ID: "acct2", Email: "acct2@example.com", Models: []gemini.ModelInfo{testModel}, Capacity: 2, ModelsAt: time.Now(),
		Client: recordingSessionClient(nil, &acct2Metas),
	}

	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"turn one"}]}`, testModel.ModelName)))
	req1.Header.Set("X-GSB-Session", "pool-sess-1")
	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}

	srv.SetAccounts([]*AccountState{acct1, acct2})
	srv.setRotateCursor("acct2")

	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"turn two"}]}`, testModel.ModelName)))
	req2.Header.Set("Authorization", "Bearer public")
	req2.Header.Set("X-GSB-Session", "pool-sess-1")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != 200 || len(acct2Metas) != 0 || len(acct1Metas) != 2 || len(acct1Metas[1]) == 0 {
		t.Fatalf("expected turn 2 on acct1 with continuation: code=%d acct2=%d acct1=%#v", rec2.Code, len(acct2Metas), acct1Metas)
	}
	if srv.rotateCursor != "acct2" {
		t.Fatalf("bound session turn must not mutate rotateCursor, got %q", srv.rotateCursor)
	}

	delReq := httptest.NewRequest("DELETE", "/v1/admin/accounts/"+acct1.ID, nil)
	delReq.Header.Set("Authorization", "Bearer adm-secret")
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, delReq)
	if delRec.Code != 200 || srv.sessionBindingLen() != 0 {
		t.Fatalf("expected deleted account bindings purged: code=%d len=%d", delRec.Code, srv.sessionBindingLen())
	}
}
