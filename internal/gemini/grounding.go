package gemini

import "strings"

// GroundingSource is one web-search citation reported by Gemini Web's Google
// Search grounding extension.
type GroundingSource struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// extractGroundingSources pulls the Google Search citations out of one
// candidate slice.
//
// Gemini Web carries them at inner[4][0][2][1]: the candidate's slot 2 holds
// an array whose second element is the list of grounding chunks, and each chunk
// holds the source list in slot 2 as [url, title, favicon, snippet]. The slot
// layout is identical whether the payload is dense or sparse (JSPB), so the
// candidate lookup goes through jspbField.
//
// A candidate without search grounding has no such slot, so the result is
// empty rather than an error.
func extractGroundingSources(candidate []any) []GroundingSource {
	slot := jspbField(candidate, 2)
	holder, ok := slot.([]any)
	if !ok || len(holder) < 2 {
		return nil
	}
	chunks, ok := holder[1].([]any)
	if !ok || len(chunks) == 0 {
		return nil
	}
	var out []GroundingSource
	for _, chunkAny := range chunks {
		chunk, ok := chunkAny.([]any)
		if !ok || len(chunk) < 3 {
			continue
		}
		sources, ok := chunk[2].([]any)
		if !ok || len(sources) == 0 {
			continue
		}
		for _, srcAny := range sources {
			src, ok := srcAny.([]any)
			if !ok || len(src) < 2 {
				continue
			}
			url, _ := src[0].(string)
			url = stripTextFragment(url)
			if url == "" {
				continue
			}
			title, _ := src[1].(string)
			snippet := ""
			if len(src) > 3 {
				snippet, _ = src[3].(string)
			}
			out = append(out, GroundingSource{URL: url, Title: title, Snippet: snippet})
		}
	}
	return out
}

// stripTextFragment removes Google's "#:~:text=" scroll anchor from a URL so
// the citation points at the page instead of a text fragment.
func stripTextFragment(u string) string {
	if i := strings.Index(u, "#:~:text="); i >= 0 {
		return u[:i]
	}
	return u
}

// MergeGroundingSources appends sources that are not already present, keyed by
// URL, so the same citation is not repeated across stream frames.
func MergeGroundingSources(dst []GroundingSource, src []GroundingSource) []GroundingSource {
	for _, s := range src {
		dup := false
		for _, existing := range dst {
			if existing.URL == s.URL {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		dst = append(dst, s)
	}
	return dst
}
