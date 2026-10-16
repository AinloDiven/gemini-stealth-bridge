package config

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime settings for the gemini-stealth-bridge daemon.
// Environment variables are 100% compatible with gemini-app-bridge.
type Config struct {
	Host               string
	Port               int
	HomeDir            string
	CredFile           string
	AccountsDir        string
	CookieDB           string
	APIKey             string
	AccountTokens      map[string]string
	Temporary          bool
	RejectDowngrade    bool
	LiveThoughts       bool
	SessionCache       bool
	KeepToolPreamble   bool
	MaxPromptChars     int
	MaxOldToolChars    int
	RotateCookiesEvery time.Duration
	ModelRefreshEvery  time.Duration
	StreamTimeout      time.Duration
	FirstByteTimeout   time.Duration
	StallTimeout       time.Duration
	IdleSessionTimeout time.Duration
	MaxSessions        int
	MaxQueueDepth      int
	SessionWaitTimeout time.Duration
	ProxyURL           string
	ImpersonateProfile string
	DefaultThinking    int
	UserAgent          string
	SecCHUA            string
	SecCHUAPlatform    string
}

// ThinkingLevels maps Gemini Web's 3 thinking levels ("low", "medium", "high") to
// Google's Assistant_Lamda_ThinkingLevel protobuf enum (otAQ7b field 24):
//
//	1 = THINKING_LEVEL_STANDARD ("low", default)
//	4 = THINKING_LEVEL_MEDIUM   ("medium")
//	2 = THINKING_LEVEL_EXTENDED ("high")
var ThinkingLevels = map[string]int{
	"low":    1,
	"medium": 4,
	"high":   2,
}

// FromEnv constructs a Config from process environment variables.
func FromEnv() *Config {
	home, _ := os.UserHomeDir()
	bridgeHome := envStr("BRIDGE_HOME", filepath.Join(home, ".gemini-stealth-bridge"))
	credFile := envStr("WEB_CRED_FILE", filepath.Join(bridgeHome, "web-credentials.json"))
	accountsDir := filepath.Join(bridgeHome, "accounts")

	tokens := make(map[string]string)
	for _, pair := range strings.FieldsFunc(os.Getenv("BRIDGE_ACCOUNT_TOKENS"), func(r rune) bool { return r == ',' || r == ';' }) {
		if idx := strings.IndexByte(pair, ':'); idx > 0 {
			tok := strings.TrimSpace(pair[:idx])
			acc := strings.TrimSpace(pair[idx+1:])
			if tok != "" && acc != "" {
				tokens[tok] = acc
			}
		}
	}

	defThinkKey := strings.ToLower(strings.TrimSpace(envStr("BRIDGE_DEFAULT_THINKING", "high")))
	defThink, ok := ThinkingLevels[defThinkKey]
	if !ok {
		defThink = 2
	}

	impersonate := envStr("BRIDGE_IMPERSONATE", "chrome_150_psk")
	ua, chUA, platform := browserHeadersForOS(runtime.GOOS, impersonate)

	return &Config{
		Host:               envStr("HOST", "127.0.0.1"),
		Port:               envInt("PORT", 20133),
		HomeDir:            bridgeHome,
		CredFile:           credFile,
		AccountsDir:        accountsDir,
		CookieDB:           strings.TrimSpace(os.Getenv("CHROME_COOKIES")),
		APIKey:             envStr("BRIDGE_API_KEY", "public"),
		AccountTokens:      tokens,
		Temporary:          envBool("BRIDGE_TEMPORARY", false),
		RejectDowngrade:    envBool("BRIDGE_REJECT_DOWNGRADE", true),
		LiveThoughts:       envBool("BRIDGE_LIVE_THOUGHTS", true),
		SessionCache:       envBool("BRIDGE_SESSION_CACHE", true),
		KeepToolPreamble:   envBool("BRIDGE_TOOL_PREAMBLE", false),
		MaxPromptChars:     envInt("BRIDGE_MAX_PROMPT_CHARS", 850000),
		MaxOldToolChars:    envInt("BRIDGE_MAX_OLD_TOOL_CHARS", 32000),
		RotateCookiesEvery: time.Duration(envInt("BRIDGE_ROTATE_COOKIES_MS", 540000)) * time.Millisecond,
		ModelRefreshEvery:  time.Duration(envInt("BRIDGE_MODEL_REFRESH_MS", 600000)) * time.Millisecond,
		StreamTimeout:      time.Duration(envInt("BRIDGE_STREAM_TIMEOUT_MS", 600000)) * time.Millisecond,
		FirstByteTimeout:   time.Duration(envInt("BRIDGE_FIRST_BYTE_TIMEOUT_MS", 60000)) * time.Millisecond,
		StallTimeout:       time.Duration(envInt("BRIDGE_STALL_TIMEOUT_MS", 90000)) * time.Millisecond,
		IdleSessionTimeout: time.Duration(envInt("BRIDGE_IDLE_SESSION_MS", 180000)) * time.Millisecond,
		MaxSessions:        envInt("BRIDGE_MAX_SESSIONS", 16),
		MaxQueueDepth:      envInt("BRIDGE_MAX_QUEUE", 4),
		SessionWaitTimeout: time.Duration(envInt("BRIDGE_SESSION_WAIT_MS", 10000)) * time.Millisecond,
		ProxyURL:           strings.TrimSpace(os.Getenv("BRIDGE_PROXY_SERVER")),
		ImpersonateProfile: impersonate,
		DefaultThinking:    defThink,
		UserAgent:          ua,
		SecCHUA:            chUA,
		SecCHUAPlatform:    platform,
	}
}

// Load returns a value copy of FromEnv().
func Load() Config {
	return *FromEnv()
}

// IsLoopbackHost reports whether host is a loopback address.
func IsLoopbackHost(h string) bool {
	if h == "localhost" || h == "::1" || h == "::ffff:127.0.0.1" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// browserHeadersForOS aligns User-Agent and Sec-CH-UA* headers with both the
// host kernel's TCP stack (avoiding OS/JA4T cross-layer contradiction) and the
// active Chrome uTLS profile version.
func browserHeadersForOS(goos, profile string) (ua, secCHUA, platform string) {
	ver := "150"
	p := strings.ToLower(profile)
	switch {
	case strings.Contains(p, "152"):
		ver = "152"
	case strings.Contains(p, "150"):
		ver = "150"
	case strings.Contains(p, "146"):
		ver = "146"
	case strings.Contains(p, "144"):
		ver = "144"
	case strings.Contains(p, "133"):
		ver = "133"
	case strings.Contains(p, "131"):
		ver = "131"
	}

	secCHUA = `"Google Chrome";v="` + ver + `", "Chromium";v="` + ver + `", "Not?A_Brand";v="24"`
	switch goos {
	case "darwin":
		platform = `"macOS"`
		ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	case "windows":
		platform = `"Windows"`
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	default:
		platform = `"Linux"`
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	}
	return ua, secCHUA, platform
}

func envStr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
