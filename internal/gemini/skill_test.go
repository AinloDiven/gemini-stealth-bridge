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

func TestSkillCacheValidRequiresSameTargetAndPageSession(t *testing.T) {
	const handle = "0123456789abcdef0123456789abcdef"
	if !skillCacheValid(handle, "research", "fsid-1", "research", "fsid-1") {
		t.Fatal("expected a confirmed handle to survive an empty listing")
	}
	if !skillCacheValid(handle, "@Research", "fsid-1", "research", "fsid-1") {
		t.Fatal("case and leading-@ differences must not invalidate a confirmed handle")
	}
	for _, tc := range []struct {
		name    string
		handle  string
		cached  string
		fsid    string
		target  string
		current string
	}{
		{name: "missing handle", cached: "research", fsid: "fsid-1", target: "research", current: "fsid-1"},
		{name: "different skill", handle: handle, cached: "other", fsid: "fsid-1", target: "research", current: "fsid-1"},
		{name: "page session changed", handle: handle, cached: "research", fsid: "fsid-1", target: "research", current: "fsid-2"},
	} {
		if skillCacheValid(tc.handle, tc.cached, tc.fsid, tc.target, tc.current) {
			t.Fatalf("%s: stale handle must not be reused", tc.name)
		}
	}
}

func TestExtractSkillRecordsAndMatch(t *testing.T) {
	handle := "0123456789abcdef0123456789abcdef"
	payload := []any{
		[]any{
			[]any{handle, "Research", "desc", "prompt"},
			[]any{"ffffffffffffffffffffffffffffffff", "other"},
		},
	}
	records := extractSkillRecords(payload)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %#v", records)
	}
	if !skillRecordMatches(records[0], "research") {
		t.Fatal("expected case-insensitive name match")
	}
	if !skillRecordMatches(records[0], normalizeSkillName("@research")) {
		t.Fatal("expected @-prefixed name match")
	}
	if skillRecordMatches(records[0], "missing") {
		t.Fatal("unexpected match")
	}
}

func TestExtractSkillRecordsSkipsMalformedFirstRecord(t *testing.T) {
	handle := "0123456789abcdef0123456789abcdef"
	payload := []any{
		[]any{
			[]any{"not-a-handle"},
			[]any{handle, "research"},
		},
	}
	records := extractSkillRecords(payload)
	if len(records) != 1 {
		t.Fatalf("expected only the valid sibling record, got %#v", records)
	}
	if !skillRecordMatches(records[0], "research") {
		t.Fatal("expected the valid record after the malformed one to match")
	}
}

func TestPersistStateKeepsAccountSkill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	ac := NewAccountClient("acct", "SID=value", path, nil, nil)
	ac.skill = "@research"
	ac.persistState()
	rec, _, err := cookies.LoadAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Skill != "@research" {
		t.Fatalf("expected account skill to survive cookie rotation, got %q", rec.Skill)
	}
}
