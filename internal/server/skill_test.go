package server

import "testing"

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
