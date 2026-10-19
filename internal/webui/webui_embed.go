//go:build !nowebui

package webui

import _ "embed"

//go:embed index.html
var indexHTML []byte

// PageHTML returns the embedded control-plane HTML and true when compiled without -tags nowebui.
func PageHTML() ([]byte, bool) {
	return indexHTML, len(indexHTML) > 0
}
