package cookies

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	_ "modernc.org/sqlite"
)

// BrowserProfiles maps well-known Linux browser names to their default Cookie DB paths.
var BrowserProfiles = map[string]string{
	"chrome":   ".config/google-chrome/Default/Cookies",
	"chromium": ".config/chromium/Default/Cookies",
	"brave":    ".config/BraveSoftware/Brave-Browser/Default/Cookies",
	"edge":     ".config/microsoft-edge/Default/Cookies",
	"vivaldi":  ".config/vivaldi/Default/Cookies",
}

var keyringApps = []string{"chrome", "brave", "chromium", "microsoft-edge", "vivaldi", "opera"}

// CookieRow represents a decrypted cookie from a browser SQLite database.
type CookieRow struct {
	Host  string
	Name  string
	Value string
}

// ReadResult summarizes a browser cookie DB extraction.
type ReadResult struct {
	Cookies     []CookieRow
	Header      string
	Decrypted   int
	Plain       int
	Failed      int
	DomainBound int
	KeyFound    bool
}

// FindBrowserCookieDB resolves a browser name or file path to an existing SQLite Cookie DB.
func FindBrowserCookieDB(nameOrPath string) (string, error) {
	if strings.Contains(nameOrPath, "/") || strings.HasSuffix(nameOrPath, "Cookies") || strings.HasSuffix(nameOrPath, ".sqlite") {
		if st, err := os.Stat(nameOrPath); err == nil && !st.IsDir() {
			return nameOrPath, nil
		}
		return "", fmt.Errorf("cookie database not found at %s", nameOrPath)
	}
	home, _ := os.UserHomeDir()
	rel, ok := BrowserProfiles[strings.ToLower(strings.TrimSpace(nameOrPath))]
	if !ok {
		return "", fmt.Errorf("unknown browser %q (known: chrome, chromium, brave, edge, vivaldi)", nameOrPath)
	}
	full := filepath.Join(home, rel)
	if st, err := os.Stat(full); err == nil && !st.IsDir() {
		return full, nil
	}
	return "", fmt.Errorf("no cookie database found for %s at %s", nameOrPath, full)
}

// ReadBrowserCookies reads and decrypts Google session cookies from a Chrome/Chromium
// or Firefox SQLite database without locking the browser's live file.
func ReadBrowserCookies(dbPath string) (*ReadResult, error) {
	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("gsb-ck-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	tmpDB := filepath.Join(tmpDir, "Cookies.db")
	if err := copyFile(dbPath, tmpDB); err != nil {
		return nil, fmt.Errorf("copy cookie db: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			_ = copyFile(dbPath+suffix, tmpDB+suffix)
		}
	}

	db, err := sql.Open("sqlite", tmpDB)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if strings.HasSuffix(strings.ToLower(dbPath), ".sqlite") || strings.Contains(strings.ToLower(dbPath), "firefox") {
		return readFirefoxCookies(db)
	}
	return readChromiumCookies(db)
}

func readFirefoxCookies(db *sql.DB) (*ReadResult, error) {
	rows, err := db.Query(`SELECT host, name, value FROM moz_cookies WHERE host LIKE '%google.com' OR host LIKE '%gemini.google%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &ReadResult{KeyFound: true}
	for rows.Next() {
		var host, name, value string
		if err := rows.Scan(&host, &name, &value); err != nil {
			continue
		}
		res.Plain++
		res.Cookies = append(res.Cookies, CookieRow{Host: host, Name: name, Value: value})
	}
	res.Header = bestHeaderFromRows(res.Cookies)
	return res, nil
}

type rawChromiumRow struct {
	host      string
	name      string
	encrypted []byte
	plain     string
}

func readChromiumCookies(db *sql.DB) (*ReadResult, error) {
	rows, err := db.Query(`SELECT host_key, name, encrypted_value, value FROM cookies WHERE host_key LIKE '%google%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rawRows []rawChromiumRow
	for rows.Next() {
		var r rawChromiumRow
		if err := rows.Scan(&r.host, &r.name, &r.encrypted, &r.plain); err != nil {
			continue
		}
		rawRows = append(rawRows, r)
	}

	key256, key128, keyFound := detectBestKey(rawRows)
	res := &ReadResult{KeyFound: keyFound}
	for _, r := range rawRows {
		val := r.plain
		if len(r.encrypted) > 0 {
			if !keyFound {
				res.Failed++
				continue
			}
			dec := decryptCookieBytes(r.encrypted, key256, key128)
			if dec == nil {
				res.Failed++
				continue
			}
			stripped, bound := stripDomainPrefix(dec, r.host)
			if bound {
				res.DomainBound++
			}
			val = string(stripped)
			res.Decrypted++
		} else {
			res.Plain++
		}
		res.Cookies = append(res.Cookies, CookieRow{Host: r.host, Name: r.name, Value: val})
	}
	res.Header = bestHeaderFromRows(res.Cookies)
	return res, nil
}

func candidatePasswords() []string {
	var passes []string
	if envPass := os.Getenv("CHROME_PASS"); envPass != "" {
		passes = append(passes, envPass)
	} else {
		seen := map[string]bool{}
		for _, app := range keyringApps {
			out, err := exec.Command("secret-tool", "lookup", "application", app).Output()
			if err == nil {
				p := strings.TrimSpace(string(out))
				if p != "" && !seen[p] {
					seen[p] = true
					passes = append(passes, p)
				}
			}
		}
	}
	passes = append(passes, "peanuts", "")
	return passes
}

func detectBestKey(rows []rawChromiumRow) (best256, best128 []byte, ok bool) {
	passes := candidatePasswords()
	bestScore := 0
	for _, p := range passes {
		k256 := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1, 32, sha1.New)
		k128 := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1, 16, sha1.New)
		score := 0
		for _, r := range rows {
			if len(r.encrypted) == 0 {
				continue
			}
			if decryptCookieBytes(r.encrypted, k256, k128) != nil {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			best256 = k256
			best128 = k128
			ok = true
		}
	}
	return best256, best128, ok
}

func decryptCookieBytes(buf, key256, key128 []byte) []byte {
	if len(buf) < 4 {
		return nil
	}
	prefix := string(buf[:3])
	if prefix != "v10" && prefix != "v11" {
		return nil
	}
	rest := buf[3:]
	// Try AES-256-GCM (Chrome / Brave modern Linux keyring)
	if len(rest) >= 12+16 && len(key256) == 32 {
		if block, err := aes.NewCipher(key256); err == nil {
			if gcm, err := cipher.NewGCM(block); err == nil {
				nonce := rest[:12]
				ctAndTag := rest[12:]
				if plain, err := gcm.Open(nil, nonce, ctAndTag, nil); err == nil {
					return plain
				}
			}
		}
	}
	// Try AES-128-CBC (Chromium / v10 legacy)
	if len(rest) >= aes.BlockSize && len(rest)%aes.BlockSize == 0 && len(key128) == 16 {
		if block, err := aes.NewCipher(key128); err == nil {
			iv := bytes.Repeat([]byte{0x20}, aes.BlockSize)
			out := make([]byte, len(rest))
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, rest)
			pad := int(out[len(out)-1])
			if pad > 0 && pad <= aes.BlockSize && pad <= len(out) {
				validPad := true
				for i := len(out) - pad; i < len(out); i++ {
					if int(out[i]) != pad {
						validPad = false
						break
					}
				}
				if validPad {
					return out[:len(out)-pad]
				}
			}
		}
	}
	return nil
}

// Chrome 130+ prepends SHA-256(host_key) to the decrypted cookie value.
func stripDomainPrefix(buf []byte, host string) ([]byte, bool) {
	if len(buf) >= 32 {
		sum := sha256.Sum256([]byte(host))
		if bytes.Equal(buf[:32], sum[:]) {
			return buf[32:], true
		}
	}
	return buf, false
}

func bestHeaderFromRows(rows []CookieRow) string {
	score := func(h string) int {
		if h == ".google.com" || h == "google.com" {
			return 3
		}
		if strings.HasSuffix(h, ".google.com") {
			return 2
		}
		return 1
	}
	best := make(map[string]string)
	bestScore := make(map[string]int)
	for _, r := range rows {
		if !essentialSet[r.Name] || r.Value == "" {
			continue
		}
		s := score(r.Host)
		if s > bestScore[r.Name] {
			best[r.Name] = r.Value
			bestScore[r.Name] = s
		}
	}
	return CanonicalHeader(best)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// FindChromeCookieDB locates an existing Chrome/Brave/Chromium SQLite Cookie DB.
func FindChromeCookieDB(explicitPath string) string {
	if explicitPath != "" {
		if p, err := FindBrowserCookieDB(explicitPath); err == nil {
			return p
		}
	}
	for _, browser := range []string{"chrome", "brave", "chromium", "edge", "vivaldi"} {
		if p, err := FindBrowserCookieDB(browser); err == nil {
			return p
		}
	}
	return ""
}

// ReadChromeCookies reads and decrypts Google cookies from dbPath and returns the canonical Cookie header.
func ReadChromeCookies(dbPath string) (string, error) {
	res, err := ReadBrowserCookies(dbPath)
	if err != nil {
		return "", err
	}
	return res.Header, nil
}
