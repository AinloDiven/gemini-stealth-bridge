package transport

import (
	"context"
	"testing"
	"time"
)

// TestPoolAcquireWithProxyRoutesPerAccount verifies that two accounts in the
// same pool can egress through different proxies, and that the pool-wide
// default applies to accounts without their own proxy.
func TestPoolAcquireWithProxyRoutesPerAccount(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{
			CustomDoer: &stubDoer{},
			ProxyURL:   "socks5://127.0.0.1:1080",
		},
		MaxSessions: 4,
		MaxQueue:    2,
		WaitTimeout: time.Second,
	})

	proxied, err := pool.AcquireWithProxy(context.Background(), "acct-a", "SID=1", "http://127.0.0.1:9001")
	if err != nil {
		t.Fatalf("AcquireWithProxy: %v", err)
	}
	if got := proxied.ProxyURL(); got != "http://127.0.0.1:9001" {
		t.Fatalf("expected account A to use its own proxy, got %q", got)
	}
	if got := proxied.Client.ProxyURL(); got != "http://127.0.0.1:9001" {
		t.Fatalf("expected the session client to carry the account proxy, got %q", got)
	}

	direct, err := pool.AcquireWithProxy(context.Background(), "acct-b", "SID=2", "")
	if err != nil {
		t.Fatalf("AcquireWithProxy: %v", err)
	}
	if got := direct.ProxyURL(); got != "" {
		t.Fatalf("expected account B to use the pool default, got %q", got)
	}
	if got := direct.Client.ProxyURL(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("expected the pool default proxy on the client, got %q", got)
	}
}

// TestPoolAcquireWithProxyRebuildsSessionOnProxyChange verifies that pointing
// an account at a different proxy replaces its session instead of keeping the
// old client, which would still hold connections to the previous egress.
func TestPoolAcquireWithProxyRebuildsSessionOnProxyChange(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     2,
		WaitTimeout:  time.Second,
	})

	first, err := pool.AcquireWithProxy(context.Background(), "acct", "SID=1", "http://127.0.0.1:9001")
	if err != nil {
		t.Fatalf("first AcquireWithProxy: %v", err)
	}
	firstClient := first.Client

	second, err := pool.AcquireWithProxy(context.Background(), "acct", "SID=1", "http://127.0.0.1:9002")
	if err != nil {
		t.Fatalf("second AcquireWithProxy: %v", err)
	}
	if second == first {
		t.Fatalf("expected a new session after the proxy changed")
	}
	if got := second.ProxyURL(); got != "http://127.0.0.1:9002" {
		t.Fatalf("expected the new proxy, got %q", got)
	}
	if second.Client == firstClient {
		t.Fatalf("expected a new client after the proxy changed")
	}
}

// TestPoolAcquireWithProxyKeepsSessionWhenProxyUnchanged verifies that a
// repeated acquire on the same account and proxy reuses the warm session
// instead of rebuilding the transport on every request.
func TestPoolAcquireWithProxyKeepsSessionWhenProxyUnchanged(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     2,
		WaitTimeout:  time.Second,
	})

	first, err := pool.AcquireWithProxy(context.Background(), "acct", "SID=1", "http://127.0.0.1:9001")
	if err != nil {
		t.Fatalf("first AcquireWithProxy: %v", err)
	}
	second, err := pool.AcquireWithProxy(context.Background(), "acct", "SID=1", "http://127.0.0.1:9001")
	if err != nil {
		t.Fatalf("second AcquireWithProxy: %v", err)
	}
	if second != first {
		t.Fatalf("expected the warm session to be reused when the proxy is unchanged")
	}
}
