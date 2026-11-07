package gemini

import (
	"encoding/json"
	"strings"
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
	template := "{tool_logo_placeholder} Skills: Research"
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
		[]any{"👉 **视频链接**：[[https://www.youtube](https://www.youtube)."},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(1)},
	}
	f1, ok := parseCandidateSlice(cStreaming, "id1", "Flash")
	if !ok {
		t.Fatal("expected parseCandidateSlice f1 ok")
	}
	if f1.Text != "👉 **视频链接**：" {
		t.Fatalf("expected incomplete trailing auto-link held back while streaming, got %q", f1.Text)
	}

	cDone := []any{
		"rc_yt",
		[]any{"👉 **视频链接**：[https://www.youtube.com/watch?v=SntsJWcCEhs](https://www.youtube.com/watch?v=SntsJWcCEhs)\nhttp://googleusercontent.com/youtube_content/1"},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(2)},
	}
	f2, ok := parseCandidateSlice(cDone, "id1", "Flash")
	if !ok {
		t.Fatal("expected parseCandidateSlice f2 ok")
	}
	want := "👉 **视频链接**：[https://www.youtube.com/watch?v=SntsJWcCEhs](https://www.youtube.com/watch?v=SntsJWcCEhs)"
	if f2.Text != want {
		t.Fatalf("expected youtube_content placeholder stripped and final link preserved, got %q, want %q", f2.Text, want)
	}
}

func TestTrimIncompleteTrailingURLPreservesPrecedingASCIIAndIsMonotonic(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"See (https://example.com/docs", "See ("},
		{"Read [guide](https://example.com/guide", "Read [guide]("},
		{`{"url":"https://example.com/api`, `{"url":"`},
		{"👉 **视频链接**：[[https://www.youtube](https://www.youtube).", "👉 **视频链接**："},
		{"plain text without url", "plain text without url"},
	}
	for _, tc := range cases {
		if got := trimIncompleteTrailingURL(tc.in); got != tc.want {
			t.Fatalf("trimIncompleteTrailingURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	streams := []string{
		"See (https://example.com/docs) for details.",
		"Read [the guide](https://example.com/guide) next.",
		`{"tool_calls":[{"name":"fetch","arguments":{"url":"https://example.com/api"}}]} `,
		"👉 **视频链接**：[[https://www.youtube](https://www.youtube).",
	}
	for _, full := range streams {
		prev := ""
		for k := 1; k <= len(full); k++ {
			// Only check valid UTF-8 prefix boundaries.
			if k < len(full) && (full[k]&0xc0) == 0x80 {
				continue
			}
			cur := trimIncompleteTrailingURL(full[:k])
			if !strings.HasPrefix(cur, prev) {
				t.Fatalf("non-monotonic trim on %q at %d: prev=%q cur=%q", full, k, prev, cur)
			}
			prev = cur
		}
	}
}
