package server

import (
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

func payloadFor(t *testing.T, cfg config.Config, model gemini.ModelInfo) map[string]any {
	t.Helper()
	s := &Server{Cfg: cfg}
	payload := s.BuildModelsPayload([]gemini.ModelInfo{model})
	data, ok := payload["data"].([]map[string]any)
	if !ok || len(data) != 1 {
		t.Fatalf("unexpected payload shape: %#v", payload)
	}
	return data[0]
}

func capsOf(t *testing.T, entry map[string]any) map[string]any {
	t.Helper()
	caps, ok := entry["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("missing capabilities: %#v", entry)
	}
	return caps
}

// The advertised default must equal what the daemon actually applies to a
// request that carries no reasoning_effort.
func TestModelsPayloadAdvertisesConfiguredDefault(t *testing.T) {
	for _, level := range []int{1, 4, 2} {
		want := ThinkingLevelName(level)
		if want == "" {
			t.Fatalf("no name for level %d", level)
		}
		cfg := config.Config{DefaultThinking: level}
		entry := payloadFor(t, cfg, gemini.ModelInfo{
			ModelName:      "gemini-argon",
			DisplayName:    "4 Argon",
			ThinkingLevels: []string{"low", "medium", "high"},
		})
		thinking := entry["thinking"].(map[string]any)
		if got := thinking["default"]; got != want {
			t.Fatalf("thinking.default = %v, want %v (level %d)", got, want, level)
		}
		if got := capsOf(t, entry)["defaultThinkingLevel"]; got != want {
			t.Fatalf("capabilities.defaultThinkingLevel = %v, want %v", got, want)
		}
	}
}

// A configured default the model does not support must not be advertised for it.
func TestModelsPayloadIgnoresUnsupportedConfiguredDefault(t *testing.T) {
	cfg := config.Config{DefaultThinking: 4} // medium
	entry := payloadFor(t, cfg, gemini.ModelInfo{
		ModelName:      "gemini-argon",
		DisplayName:    "4 Argon",
		ThinkingLevels: []string{"low", "high"},
	})
	thinking := entry["thinking"].(map[string]any)
	if got := thinking["default"]; got == "medium" {
		t.Fatal("advertised a default the model does not support")
	}
}

// Each model advertises its own registry levels, not one global list.
func TestModelsPayloadAdvertisesRegistryLevels(t *testing.T) {
	cfg := config.Config{DefaultThinking: 2}
	entry := payloadFor(t, cfg, gemini.ModelInfo{
		ModelName:      "gemini-flash-lite",
		DisplayName:    "3.5 Flash-Lite",
		ThinkingLevels: []string{"low", "high"},
	})
	caps := capsOf(t, entry)
	levels, _ := caps["thinkingLevels"].([]string)
	if len(levels) != 2 || levels[0] != "low" || levels[1] != "high" {
		t.Fatalf("thinkingLevels = %v, want the registry list", levels)
	}
	if caps["reasoning"] != true {
		t.Fatal("model with registry levels must be reasoning")
	}
}

func TestModelsPayloadUsesDocumentedLimits(t *testing.T) {
	cfg := config.Config{ContextWindow: 200000, MaxOutputTokens: 8192}
	entry := payloadFor(t, cfg, gemini.ModelInfo{ModelName: "gemini-argon", DisplayName: "4 Argon"})
	if got := entry["context_length"]; got != 1048576 {
		t.Fatalf("context_length = %v, want the documented 1048576", got)
	}
	if got := entry["max_completion_tokens"]; got != 65536 {
		t.Fatalf("max_completion_tokens = %v, want the documented 65536", got)
	}
	if got := capsOf(t, entry)["limitsSource"]; got != "documented" {
		t.Fatalf("limitsSource = %v, want documented", got)
	}
}

func TestModelsPayloadFallsBackToConfiguredLimits(t *testing.T) {
	cfg := config.Config{ContextWindow: 200000, MaxOutputTokens: 8192}
	entry := payloadFor(t, cfg, gemini.ModelInfo{ModelName: "gemini-mystery", DisplayName: "9.9 Mystery"})
	if got := entry["context_length"]; got != 200000 {
		t.Fatalf("context_length = %v, want the configured 200000", got)
	}
	if got := capsOf(t, entry)["limitsSource"]; got != "config" {
		t.Fatalf("limitsSource = %v, want config", got)
	}
}
