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
	if res.Action.ToolID != "skills" || res.Action.ToolTitle != "Skill Title" || res.Action.StatusTemplate != template {
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

func TestParseActionStatusYouTubeInterpolatesToolNameAndDropsOrphanTemplate(t *testing.T) {
	res := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{
			nil,
			[]any{"youtube_tool", []any{nil, nil, "YouTube", "https://www.gstatic.com/images/branding/productlogos/youtube/v9/192px.svg", nil, "正在连接 {tool_logo_placeholder} {tool_name_placeholder}"}, float64(1)},
		}},
	})
	if res.Action == nil {
		t.Fatal("expected action card for youtube_tool")
	}
	if res.Action.ToolID != "youtube_tool" || res.Action.ToolTitle != "YouTube" || res.Action.StatusTemplate != "正在连接 {tool_logo_placeholder} YouTube" {
		t.Fatalf("unexpected youtube_tool action card: %+v", res.Action)
	}

	// Subsequent orphan completion template in slot 5 without a tool array must be ignored
	// so it never clobbers pending tool cards or leaks raw brace placeholders.
	orphan := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"已连接到 {tool_logo_placeholder} {tool_name_placeholder}"}}},
	})
	if orphan.Action != nil {
		t.Fatalf("expected nil Action for orphan tool placeholder template in slot 5, got %+v", orphan.Action)
	}
}

func TestParseCandidateSliceStripsYouTubeArtifactAndHoldsTrailingAutoLink(t *testing.T) {
	cStreaming := []any{
		"rc_yt",
		[]any{"视频链接：\n[http://www.youtube.com/watch?v=DAn34l](http://www.youtube.com/watch?v=DAn34l)"},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(1)},
	}
	f1, ok := parseCandidateSlice(cStreaming, "id1", "Flash")
	if !ok {
		t.Fatal("expected parseCandidateSlice f1 ok")
	}
	if f1.Text != "视频链接：\n" {
		t.Fatalf("expected incomplete trailing auto-link held back while streaming, got %q", f1.Text)
	}

	cDone := []any{
		"rc_yt",
		[]any{"视频链接：\n[http://www.youtube.com/watch?v=DAn34l_YrUM](http://www.youtube.com/watch?v=DAn34l_YrUM)\nhttp://googleusercontent.com/youtube_content/1"},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(2)},
	}
	f2, ok := parseCandidateSlice(cDone, "id1", "Flash")
	if !ok {
		t.Fatal("expected parseCandidateSlice f2 ok")
	}
	want := "视频链接：\n[http://www.youtube.com/watch?v=DAn34l_YrUM](http://www.youtube.com/watch?v=DAn34l_YrUM)"
	if f2.Text != want {
		t.Fatalf("expected youtube_content placeholder stripped and final link preserved, got %q, want %q", f2.Text, want)
	}
}
