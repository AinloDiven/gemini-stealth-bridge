//go:build nowebui

package webui

// PageHTML returns nil, false when compiled with -tags nowebui.
func PageHTML() ([]byte, bool) {
	return nil, false
}
