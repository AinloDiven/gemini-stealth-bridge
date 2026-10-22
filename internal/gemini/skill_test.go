package gemini

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
)

func TestBuildInnerRequestArmsSkillSlot81(t *testing.T) {
	ac := &AccountClient{}
	raw, _, _ := ac.BuildInnerRequest("hi", StreamOptions{SkillRuntime: "0123456789abcdef0123456789abcdef"})
	var inner []any
	if err := json.Unmarshal([]byte(raw), &inner); err != nil {
		t.Fatal(err)
	}
	if len(inner) != 82 {
		t.Fatalf("expected 82 inner slots, got %d", len(inner))
	}
	arr, ok := inner[81].([]any)
	if !ok || len(arr) != 1 || arr[0] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("expected inner[81]=[handle], got %#v", inner[81])
	}

	empty, _, _ := ac.BuildInnerRequest("hi", StreamOptions{})
	var plain []any
	if err := json.Unmarshal([]byte(empty), &plain); err != nil {
		t.Fatal(err)
	}
	if plain[81] != nil {
		t.Fatalf("expected empty slot 81 without a skill, got %#v", plain[81])
	}
}

func TestExtractSkillRecordsAndMatch(t *testing.T) {
	handle := "0123456789abcdef0123456789abcdef"
	payload := []any{
		[]any{
			[]any{handle, "伊珂丝", "desc", "prompt"},
			[]any{"ffffffffffffffffffffffffffffffff", "other"},
		},
	}
	records := extractSkillRecords(payload)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %#v", records)
	}
	if !skillRecordMatches(records[0], "伊珂丝") {
		t.Fatal("expected name match")
	}
	if !skillRecordMatches(records[0], "eksie") {
		t.Fatal("expected alias match")
	}
	if skillRecordMatches(records[0], "missing") {
		t.Fatal("unexpected match")
	}
}

func TestPersistStateKeepsAccountSkill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	ac := NewAccountClient("acct", "SID=value", path, nil, nil)
	ac.skill = "/伊珂丝"
	ac.persistState()
	rec, _, err := cookies.LoadAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Skill != "/伊珂丝" {
		t.Fatalf("expected account skill to survive cookie rotation, got %q", rec.Skill)
	}
}
