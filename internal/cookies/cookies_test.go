package cookies

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

type stubCookieDoer struct {
	fn func(req *fhttp.Request) (*fhttp.Response, error)
}

func (d *stubCookieDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.fn(req)
}

func (d *stubCookieDoer) CloseIdleConnections() {}

func TestAbsorbSetCookieAndSAPISIDHash(t *testing.T) {
	initial := "SID=s1; SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=old_ts"
	merged, changed := AbsorbSetCookie(initial, []string{
		"__Secure-1PSIDTS=new_ts; Path=/; Secure; HttpOnly",
		"UNRELATED_COOKIE=ignore_me; Path=/",
	})
	if !changed {
		t.Fatal("expected changed=true when __Secure-1PSIDTS rotated")
	}
	if !strings.Contains(merged, "__Secure-1PSIDTS=new_ts") || strings.Contains(merged, "UNRELATED_COOKIE") {
		t.Fatalf("unexpected merged header: %s", merged)
	}

	hash := SAPISIDHash(merged, "https://gemini.google.com")
	if !strings.HasPrefix(hash, "SAPISIDHASH ") || len(hash) < 50 {
		t.Fatalf("unexpected SAPISIDHASH: %s", hash)
	}
}

func TestDualPathCookieRotator(t *testing.T) {
	var seenBodies []string
	var seenURLs []string
	doer := &stubCookieDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			seenURLs = append(seenURLs, req.URL.String())
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				seenBodies = append(seenBodies, string(b))
			}
			if strings.Contains(req.URL.Path, "RotateCookiesPage") {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`<script>init('rot_id_999', 1, 540)</script>`)),
				}, nil
			}
			// RotateCookies POST
			lastBody := ""
			if len(seenBodies) > 0 {
				lastBody = seenBodies[len(seenBodies)-1]
			}
			if strings.HasPrefix(lastBody, "[000,") {
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_psidts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader("[]")),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header: fhttp.Header{
					"Set-Cookie": []string{"__Secure-1PSIDCC=fresh_psidcc; Path=/; Secure; HttpOnly"},
				},
				Body: io.NopCloser(strings.NewReader("[]")),
			}, nil
		},
	}

	sc, err := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	if err != nil {
		t.Fatalf("NewStealthClient: %v", err)
	}

	rot := NewRotator()
	initial := "SAPISID=sap; __Secure-1PSID=psid; __Secure-1PSIDTS=old_ts; __Secure-1PSIDCC=old_cc"
	// Call 1 uses primary [000,"-0000000000000000000"] path
	res, err := rot.RotateAccount(context.Background(), "acct1", initial, sc)
	if err != nil {
		t.Fatalf("RotateAccount primary failed: %v", err)
	}
	if !res.Rotated || res.Status != 200 || !strings.Contains(res.CookieHeader, "__Secure-1PSIDTS=fresh_psidts") {
		t.Fatalf("expected primary PSIDTS rotation, got: %+v", res)
	}
	// Call 2 within 60s falls back to 658 iframe path
	res2, err := rot.RotateAccount(context.Background(), "acct1", res.CookieHeader, sc)
	if err != nil {
		t.Fatalf("RotateAccount fallback failed: %v", err)
	}
	if !res2.Rotated || !strings.Contains(res2.CookieHeader, "__Secure-1PSIDCC=fresh_psidcc") {
		t.Fatalf("expected fallback PSIDCC rotation, got: %+v", res2)
	}
	if len(seenBodies) != 2 || seenBodies[0] != `[000,"-0000000000000000000"]` || seenBodies[1] != `[658,"rot_id_999"]` {
		t.Fatalf("unexpected RotateCookies POST bodies: %v", seenBodies)
	}
}

func TestSaveAndLoadAccountFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acct.json")
	rec := CredentialFile{
		Name:    "acct",
		Email:   "acct@gmail.com",
		Cookies: "SAPISID=123",
		AT:      "SNlM0e_tok",
		BL:      "boq_123",
	}
	mtime, err := SaveCredentialFile(path, rec)
	if err != nil || mtime <= 0 {
		t.Fatalf("SaveCredentialFile failed: mtime=%d err=%v", mtime, err)
	}
	loaded, mtime2, err := LoadAccountFile(path)
	if err != nil || mtime2 <= 0 {
		t.Fatalf("LoadAccountFile failed: %v", err)
	}
	if loaded.Email != "acct@gmail.com" || loaded.AT != "SNlM0e_tok" {
		t.Fatalf("unexpected loaded record: %+v", loaded)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", st.Mode().Perm())
	}
}
