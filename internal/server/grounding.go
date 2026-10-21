package server

import (
	"fmt"
	"strings"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// appendGroundingCitations renders the web-search citations that Gemini Web
// attached to an answer and appends them to the text.
//
// The citations are a plain numbered list at the end of the answer: clients
// that render markdown show them as links, and clients that only read plain
// text still get the source URLs.
func appendGroundingCitations(text string, sources []gemini.GroundingSource) string {
	if len(sources) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, " \t\r\n"))
	b.WriteString("\n\n---\nSources:\n")
	for i, s := range sources {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = s.URL
		}
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, title, s.URL)
	}
	return b.String()
}
