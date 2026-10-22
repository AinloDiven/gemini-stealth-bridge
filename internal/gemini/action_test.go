package gemini

import (
	"encoding/json"
	"testing"
)

func TestParseActionStatusThinkingTitle(t *testing.T) {
	var c []any
	candidate := `["rc_1",["answer"],{"7":[null,null,null,null,null,["Analyzing Video Content"]]},null,null,null,null,null,[[2]]]`
	if err := json.Unmarshal([]byte(candidate), &c); err != nil {
		t.Fatal(err)
	}
	action := parseActionStatus(c)
	if action == nil || action.ThinkingTitle != "Analyzing Video Content" {
		t.Fatalf("expected thinking title, got %+v", action)
	}
}

func TestParseActionStatusToolCardRelaysIconInputs(t *testing.T) {
	var c []any
	candidate := `["rc_1",["answer"],{"7":[null,["skills",[null,null,"Skill Title",null,null,"{tool_logo_placeholder} Skills: Eksie"],1]]},null,null,null,null,null,[[2]]]`
	if err := json.Unmarshal([]byte(candidate), &c); err != nil {
		t.Fatal(err)
	}
	action := parseActionStatus(c)
	if action == nil {
		t.Fatal("expected action card")
	}
	if action.ToolID != "skills" || action.ToolTitle != "Skill Title" || action.StatusTemplate != "{tool_logo_placeholder} Skills: Eksie" || action.State != "active" {
		t.Fatalf("unexpected action card: %+v", action)
	}
}

func TestParseActionStatusMirrorSlot(t *testing.T) {
	var c []any
	candidate := `["rc_1",["answer"],null,null,null,null,null,null,null,null,null,[[null,null,null,null,null,["Mirrored title"]]]]`
	if err := json.Unmarshal([]byte(candidate), &c); err != nil {
		t.Fatal(err)
	}
	action := parseActionStatus(c)
	if action == nil || action.ThinkingTitle != "Mirrored title" {
		t.Fatalf("expected mirrored thinking title, got %+v", action)
	}
}
