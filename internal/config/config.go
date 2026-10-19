package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version is the build version of gemini-stealth-bridge (overridable via -ldflags).
var Version = "0.2.0"

// Config holds all runtime settings for the gemini-stealth-bridge daemon.
// Environment variables are 100% compatible with gemini-app-bridge.
type Config struct {
	Host                    string
	Port                    int
	HomeDir                 string
	CredFile                string
	AccountsDir             string
	CookieDB                string
	APIKey                  string
	AccountTokens           map[string]string
	Temporary               bool
	RejectDowngrade         bool
	LiveThoughts            bool
	SessionCache            bool
	KeepToolPreamble        bool
	LiveToolCalls           bool
	MaxPromptChars          int
	MaxOldToolChars         int
	RotateCookiesEvery      time.Duration
	ModelRefreshEvery       time.Duration
	StreamTimeout           time.Duration
	FirstByteTimeout        time.Duration
	StallTimeout            time.Duration
	IdleSessionTimeout      time.Duration
	MaxSessions             int
	MaxQueueDepth           int
	SessionWaitTimeout      time.Duration
	ProxyURL                string
	ImpersonateProfile      string
	DefaultThinking         int
	UserAgent               string
	SecCHUA                 string
	SecCHUAPlatform         string
	ContextWindow           int
	MaxOutputTokens         int
	TokenEstimator          string
	EarlySSE                bool
	EarlySSEGrace           time.Duration
	StreamBufferMode        string
	MaxAttempts             int
	MaxUpstreamAttempts     int
	RetryBudget             time.Duration
	ToolSchemaCompact       bool
	ToolDescMaxChars        int
	QuotaPreflight          string
	LogRequests             bool
	DumpRequestDir          string
	TLSExtOrder             string
	FollowRedirects         bool
	IPv6Mode                string
	UAOS                    string
	ProtocolMode            string
	BLOverride              string
	Locale                  string
	AcceptLanguage          string
	MaxRPMPerAccount        int
	MaxConcurrentPerAccount int
	QuietHours              string
	AccountGroups           map[string]string
	AccountStrategy         string
	AccountPriority         []string
	HistoryOffload          bool
	PersonalContext         string
	EgressPerAccount        map[string]string
	Warnings                []string
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
	var warnings []string
	home, _ := os.UserHomeDir()
	bridgeHome := envStr("BRIDGE_HOME", filepath.Join(home, ".gemini-stealth-bridge"))
	credFile := envStr("WEB_CRED_FILE", filepath.Join(bridgeHome, "web-credentials.json"))
	accountsDir := filepath.Join(bridgeHome, "accounts")

	tokens := parseKeyValMap(os.Getenv("BRIDGE_ACCOUNT_TOKENS"))
	accountGroups := parseKeyValMap(os.Getenv("BRIDGE_ACCOUNT_GROUP"))
	egressPerAccount := parseKeyValMap(os.Getenv("BRIDGE_EGRESS_PER_ACCOUNT"))

	rawThink := strings.TrimSpace(os.Getenv("BRIDGE_DEFAULT_THINKING"))
	defThinkKey := strings.ToLower(rawThink)
	if defThinkKey == "" {
		defThinkKey = "low"
	}
	defThink, ok := ThinkingLevels[defThinkKey]
	if !ok {
		warnings = append(warnings, fmt.Sprintf("invalid BRIDGE_DEFAULT_THINKING=%q; falling back to \"low\"", rawThink))
		defThink = 1
	}

	impersonate := envStr("BRIDGE_IMPERSONATE", "chrome_150_psk")
	rawUAOS := strings.TrimSpace(os.Getenv("BRIDGE_UA_OS"))
	targetOS := runtime.GOOS
	normalizedUAOS := ""
	if rawUAOS != "" {
		switch strings.ToLower(rawUAOS) {
		case "linux":
			targetOS = "linux"
			normalizedUAOS = "linux"
		case "windows", "win":
			targetOS = "windows"
			normalizedUAOS = "windows"
		case "macos", "darwin", "mac":
			targetOS = "darwin"
			normalizedUAOS = "macos"
		default:
			warnings = append(warnings, fmt.Sprintf("invalid BRIDGE_UA_OS=%q; falling back to host OS %q", rawUAOS, runtime.GOOS))
		}
	}
	ua, chUA, platform := browserHeadersForOS(targetOS, impersonate)

	tokenEstimator := envEnum("BRIDGE_TOKEN_ESTIMATOR", "script", []string{"script", "bytes4"}, &warnings)
	streamBuffer := envEnum("BRIDGE_STREAM_BUFFER", "verified", []string{"verified", "eager"}, &warnings)
	quotaPreflight := envEnum("BRIDGE_QUOTA_PREFLIGHT", "strict", []string{"strict", "advisory", "off"}, &warnings)
	tlsExtOrder := envEnum("BRIDGE_TLS_EXT_ORDER", "fixed", []string{"fixed", "random"}, &warnings)
	protocolMode := envEnum("BRIDGE_PROTOCOL_MODE", "bridge", []string{"bridge", "web"}, &warnings)
	personalContext := envEnum("BRIDGE_PERSONAL_CONTEXT", "auto", []string{"auto", "off", "on"}, &warnings)
	accountStrategy := envEnum("BRIDGE_ACCOUNT_STRATEGY", "rotate", []string{"rotate", "lru", "fill-first"}, &warnings)
	accountPriority := parseCSVList(os.Getenv("BRIDGE_ACCOUNT_PRIORITY"))
	if accountStrategy == "rotate" && len(accountPriority) > 0 {
		warnings = append(warnings, "BRIDGE_ACCOUNT_PRIORITY is ignored when BRIDGE_ACCOUNT_STRATEGY=rotate")
	}

	ipv6Default := "auto"
	if strings.TrimSpace(os.Getenv("BRIDGE_DISABLE_IPV6")) == "1" {
		ipv6Default = "off"
	}
	ipv6Mode := envEnum("BRIDGE_IPV6", ipv6Default, []string{"auto", "off", "on"}, &warnings)

	locale := envStr("BRIDGE_LOCALE", "en")
	acceptLang := deriveAcceptLanguage(locale)

	maxAttempts := envClampInt("BRIDGE_MAX_ATTEMPTS", 3, 1, 10, &warnings)
	maxUpstream := envClampInt("BRIDGE_MAX_UPSTREAM_ATTEMPTS", 3, 1, 10, &warnings)

	quietHours := strings.TrimSpace(os.Getenv("BRIDGE_QUIET_HOURS"))
	if quietHours != "" && !isValidQuietHours(quietHours) {
		warnings = append(warnings, fmt.Sprintf("invalid BRIDGE_QUIET_HOURS=%q; expected HH:MM-HH:MM", quietHours))
	}

	return &Config{
		Host:                    envStr("HOST", "127.0.0.1"),
		Port:                    envPositiveInt("PORT", 20133, 1, &warnings),
		HomeDir:                 bridgeHome,
		CredFile:                credFile,
		AccountsDir:             accountsDir,
		CookieDB:                strings.TrimSpace(os.Getenv("CHROME_COOKIES")),
		APIKey:                  envStr("BRIDGE_API_KEY", "public"),
		AccountTokens:           tokens,
		Temporary:               envBool("BRIDGE_TEMPORARY", false),
		RejectDowngrade:         envBool("BRIDGE_REJECT_DOWNGRADE", true),
		LiveThoughts:            envBool("BRIDGE_LIVE_THOUGHTS", true),
		SessionCache:            envBool("BRIDGE_SESSION_CACHE", true),
		KeepToolPreamble:        envBool("BRIDGE_TOOL_PREAMBLE", false),
		LiveToolCalls:           envBool("BRIDGE_LIVE_TOOL_CALLS", false),
		MaxPromptChars:          envPositiveInt("BRIDGE_MAX_PROMPT_CHARS", 850000, 1024, &warnings),
		MaxOldToolChars:         envPositiveInt("BRIDGE_MAX_OLD_TOOL_CHARS", 32000, 256, &warnings),
		RotateCookiesEvery:      time.Duration(envPositiveInt("BRIDGE_ROTATE_COOKIES_MS", 540000, 1000, &warnings)) * time.Millisecond,
		ModelRefreshEvery:       time.Duration(envPositiveInt("BRIDGE_MODEL_REFRESH_MS", 1800000, 1000, &warnings)) * time.Millisecond,
		StreamTimeout:           time.Duration(envPositiveInt("BRIDGE_STREAM_TIMEOUT_MS", 600000, 1000, &warnings)) * time.Millisecond,
		FirstByteTimeout:        time.Duration(envPositiveInt("BRIDGE_FIRST_BYTE_TIMEOUT_MS", 60000, 1000, &warnings)) * time.Millisecond,
		StallTimeout:            time.Duration(envPositiveInt("BRIDGE_STALL_TIMEOUT_MS", 90000, 1000, &warnings)) * time.Millisecond,
		IdleSessionTimeout:      time.Duration(envPositiveInt("BRIDGE_IDLE_SESSION_MS", 180000, 1000, &warnings)) * time.Millisecond,
		MaxSessions:             envPositiveInt("BRIDGE_MAX_SESSIONS", 16, 1, &warnings),
		MaxQueueDepth:           envPositiveInt("BRIDGE_MAX_QUEUE", 4, 1, &warnings),
		SessionWaitTimeout:      time.Duration(envPositiveInt("BRIDGE_SESSION_WAIT_MS", 10000, 100, &warnings)) * time.Millisecond,
		ProxyURL:                strings.TrimSpace(os.Getenv("BRIDGE_PROXY_SERVER")),
		ImpersonateProfile:      impersonate,
		DefaultThinking:         defThink,
		UserAgent:               ua,
		SecCHUA:                 chUA,
		SecCHUAPlatform:         platform,
		ContextWindow:           envPositiveInt("BRIDGE_CONTEXT_WINDOW", 1048576, 4096, &warnings),
		MaxOutputTokens:         envPositiveInt("BRIDGE_MAX_OUTPUT_TOKENS", 65536, 256, &warnings),
		TokenEstimator:          tokenEstimator,
		EarlySSE:                envBool("BRIDGE_EARLY_SSE", false),
		EarlySSEGrace:           time.Duration(envNonNegInt("BRIDGE_EARLY_SSE_GRACE_MS", 20000, &warnings)) * time.Millisecond,
		StreamBufferMode:        streamBuffer,
		MaxAttempts:             maxAttempts,
		MaxUpstreamAttempts:     maxUpstream,
		RetryBudget:             time.Duration(envNonNegInt("BRIDGE_RETRY_BUDGET_MS", 45000, &warnings)) * time.Millisecond,
		ToolSchemaCompact:       envBool("BRIDGE_TOOL_SCHEMA_COMPACT", true),
		ToolDescMaxChars:        envPositiveInt("BRIDGE_TOOL_DESC_MAX_CHARS", 400, 64, &warnings),
		QuotaPreflight:          quotaPreflight,
		LogRequests:             envBool("BRIDGE_LOG_REQUESTS", false),
		DumpRequestDir:          strings.TrimSpace(os.Getenv("BRIDGE_DUMP_REQUEST")),
		TLSExtOrder:             tlsExtOrder,
		FollowRedirects:         envBool("BRIDGE_FOLLOW_REDIRECTS", true),
		IPv6Mode:                ipv6Mode,
		UAOS:                    normalizedUAOS,
		ProtocolMode:            protocolMode,
		BLOverride:              strings.TrimSpace(os.Getenv("BRIDGE_BL_OVERRIDE")),
		Locale:                  locale,
		AcceptLanguage:          acceptLang,
		MaxRPMPerAccount:        envNonNegInt("BRIDGE_MAX_RPM_PER_ACCOUNT", 10, &warnings),
		MaxConcurrentPerAccount: envNonNegInt("BRIDGE_MAX_CONCURRENT_PER_ACCOUNT", 2, &warnings),
		HistoryOffload:          envBool("BRIDGE_HISTORY_OFFLOAD", true),
		PersonalContext:         personalContext,
		QuietHours:              quietHours,
		AccountGroups:           accountGroups,
		AccountStrategy:         accountStrategy,
		AccountPriority:         accountPriority,
		EgressPerAccount:        egressPerAccount,
		Warnings:                warnings,
	}
}

// Validate returns a combined error if any configuration warning or invalid format was recorded.
func (c Config) Validate() error {
	var errs []string
	errs = append(errs, c.Warnings...)
	if c.QuietHours != "" && !isValidQuietHours(c.QuietHours) {
		msg := fmt.Sprintf("invalid BRIDGE_QUIET_HOURS=%q; expected HH:MM-HH:MM", c.QuietHours)
		found := false
		for _, w := range errs {
			if w == msg {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
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

// parseCSVList splits a comma- or semicolon-separated list, trimming blanks and
// dropping duplicates while preserving the caller's order.
func parseCSVList(raw string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		v := strings.TrimSpace(item)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func parseKeyValMap(raw string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		if idx := strings.IndexByte(pair, ':'); idx > 0 {
			k := strings.TrimSpace(pair[:idx])
			v := strings.TrimSpace(pair[idx+1:])
			if k != "" && v != "" {
				out[k] = v
			}
		}
	}
	return out
}

func envPositiveInt(key string, fallback, minVal int, warnings *[]string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
		}
		return fallback
	}
	if n < minVal {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("%s=%d below minimum %d; clamping to %d", key, n, minVal, minVal))
		}
		return minVal
	}
	return n
}

func envNonNegInt(key string, fallback int, warnings *[]string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
		}
		return fallback
	}
	return n
}

func envClampInt(key string, fallback, minVal, maxVal int, warnings *[]string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %d", key, v, fallback))
		}
		return fallback
	}
	if n < minVal {
		return minVal
	}
	if n > maxVal {
		return maxVal
	}
	return n
}

func envEnum(key, fallback string, allowed []string, warnings *[]string) string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	low := strings.ToLower(raw)
	for _, a := range allowed {
		if low == a {
			return low
		}
	}
	if warnings != nil {
		*warnings = append(*warnings, fmt.Sprintf("invalid %s=%q; falling back to %q", key, raw, fallback))
	}
	return fallback
}

func deriveAcceptLanguage(locale string) string {
	loc := strings.TrimSpace(locale)
	if loc == "" || strings.EqualFold(loc, "en") || strings.EqualFold(loc, "en-us") {
		return "en-US,en;q=0.9"
	}
	if strings.Contains(loc, "-") {
		base := strings.SplitN(loc, "-", 2)[0]
		return fmt.Sprintf("%s,%s;q=0.9,en-US;q=0.8,en;q=0.7", loc, base)
	}
	return fmt.Sprintf("%s,en-US;q=0.9,en;q=0.8", loc)
}

func isValidQuietHours(s string) bool {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 2 {
		return false
	}
	return isValidHHMM(parts[0]) && isValidHHMM(parts[1])
}

func isValidHHMM(s string) bool {
	hm := strings.Split(strings.TrimSpace(s), ":")
	if len(hm) != 2 || len(hm[0]) != 2 || len(hm[1]) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return h >= 0 && h <= 23 && m >= 0 && m <= 59
}
