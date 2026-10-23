package server

import (
	"context"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

type skillUpstream struct {
	*mockUpstream
	handle string
	err    error
	name   string
}

func (m *skillUpstream) BindSkillRuntime(ctx context.Context, name string) (string, error) {
	m.name = name
	return m.handle, m.err
}

func TestFormatSkillPromptIsIdempotentAndKeepsByteZero(t *testing.T) {
	cases := []struct{ text, want string }{
		{"hi", "/伊珂丝 hi"},
		{"/伊珂丝 hi", "/伊珂丝 hi"},
		{"/伊珂丝\nhi", "/伊珂丝\nhi"},
		{"/伊珂丝hi", "/伊珂丝 hi"},
		{"/伊珂丝", "/伊珂丝"},
		{"", "/伊珂丝"},
	}
	for _, tc := range cases {
		if got := formatSkillPrompt(tc.text, "伊珂丝"); got != tc.want {
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
}

func TestSkillBindingArmsSlotAndPrefixesPrompt(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "/伊珂丝"
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
	if sm.name != "/伊珂丝" || handle != sm.handle {
		t.Fatalf("expected skill %q handle %q, got name=%q handle=%q", "/伊珂丝", sm.handle, sm.name, handle)
	}
	if !strings.HasPrefix(prompt, "/伊珂丝 synthetic") {
		t.Fatalf("expected skill prefix at byte 0, got %q", prompt)
	}
}

func TestSkillBindingFailureDoesNotFailTurn(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	srv.Cfg.Skill = "/伊珂丝"
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
	if prompt != "/伊珂丝 synthetic" {
		t.Fatalf("failed slot must keep the configured text projection, got %q", prompt)
	}
}
