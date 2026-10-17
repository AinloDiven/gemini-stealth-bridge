package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/server"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var reSafeAccountName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func main() {
	var (
		loginAcct        = flag.String("login", "", "Interactive browser sign-in to capture a fresh session into ~/.gemini-stealth-bridge/accounts/<name>.json")
		importChrome     = flag.String("import-chrome", "", "Import Google cookies from local Chrome/Brave/Chromium/Edge/Firefox SQLite DB into accounts/<name>.json")
		cookieDB         = flag.String("cookie-db", "", "Explicit path to browser Cookies SQLite database or profile directory for --import-chrome / --login")
		chromeProfile    = flag.String("chrome-profile", "", "Browser profile name (e.g. \"Profile 4\"), profile directory, or Cookies SQLite path for --import-chrome / --login")
		importCookie     = flag.String("import-cookie", "", "Import raw Cookie header or Cookie-Editor JSON from --cookie-value, --cookie-file, or stdin into accounts/<name>.json")
		cookieValue      = flag.String("cookie-value", "", "Raw Cookie header or JSON string for --import-cookie")
		cookieFile       = flag.String("cookie-file", "", "Path to file containing raw Cookie header or Cookie-Editor JSON array for --import-cookie")
		probeOnce        = flag.Bool("probe", false, "Bootstrap all configured accounts, verify models & quota, and exit")
		rotateOnce       = flag.Bool("rotate", false, "Run dual-path cookie rotation on all configured accounts and exit")
		showVersion      = flag.Bool("version", false, "Print build version and exit")
		checkConfig      = flag.Bool("check-config", false, "Validate environment configuration and exit")
		modelsJSON       = flag.Bool("models-json", false, "Print canonical /v1/models JSON payload and exit")
		unquarantineAcct = flag.String("unquarantine", "", "Clear quarantine state for account <id> and exit")
		captureRef       = flag.Bool("capture-reference", false, "Capture reference headers from local Chrome login profile via CDP or dump instructions")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("gsb %s (%s/%s, %s)\n", config.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	cfg := config.Load()
	for _, w := range cfg.Warnings {
		log.Printf("[config] WARNING: %s", w)
	}

	if *checkConfig {
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "config validation failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("config OK")
		return
	}

	if *unquarantineAcct != "" {
		if err := runUnquarantine(cfg, *unquarantineAcct); err != nil {
			log.Fatalf("[unquarantine] failed: %v", err)
		}
		return
	}

	dbOrProfile := *cookieDB
	if dbOrProfile == "" {
		dbOrProfile = *chromeProfile
	}

	if *captureRef {
		if err := runCaptureReference(cfg, dbOrProfile); err != nil {
			log.Fatalf("[capture-reference] failed: %v", err)
		}
		return
	}

	if *modelsJSON {
		runModelsJSON(cfg)
		return
	}

	if *loginAcct != "" {
		if err := runInteractiveLogin(cfg, *loginAcct, dbOrProfile); err != nil {
			log.Fatalf("[login] failed: %v", err)
		}
		return
	}
	if *importChrome != "" {
		if err := runImportChrome(cfg, *importChrome, dbOrProfile); err != nil {
			log.Fatalf("[import-chrome] failed: %v", err)
		}
		return
	}
	if *importCookie != "" {
		if err := runImportCookie(cfg, *importCookie, *cookieValue, *cookieFile); err != nil {
			log.Fatalf("[import-cookie] failed: %v", err)
		}
		return
	}

	lock, lockErr := config.AcquireLock(cfg.HomeDir)
	if lockErr != nil {
		log.Fatalf("[lock] %v", lockErr)
	}
	defer func() { _ = lock.Release() }()

	srv := server.New(cfg)
	loaded := srv.LoadCredentials()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *probeOnce {
		if loaded == 0 {
			log.Fatalf("[probe] no accounts found in %s", cfg.AccountsDir)
		}
		accts := srv.EnsureModels(ctx, true)
		for _, a := range accts {
			if a.ModelsError != nil {
				fmt.Printf("Account %-16s email=%-28s ERROR: %v\n", a.ID, a.Email, a.ModelsError)
				var ue *transport.UpstreamError
				if errors.As(a.ModelsError, &ue) && ue != nil && ue.Code == "account_banned" {
					if ac, ok := a.Client.(*gemini.AccountClient); ok {
						ac.SetQuarantined(true, ue.Error())
					}
				}
			} else {
				fmt.Printf("Account %-16s email=%-28s capacity=%d models=%d fails=%d\n",
					a.ID, a.Email, a.Capacity, len(a.Models), a.Fails)
				if ac, ok := a.Client.(*gemini.AccountClient); ok {
					ac.SetSurfaceStatus("geminiWeb", "ok")
				}
			}
			for _, m := range a.Models {
				fmt.Printf("  - %-24s (%s, id=%s, cap=%d)\n", m.ModelName, m.DisplayName, m.ModelID, m.Capacity)
			}
			for _, q := range a.Quota {
				if q.Unlimited {
					fmt.Printf("  quota: %-18s unlimited\n", q.Label)
				} else {
					fmt.Printf("  quota: %-18s remaining=%d/%d resets=%s\n", q.Label, q.Remaining, q.Total, q.ResetAt)
				}
			}
		}
		return
	}

	if *rotateOnce {
		if loaded == 0 {
			log.Fatalf("[rotate] no accounts found in %s", cfg.AccountsDir)
		}
		for _, a := range srv.Accounts() {
			res, err := a.Client.RotateCookies(ctx)
			if err != nil {
				fmt.Printf("Account %-16s error: %v\n", a.ID, err)
			} else {
				fmt.Printf("Account %-16s HTTP %d rotated=%v next=%s\n", a.ID, res.Status, res.Rotated, res.NextInterval)
			}
		}
		return
	}

	srv.StartBackgroundWorkers(ctx)

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		if !config.IsLoopbackHost(cfg.Host) && cfg.APIKey == "public" && len(cfg.AccountTokens) == 0 {
			log.Printf("  WARNING:   binding to non-loopback host %s with default BRIDGE_API_KEY=public; set BRIDGE_API_KEY or BRIDGE_ACCOUNT_TOKENS", cfg.Host)
		}
		log.Printf("gemini-stealth-bridge (gsb) listening on http://%s", addr)
		log.Printf("  transport: browserless Chrome uTLS + HTTP/2 (%s)", cfg.ImpersonateProfile)
		log.Printf("  data:      %s (%d account(s) loaded)", cfg.HomeDir, loaded)
		log.Printf("  models:    http://%s/v1/models", addr)
		if loaded == 0 {
			log.Printf("  WARNING:   no credentials found — run `gsb --import-chrome <name>` or place accounts in %s", cfg.AccountsDir)
		}
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[fatal] listen %s: %v", addr, err)
		}
	}()

	<-ctx.Done()
	log.Printf("[shutdown] stopping gemini-stealth-bridge...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	srv.Pool.Reset()
}

func runImportChrome(cfg config.Config, acctName, explicitDB string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	var dbPath string
	if strings.TrimSpace(explicitDB) != "" {
		p, err := cookies.FindBrowserCookieDB(explicitDB)
		if err != nil {
			return err
		}
		dbPath = p
	} else {
		if cfg.CookieDB != "" {
			dbPath = cookies.FindChromeCookieDB(cfg.CookieDB)
		}
		if dbPath == "" {
			dbPath = cookies.FindChromeCookieDB("")
		}
		if dbPath == "" {
			return fmt.Errorf("could not locate Chrome/Brave/Chromium Cookies SQLite database")
		}
	}
	if strings.TrimSpace(explicitDB) == "" && strings.TrimSpace(cfg.CookieDB) == "" {
		fmt.Printf("Note: No --chrome-profile or --cookie-db specified; auto-selected %s\n", dbPath)
		fmt.Println("      Pass --chrome-profile \"<Profile Name>\" to target a dedicated browser profile.")
	}
	if lockPath := cookies.DetectActiveProfileLock(dbPath); lockPath != "" {
		fmt.Printf("Note: Browser profile lock detected (%s); concurrent browser and bridge rotation on the same session can cause __Secure-1PSIDTS conflicts.\n", filepath.Base(lockPath))
	}
	hdr, err := cookies.ReadChromeCookies(dbPath)
	if err != nil {
		return err
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		return fmt.Errorf("no SAPISID cookie found in %s — sign in to gemini.google.com first", dbPath)
	}
	outPath := filepath.Join(cfg.AccountsDir, acctName+".json")
	srv := server.New(cfg)
	ac := gemini.NewAccountClient(acctName, hdr, outPath, srv.Pool, srv.Rotator)
	ac.SetProvenance(dbPath, explicitDB)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap with imported cookies failed: %w", err)
	}
	fmt.Printf("Imported account %q (%s) -> %s\n", acctName, ac.Email(), outPath)
	printStandaloneConnectionInfo(cfg, acctName)
	return nil
}

func runImportCookie(cfg config.Config, acctName, rawVal, filePath string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if rawVal == "" && filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		rawVal = string(b)
	}
	if rawVal == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		rawVal = string(b)
	}
	hdr, _, err := cookies.ParseImportPayload(rawVal)
	if err != nil {
		return err
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		return fmt.Errorf("cookie payload does not contain SAPISID")
	}
	outPath := filepath.Join(cfg.AccountsDir, acctName+".json")
	srv := server.New(cfg)
	ac := gemini.NewAccountClient(acctName, hdr, outPath, srv.Pool, srv.Rotator)
	ac.SetProvenance(filePath, "cookie-export")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap with imported cookies failed: %w", err)
	}
	fmt.Printf("Imported account %q (%s) -> %s\n", acctName, ac.Email(), outPath)
	printStandaloneConnectionInfo(cfg, acctName)
	return nil
}

func runInteractiveLogin(cfg config.Config, acctName, explicitDB string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if explicitDB == "" {
		explicitDB = cfg.CookieDB
	}
	targetURL := "https://gemini.google.com/app"
	browser, profileDir := cookies.ParseProfileHint(explicitDB)
	if explicitDB != "" {
		fmt.Printf("Opening %s in your browser (target profile: %s)...\n", targetURL, explicitDB)
	} else {
		fmt.Printf("Opening %s in your default browser...\n", targetURL)
		fmt.Println("Note: No --chrome-profile specified; auto-import reads from the Default profile.")
		fmt.Println("      On multi-profile setups, pass --chrome-profile \"<Profile Name or Path>\" (e.g. \"Profile 4\").")
	}
	openBrowserURL(targetURL, browser, profileDir)
	fmt.Println("1. Sign in to Google Gemini in your browser.")
	fmt.Println("2. Press Enter to auto-import cookies from your local browser profile,")
	fmt.Println("   or paste a raw Cookie header / Cookie-Editor JSON line and press Enter:")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "" {
		return runImportCookie(cfg, acctName, line, "")
	}
	return runImportChrome(cfg, acctName, explicitDB)
}

func findBrowserBinary(browser string) string {
	b := strings.ToLower(strings.TrimSpace(browser))
	if b == "" {
		b = "chrome"
	}
	switch runtime.GOOS {
	case "windows":
		roots := []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("LOCALAPPDATA"),
		}
		var rels []string
		switch b {
		case "brave":
			rels = []string{`BraveSoftware\Brave-Browser\Application\brave.exe`}
		case "edge":
			rels = []string{`Microsoft\Edge\Application\msedge.exe`}
		case "vivaldi":
			rels = []string{`Vivaldi\Application\vivaldi.exe`}
		case "chromium":
			rels = []string{`Chromium\Application\chrome.exe`}
		default:
			rels = []string{`Google\Chrome\Application\chrome.exe`}
		}
		for _, r := range roots {
			if r == "" {
				continue
			}
			for _, rel := range rels {
				cand := filepath.Join(r, rel)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() {
					return cand
				}
			}
		}
	default:
		var names []string
		switch b {
		case "brave":
			names = []string{"brave-browser", "brave"}
		case "edge":
			names = []string{"microsoft-edge", "microsoft-edge-stable"}
		case "vivaldi":
			names = []string{"vivaldi", "vivaldi-stable"}
		case "chromium":
			names = []string{"chromium", "chromium-browser"}
		default:
			names = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
		}
		for _, n := range names {
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
	}
	return ""
}

func openBrowserURL(url, browser, profileDir string) {
	if profileDir != "" {
		if runtime.GOOS == "darwin" {
			appName := "Google Chrome"
			switch strings.ToLower(browser) {
			case "brave":
				appName = "Brave Browser"
			case "edge":
				appName = "Microsoft Edge"
			case "vivaldi":
				appName = "Vivaldi"
			case "chromium":
				appName = "Chromium"
			}
			cmd := exec.Command("open", "-a", appName, "--args", "--profile-directory="+profileDir, url)
			if err := cmd.Start(); err == nil {
				return
			}
		} else if bin := findBrowserBinary(browser); bin != "" {
			cmd := exec.Command(bin, "--profile-directory="+profileDir, url)
			if err := cmd.Start(); err == nil {
				return
			}
		}
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func printStandaloneConnectionInfo(cfg config.Config, acctName string) {
	fmt.Printf("  base url: http://%s:%d/v1\n", cfg.Host, cfg.Port)
	fmt.Printf("  api key : gsb-%s (or %s)\n", acctName, acctName)
}

func runUnquarantine(cfg config.Config, acctID string) error {
	if !reSafeAccountName.MatchString(acctID) {
		return fmt.Errorf("invalid account id %q", acctID)
	}
	path := filepath.Join(cfg.AccountsDir, acctID+".json")
	rec, _, err := cookies.LoadAccountFile(path)
	if err != nil {
		return fmt.Errorf("load account %s: %w", path, err)
	}
	rec.Quarantined = false
	rec.QuarantineReason = ""
	if rec.Surfaces == nil {
		rec.Surfaces = make(map[string]string)
	}
	rec.Surfaces["geminiWeb"] = "ok"
	if _, err := cookies.SaveCredentialFile(path, *rec); err != nil {
		return fmt.Errorf("save account %s: %w", path, err)
	}
	fmt.Printf("Cleared quarantine state for account %q (%s)\n", acctID, path)
	return nil
}

func runModelsJSON(cfg config.Config) {
	srv := server.New(cfg)
	_ = srv.LoadCredentials()
	var models []gemini.ModelInfo
	for _, a := range srv.Accounts() {
		if len(a.Models) > 0 {
			models = append(models, a.Models...)
		}
	}
	if len(models) == 0 {
		models = []gemini.ModelInfo{
			{
				ModelID:     "2.5-pro-id",
				ModelName:   "gemini-pro",
				DisplayName: "2.5 Pro",
				Aliases:     []string{"gemini-2.5-pro"},
				Description: "Advanced reasoning and coding",
				Capacity:    2,
				ModelNumber: 1,
			},
			{
				ModelID:     "2.5-flash-id",
				ModelName:   "gemini-flash",
				DisplayName: "2.5 Flash",
				Aliases:     []string{"gemini-2.5-flash"},
				Description: "Fast everyday reasoning",
				Capacity:    2,
				ModelNumber: 2,
			},
			{
				ModelID:     "2.5-flash-lite-id",
				ModelName:   "gemini-flash-lite",
				DisplayName: "2.5 Flash Lite",
				Aliases:     []string{"gemini-2.5-flash-lite"},
				Description: "High-efficiency lightweight model",
				Capacity:    1,
				ModelNumber: 1,
			},
			{
				ModelID:     "2.0-flash-thinking-id",
				ModelName:   "gemini-thinking",
				DisplayName: "Flash Thinking",
				Aliases:     []string{"gemini-2.0-flash-thinking"},
				Description: "Extended step-by-step reasoning",
				Capacity:    2,
				ModelNumber: 3,
			},
			{
				ModelID:     "95a4c0abcba24364",
				ModelName:   "gemini-4-argon",
				DisplayName: "4 Argon",
				Aliases:     []string{"gemini-argon"},
				Description: "Experimental frontier reasoning model",
				Capacity:    2,
				ModelNumber: 4,
			},
		}
	}
	payload := srv.BuildModelsPayload(models)
	b, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(b))
}

func runCaptureReference(cfg config.Config, explicitProfile string) error {
	outPath := filepath.Join(cfg.HomeDir, "reference-fingerprint.json")
	if err := os.MkdirAll(cfg.HomeDir, 0o700); err != nil {
		return err
	}
	ref := transport.RequestDump{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Method:    "POST",
		URL:       "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
		Kind:      "cors",
		HeaderOrder: []string{
			"sec-ch-ua",
			"x-same-domain",
			"sec-ch-ua-mobile",
			"sec-ch-ua-full-version-list",
			"sec-ch-ua-arch",
			"sec-ch-ua-platform-version",
			"sec-ch-ua-model",
			"sec-ch-ua-bitness",
			"sec-ch-ua-wow64",
			"user-agent",
			"content-type",
			"content-length",
			"x-goog-ext-525001261-jspb",
			"x-goog-ext-525005358-jspb",
			"x-goog-ext-73010989-jspb",
			"x-goog-ext-73010990-jspb",
			"sec-ch-ua-platform",
			"accept",
			"origin",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
			"cookie",
			"priority",
		},
		PseudoOrder: []string{":method", ":authority", ":scheme", ":path"},
		Headers: map[string][]string{
			"accept-encoding": {"gzip, deflate, br, zstd"},
			"sec-ch-ua":       {cfg.SecCHUA},
		},
		TLSExtOrder: "fixed",
	}
	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote Chrome reference fingerprint (%s) -> %s\n", explicitProfile, outPath)
	fmt.Printf("Compare against a live dump with:\n  BRIDGE_DUMP_REQUEST=/tmp/gsb-dumps ./gsb --probe\n  ./scripts/diff-fingerprint.sh /tmp/gsb-dumps/req-*.json %s\n", outPath)
	return nil
}
