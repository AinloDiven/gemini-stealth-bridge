package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
)

func TestSingleWriterLock(t *testing.T) {
	dir := t.TempDir()

	lock1, err := config.AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}
	defer func() { _ = lock1.Release() }()

	lock2, err2 := config.AcquireLock(dir)
	if err2 == nil {
		_ = lock2.Release()
		t.Fatal("expected second AcquireLock on same BRIDGE_HOME to fail immediately")
	}
	if !strings.Contains(err2.Error(), "already locked") || !strings.Contains(err2.Error(), "pid=") {
		t.Fatalf("expected error to mention already locked and pid=, got: %v", err2)
	}

	if err := lock1.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	lock3, err3 := config.AcquireLock(dir)
	if err3 != nil {
		t.Fatalf("expected AcquireLock after Release to succeed, got: %v", err3)
	}
	_ = lock3.Release()
}

func TestRunUnquarantineClearsStateAndPreservesOtherSurfaces(t *testing.T) {
	home := t.TempDir()
	acctsDir := filepath.Join(home, "accounts")
	cfg := config.Config{
		HomeDir:     home,
		AccountsDir: acctsDir,
	}
	acctPath := filepath.Join(acctsDir, "acct1.json")
	_, err := cookies.SaveCredentialFile(acctPath, cookies.CredentialFile{
		Name:             "acct1",
		Email:            "acct1@example.com",
		Cookies:          "SAPISID=123",
		Quarantined:      true,
		QuarantineReason: "TOS_VIOLATION",
		Surfaces: map[string]string{
			"geminiWeb":   "blocked",
			"antigravity": "ok",
		},
	})
	if err != nil {
		t.Fatalf("SaveCredentialFile: %v", err)
	}

	if err := runUnquarantine(cfg, "acct1"); err != nil {
		t.Fatalf("runUnquarantine failed: %v", err)
	}

	loaded, _, err := cookies.LoadAccountFile(acctPath)
	if err != nil {
		t.Fatalf("LoadAccountFile: %v", err)
	}
	if loaded.Quarantined || loaded.QuarantineReason != "" {
		t.Fatalf("expected Quarantined cleared, got %+v", loaded)
	}
	if loaded.Surfaces["geminiWeb"] != "ok" || loaded.Surfaces["antigravity"] != "ok" {
		t.Fatalf("expected geminiWeb=ok and antigravity=ok preserved, got %+v", loaded.Surfaces)
	}
}
