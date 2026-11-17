package gemini

import (
	"regexp"
	"strconv"
	"strings"
)

var citeMarkerIDRe = regexp.MustCompile(`\d+`)

// GroundingSource is one citation reported by Gemini Web, either from the Google
// Search grounding slot (candidate[2]) or from inline attachment/file citation
// metadata (candidate[12] field 44).
type GroundingSource struct {
	ID       int    `json:"id,omitempty"`
	URL      string `json:"url,omitempty"`
	Title    string `json:"title,omitempty"`
	Snippet  string `json:"snippet,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// extractGroundingSources pulls Google Search grounding sources (candidate[2])
// and inline file/media citation mappings (candidate[12] field 44) out of one
// candidate slice.
func extractGroundingSources(candidate []any) []GroundingSource {
	var out []GroundingSource
	if holder, ok := jspbField(candidate, 2).([]any); ok && len(holder) >= 2 {
		if chunks, ok := holder[1].([]any); ok {
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
		}
	}
	return MergeGroundingSources(out, extractFileCitations(candidate))
}

// extractFileCitations reads candidate[12] field 44 (1-based JSPB key "44" ->
// 0-based index 43), where Gemini Web binds "[cite: N]" markers to uploaded
// file names and MIME types:
// [ ["[cite: 9]", occIdx], [ [null, [ [..., kind, fname, ..., mime], ..., title ]], ... ], ... ].
func extractFileCitations(candidate []any) []GroundingSource {
	c12 := jspbField(candidate, 12)
	if c12 == nil {
		return nil
	}
	raw, ok := jspbSubField(c12, 43).([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	var out []GroundingSource
	for _, entryAny := range raw {
		entry, ok := entryAny.([]any)
		if !ok || len(entry) < 2 {
			continue
		}
		markerWrap, ok := entry[0].([]any)
		if !ok || len(markerWrap) == 0 {
			continue
		}
		marker, _ := markerWrap[0].(string)
		ids := parseCiteIDs(marker)
		if len(ids) == 0 {
			continue
		}
		sources, ok := entry[1].([]any)
		if !ok || len(sources) == 0 {
			continue
		}
		for idx, srcAny := range sources {
			src, ok := srcAny.([]any)
			if !ok {
				continue
			}
			holder, ok := jspbField(src, 1).([]any)
			if !ok {
				continue
			}
			title, _ := jspbField(holder, 6).(string)
			var mimeType string
			if meta, ok := jspbField(holder, 0).([]any); ok {
				if strings.TrimSpace(title) == "" {
					title, _ = jspbField(meta, 2).(string)
				}
				mimeType, _ = jspbField(meta, 11).(string)
			}
			title = strings.TrimSpace(title)
			mimeType = strings.TrimSpace(mimeType)
			if title == "" {
				continue
			}
			if len(sources) == len(ids) {
				out = append(out, GroundingSource{ID: ids[idx], Title: title, MimeType: mimeType})
				continue
			}
			for _, id := range ids {
				out = append(out, GroundingSource{ID: id, Title: title, MimeType: mimeType})
			}
		}
	}
	return out
}

func parseCiteIDs(marker string) []int {
	if marker == "" || !strings.Contains(strings.ToLower(marker), "cite") {
		return nil
	}
	rawIDs := citeMarkerIDRe.FindAllString(marker, -1)
	if len(rawIDs) == 0 {
		return nil
	}
	ids := make([]int, 0, len(rawIDs))
	seen := make(map[int]bool, len(rawIDs))
	for _, raw := range rawIDs {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
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
// URL (or ID+Title for non-URL file citations), so citations do not repeat
// across stream frames.
func MergeGroundingSources(dst []GroundingSource, src []GroundingSource) []GroundingSource {
	for _, s := range src {
		dup := false
		for i, existing := range dst {
			if s.URL != "" && existing.URL == s.URL {
				if existing.ID == 0 && s.ID > 0 {
					dst[i].ID = s.ID
				}
				dup = true
				break
			}
			if s.URL == "" && existing.URL == "" && existing.ID == s.ID && existing.Title == s.Title {
				if existing.MimeType == "" && s.MimeType != "" {
					dst[i].MimeType = s.MimeType
				}
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

// GroundingCitationsWithIDs assigns 1-based fallback IDs to web grounding
// sources whose ID was not explicitly bound by file citations.
func GroundingCitationsWithIDs(sources []GroundingSource) []GroundingSource {
	if len(sources) == 0 {
		return nil
	}
	claimed := make(map[int]bool, len(sources))
	for _, s := range sources {
		if s.ID > 0 && s.URL == "" {
			claimed[s.ID] = true
		}
	}
	out := make([]GroundingSource, len(sources))
	copy(out, sources)
	webIdx := 0
	for i := range out {
		if out[i].URL != "" && out[i].ID == 0 {
			for {
				webIdx++
				if !claimed[webIdx] {
					break
				}
			}
			claimed[webIdx] = true
			out[i].ID = webIdx
		}
	}
	return out
}

