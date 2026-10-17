//go:build windows

package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// LockHandle represents an active single-writer lock on a BRIDGE_HOME directory.
type LockHandle struct {
	path string
	file *os.File
}

// AcquireLock acquires a non-blocking exclusive lock on <homeDir>/.lock.
func AcquireLock(homeDir string) (*LockHandle, error) {
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create bridge home %s: %w", homeDir, err)
	}
	lockPath := filepath.Join(homeDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	var ol windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&ol,
	)
	if err != nil {
		_, _ = f.Seek(0, io.SeekStart)
		b, _ := io.ReadAll(f)
		_ = f.Close()
		ownerInfo := strings.TrimSpace(strings.ReplaceAll(string(b), "\n", ", "))
		if ownerInfo == "" {
			ownerInfo = "unknown owner"
		}
		return nil, fmt.Errorf("bridge home %s is already locked by another gsb process (%s)", homeDir, ownerInfo)
	}

	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, io.SeekStart)
		_, _ = fmt.Fprintf(f, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
		_ = f.Sync()
	}

	return &LockHandle{
		path: lockPath,
		file: f,
	}, nil
}

// Release unlocks and closes the lock file handle.
func (h *LockHandle) Release() error {
	if h == nil || h.file == nil {
		return nil
	}
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(h.file.Fd()), 0, 1, 0, &ol)
	err := h.file.Close()
	h.file = nil
	return err
}
