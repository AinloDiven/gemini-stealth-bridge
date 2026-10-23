package gemini

import (
	"encoding/json"
	"testing"
)

func parseActionFromFrame(t *testing.T, inner []any) *LineParseResult {
	t.Helper()
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ParseFrameLine(string(envelope))
	if err != nil || res == nil {
		t.Fatalf("ParseFrameLine failed: %+v err=%v", res, err)
	}
	return res
}

func TestParseActionStatusThinkingTitle(t *testing.T) {
	res := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"Analyzing Video Content"}}},
	})
	if res.Action == nil || res.Action.ThinkingTitle != "Analyzing Video Content" {
		t.Fatalf("expected thinking title, got %+v", res.Action)
	}
}

func TestParseActionStatusToolCardRelaysIconInputs(t *testing.T) {
	template := "{tool_logo_placeholder} Skills: Eksie"
	res := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{
			template,
			[]any{"skills", []any{nil, nil, "Skill Title", "https://example.invalid/logo.svg", nil, template}, float64(1)},
		}},
	})
	if res.Action == nil {
		t.Fatal("expected action card")
	}
	if res.Action.ToolID != "skills" || res.Action.ToolTitle != "Skill Title" || res.Action.StatusTemplate != template || res.Action.State != "active" {
		t.Fatalf("unexpected action card: %+v", res.Action)
	}
}

func TestParseActionStatusMirrorSlot(t *testing.T) {
	action := []any{nil, nil, nil, nil, nil, []any{"Mirrored title"}}
	inner := make([]any, 12)
	inner[1] = []any{"c_1", "r_1"}
	inner[2] = nil
	inner[11] = []any{action}
	res := parseActionFromFrame(t, inner)
	if res.Action == nil || res.Action.ThinkingTitle != "Mirrored title" {
		t.Fatalf("expected mirrored thinking title, got %+v", res.Action)
	}
}
