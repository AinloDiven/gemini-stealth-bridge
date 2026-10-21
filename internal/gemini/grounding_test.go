package gemini

import (
	"encoding/json"
	"testing"
)

// groundingCandidate builds a candidate slice whose slot 2 carries Google
// Search grounding, the slot layout Gemini Web uses for web-search citations:
// candidate[2][1] is the chunk list, chunk[2] the source list, and each source
// is [url, title, favicon, snippet].
func groundingCandidate(rcid, text string, sources [][]any) []any {
	srcs := make([]any, 0, len(sources))
	for _, s := range sources {
		srcs = append(srcs, s)
	}
	chunk := []any{nil, nil, srcs}
	grounding := []any{nil, []any{chunk}}
	candidate := make([]any, 9)
	candidate[0] = rcid
	candidate[1] = []any{text}
	candidate[2] = grounding
	candidate[8] = []any{float64(2)} // done
	return candidate
}

// groundingFrameLine renders a full wrb.fr envelope around one candidate.
func groundingFrameLine(t *testing.T, candidate []any) string {
	t.Helper()
	inner := []any{
		nil,
		[]any{"c_1", "r_1"},
		nil,
		nil,
		[]any{candidate},
		map[string]any{"26": "ctx_1"},
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	innerEsc, err := marshalJSONString(string(innerJSON))
	if err != nil {
		t.Fatalf("escape inner: %v", err)
	}
	return `[["wrb.fr",null,` + innerEsc + `]]`
}

func TestParseFrameLineExtractsGroundingSources(t *testing.T) {
	line := groundingFrameLine(t, groundingCandidate("rc_1", "answer text", [][]any{
		{"https://example.com/a", "Example A", "favicon", "snippet A"},
		{"https://example.com/b", "Example B", "favicon", "snippet B"},
	}))

	r, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(r.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(r.Frames))
	}
	sources := r.Frames[0].GroundingSources
	if len(sources) != 2 {
		t.Fatalf("expected 2 grounding sources, got %d: %+v", len(sources), sources)
	}
	if sources[0].URL != "https://example.com/a" || sources[0].Title != "Example A" || sources[0].Snippet != "snippet A" {
		t.Fatalf("unexpected first source: %+v", sources[0])
	}
	if sources[1].URL != "https://example.com/b" || sources[1].Title != "Example B" {
		t.Fatalf("unexpected second source: %+v", sources[1])
	}
	if len(r.GroundingSources) != 2 {
		t.Fatalf("expected line-level grounding sources, got %d", len(r.GroundingSources))
	}
}

func TestGroundingStripsTextFragmentAndDeduplicates(t *testing.T) {
	line := groundingFrameLine(t, groundingCandidate("rc_1", "answer", [][]any{
		{"https://example.com/a#:~:text=hello", "Example A", "favicon", "snippet"},
		{"https://example.com/a", "Example A duplicate", "favicon", "snippet"},
	}))

	r, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	sources := r.GroundingSources
	if len(sources) != 1 {
		t.Fatalf("expected duplicate URL to collapse to 1 source, got %d: %+v", len(sources), sources)
	}
	if sources[0].URL != "https://example.com/a" {
		t.Fatalf("expected text fragment to be stripped, got %q", sources[0].URL)
	}
}

func TestFrameWithoutGroundingHasNoSources(t *testing.T) {
	candidate := make([]any, 9)
	candidate[0] = "rc_9"
	candidate[1] = []any{"plain answer"}
	candidate[8] = []any{float64(2)}

	line := groundingFrameLine(t, candidate)
	r, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(r.GroundingSources) != 0 {
		t.Fatalf("expected no grounding sources, got %+v", r.GroundingSources)
	}
	if len(r.Frames) != 1 || len(r.Frames[0].GroundingSources) != 0 {
		t.Fatalf("expected no frame-level grounding sources, got %+v", r.Frames)
	}
}
