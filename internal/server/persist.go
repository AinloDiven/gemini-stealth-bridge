package server

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// writeJSONAtomically replaces path with the JSON encoding of value. Both
// extension caches use the same replace semantics so a crash cannot leave a
// half-written cache or binding file behind.
func writeJSONAtomically(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
