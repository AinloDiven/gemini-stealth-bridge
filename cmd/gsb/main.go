package main

import (
	"bufio"
	"context"
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
)

var reSafeAccountName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func main() {
	var (
		loginAcct     = flag.String("login", "", "Interactive browser sign-in to capture a fresh session into ~/.gemini-stealth-bridge/accounts/<name>.json")
		importChrome  = flag.String("import-chrome", "", "Import Google cookies from local Chrome/Brave/Chromium/Edge/Firefox SQLite DB into accounts/<name>.json")
		cookieDB      = flag.String("cookie-db", "", "Explicit path to browser Cookies SQLite database or profile directory for --import-chrome")
		chromeProfile = flag.String("chrome-profile", "", "Alias for --cookie-db (browser profile directory or Cookies SQLite database path)")
		importCookie  = flag.String("import-cookie", "", "Import raw Cookie header or Cookie-Editor JSON from --cookie-value, --cookie-file, or stdin into accounts/<name>.json")
		cookieValue   = flag.String("cookie-value", "", "Raw Cookie header or JSON string for --import-cookie")
		cookieFile    = flag.String("cookie-file", "", "Path to file containing raw Cookie header or Cookie-Editor JSON array for --import-cookie")
		probeOnce     = flag.Bool("probe", false, "Bootstrap all configured accounts, verify models & quota, and exit")
		rotateOnce    = flag.Bool("rotate", false, "Run dual-path cookie rotation on all configured accounts and exit")
	)
	flag.Parse()

	cfg := config.Load()
	dbOrProfile := *cookieDB
	if dbOrProfile == "" {
		dbOrProfile = *chromeProfile
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
			} else {
				fmt.Printf("Account %-16s email=%-28s capacity=%d models=%d fails=%d\n",
					a.ID, a.Email, a.Capacity, len(a.Models), a.Fails)
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
	dbPath := cookies.FindChromeCookieDB(explicitDB)
	if dbPath == "" {
		dbPath = cookies.FindChromeCookieDB(cfg.CookieDB)
	}
	if dbPath == "" {
		return fmt.Errorf("could not locate Chrome/Brave/Chromium Cookies SQLite database")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	targetURL := "https://gemini.google.com/app"
	fmt.Printf("Opening %s in your browser...\n", targetURL)
	openBrowserURL(targetURL)
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

func openBrowserURL(url string) {
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
