package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-octo/octo-agent/internal/browser"
	"github.com/open-octo/octo-agent/internal/config"
)

// stubReachable swaps the TCP liveness probe (status path) and records its args.
func stubReachable(t *testing.T, reachable bool) (*string, *time.Duration) {
	t.Helper()
	orig := browserPortReachable
	t.Cleanup(func() { browserPortReachable = orig })
	var gotDialHost string
	var gotTimeout time.Duration
	browserPortReachable = func(dialHost string, timeout time.Duration) bool {
		gotDialHost, gotTimeout = dialHost, timeout
		return reachable
	}
	return &gotDialHost, &gotTimeout
}

// stubAttachProbe swaps the CDP probe (verify path) and records its args.
func stubAttachProbe(t *testing.T, ok bool, detail string) (*string, *time.Duration) {
	t.Helper()
	orig := browserAttachProbe
	t.Cleanup(func() { browserAttachProbe = orig })
	var gotDialHost string
	var gotTimeout time.Duration
	browserAttachProbe = func(dialHost string, timeout time.Duration) (bool, string) {
		gotDialHost, gotTimeout = dialHost, timeout
		return ok, detail
	}
	return &gotDialHost, &gotTimeout
}

// failIfAttachProbed makes the CDP probe fail the test if called — used to prove
// the status poll never opens a real connection (which would pop Chrome's auth).
func failIfAttachProbed(t *testing.T) {
	t.Helper()
	orig := browserAttachProbe
	t.Cleanup(func() { browserAttachProbe = orig })
	browserAttachProbe = func(dialHost string, timeout time.Duration) (bool, string) {
		t.Errorf("status must not open a CDP connection (would pop auth dialog)")
		return false, ""
	}
}

func TestBrowserStatus_NotConfigured(t *testing.T) {
	setTestHome(t)
	stubReachable(t, true) // reachable, but must be ignored when unconfigured
	failIfAttachProbed(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodGet, "/api/browser/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp browserStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Configured {
		t.Error("Configured = true, want false")
	}
	if resp.Connected {
		t.Error("Connected = true, want false (must not probe when unconfigured)")
	}
	if resp.Port != browserDefaultDebugPort {
		t.Errorf("Port = %d, want default %d", resp.Port, browserDefaultDebugPort)
	}
}

func TestBrowserStatus_ConfiguredUsesTCPProbe(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{ConnectPort: 9333}})
	gotDialHost, gotTimeout := stubReachable(t, true)
	failIfAttachProbed(t) // status must stay popup-free
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodGet, "/api/browser/status", "")
	var resp browserStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Configured || !resp.Connected {
		t.Fatalf("Configured=%v Connected=%v, want both true", resp.Configured, resp.Connected)
	}
	if resp.Port != 9333 || *gotDialHost != "127.0.0.1:9333" {
		t.Errorf("port mismatch: resp=%d probe=%q, want 127.0.0.1:9333", resp.Port, *gotDialHost)
	}
	if *gotTimeout != browserStatusProbeTimeout {
		t.Errorf("status timeout = %v, want quick %v", *gotTimeout, browserStatusProbeTimeout)
	}
}

func TestBrowserVerify_SuccessSavesPort(t *testing.T) {
	setTestHome(t)
	gotDialHost, gotTimeout := stubAttachProbe(t, true, "1 open tab(s)")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/browser/verify", `{"port": 9222}`)
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", w.Code, w.Body.String())
	}
	var resp browserVerifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Saved {
		t.Fatalf("OK=%v Saved=%v, want both true", resp.OK, resp.Saved)
	}
	if *gotDialHost != "127.0.0.1:9222" {
		t.Errorf("probe called with %q, want 127.0.0.1:9222", *gotDialHost)
	}
	// Verify must allow time for the user to approve Chrome's auth prompt.
	if *gotTimeout != browserVerifyProbeTimeout {
		t.Errorf("verify timeout = %v, want long %v", *gotTimeout, browserVerifyProbeTimeout)
	}
	cfg, _ := config.Load()
	if cfg.Browser.ConnectPort != 9222 {
		t.Errorf("connect_port = %d, want 9222 saved", cfg.Browser.ConnectPort)
	}
}

func TestBrowserVerify_FailureDoesNotSave(t *testing.T) {
	setTestHome(t)
	stubAttachProbe(t, false, "connection refused")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/browser/verify", `{"port": 9222}`)
	var resp browserVerifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Saved {
		t.Fatalf("OK=%v Saved=%v, want both false", resp.OK, resp.Saved)
	}
	cfg, _ := config.Load()
	if cfg.Browser.ConnectPort != 0 {
		t.Errorf("connect_port = %d, want 0 (not saved on failure)", cfg.Browser.ConnectPort)
	}
}

func TestBrowserVerify_EmptyBodyUsesDefaultPort(t *testing.T) {
	setTestHome(t)
	gotDialHost, _ := stubAttachProbe(t, false, "refused")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/browser/verify", "")
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", w.Code, w.Body.String())
	}
	if *gotDialHost != "127.0.0.1:9222" {
		t.Errorf("probe called with %q, want default 127.0.0.1:9222", *gotDialHost)
	}
}

// TestBrowserStatus_RemoteHost probes the remote endpoint, not the local port.
// With connect_host set the debug endpoint lives on another machine, so the TCP
// probe must dial that authority: dialing 127.0.0.1:<port> reports whatever
// unrelated local service holds the port (caddy on 80, say) and lights up
// "connected" for a browser that is not there.
func TestBrowserStatus_RemoteHost(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{
		ConnectHost: "x55-cdp.293418.xyz",
		ConnectPort: 80,
	}})
	gotDialHost, _ := stubReachable(t, true)
	failIfAttachProbed(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodGet, "/api/browser/status", "")
	var resp browserStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if want := "x55-cdp.293418.xyz:80"; *gotDialHost != want {
		t.Errorf("probe dialed %q, want the configured host %q", *gotDialHost, want)
	}
	// No local Chrome exists on a remote-endpoint setup, and none is needed:
	// the panel must not warn about installing one.
	if !resp.ChromeAvailable {
		t.Error("ChromeAvailable = false, want true when a debug endpoint is configured")
	}
}

// TestBrowserStatus_HostWithPort keeps a host that already carries its port
// verbatim rather than appending the configured port a second time.
func TestBrowserStatus_HostWithPort(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{
		ConnectHost: "browser.internal:3999",
		ConnectPort: 80,
	}})
	gotDialHost, _ := stubReachable(t, true)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	doJSON(t, srv, http.MethodGet, "/api/browser/status", "")
	if want := "browser.internal:3999"; *gotDialHost != want {
		t.Errorf("probe dialed %q, want %q (explicit port wins)", *gotDialHost, want)
	}
}

// TestBrowserChromeAvailable_LocalUnconfigured is the case the warning exists
// for: no local Chrome and no configured endpoint, so automation cannot work.
func TestBrowserChromeAvailable_LocalUnconfigured(t *testing.T) {
	setTestHome(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodGet, "/api/browser/status", "")
	var resp browserStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ChromeAvailable {
		t.Error("ChromeAvailable = true, want false when nothing is configured")
	}
}

// TestBrowserVerify_RequestPortOverridesConfiguredPort is the regression guard for
// the setup form: the port the user types must be the one verified. Probing the
// saved port instead would vouch for a different browser than the one tested.
//
// The seeded config carries connect_port but not connect_host, so the host the
// test observes afterwards is the one verify itself persisted — that is what
// proves the host travels with the port rather than being left at its old value.
func TestBrowserVerify_RequestPortOverridesConfiguredPort(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{ConnectPort: 80}})
	gotDialHost, _ := stubAttachProbe(t, true, "1 open tab(s)")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/browser/verify", `{"port": 9222}`)
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", w.Code, w.Body.String())
	}
	if want := "127.0.0.1:9222"; *gotDialHost != want {
		t.Errorf("probe dialed %q, want the requested port %q", *gotDialHost, want)
	}
	cfg, _ := config.Load()
	if cfg.Browser.ConnectPort != 9222 {
		t.Errorf("connect_port = %d, want 9222 saved", cfg.Browser.ConnectPort)
	}
	if cfg.Browser.ConnectHost != "" {
		t.Errorf("connect_host = %q, want empty: a local endpoint must not pin 127.0.0.1", cfg.Browser.ConnectHost)
	}
}

// TestBrowserVerify_RemoteHostIsSaved covers the same save path with a host
// configured: verify must persist it alongside the port, or the next load
// resolves connect_port against the local default and reaches the wrong browser.
func TestBrowserVerify_RemoteHostIsSaved(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{
		ConnectHost: "browser.internal:3999",
		ConnectPort: 80,
	}})
	gotDialHost, _ := stubAttachProbe(t, true, "1 open tab(s)")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	w := doJSON(t, srv, http.MethodPost, "/api/browser/verify", `{"port": 9222}`)
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", w.Code, w.Body.String())
	}
	if want := "browser.internal:3999"; *gotDialHost != want {
		t.Errorf("probe dialed %q, want %q", *gotDialHost, want)
	}
	cfg, _ := config.Load()
	if cfg.Browser.ConnectPort != 9222 {
		t.Errorf("connect_port = %d, want 9222 saved", cfg.Browser.ConnectPort)
	}
	// The port is dropped from the persisted host: connect_port carries it, and
	// leaving both would freeze the endpoint at 3999 no matter what the port says.
	if cfg.Browser.ConnectHost != "browser.internal" {
		t.Errorf("connect_host = %q, want browser.internal saved alongside the port", cfg.Browser.ConnectHost)
	}
}

// TestBrowserVerify_HostWithPortKeepsItsOwnPort proves a host that already names
// its port stays authoritative: the request port must not rewrite it, because
// that host:port is the endpoint (a reverse proxy, say) and not a host we get to
// reinterpret.
func TestBrowserVerify_HostWithPortKeepsItsOwnPort(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{
		ConnectHost: "browser.internal:3999",
		ConnectPort: 80,
	}})
	gotDialHost, _ := stubAttachProbe(t, true, "1 open tab(s)")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	doJSON(t, srv, http.MethodPost, "/api/browser/verify", `{"port": 9222}`)
	if want := "browser.internal:3999"; *gotDialHost != want {
		t.Errorf("probe dialed %q, want %q (host's own port is authoritative)", *gotDialHost, want)
	}
}

// TestBrowserVerify_SchemeHostKeepsScheme guards the persisted host: an https
// endpoint stripped to a bare hostname would silently downgrade to plain http on
// the next load.
func TestBrowserVerify_SchemeHostKeepsScheme(t *testing.T) {
	setTestHome(t)
	seedModels(t, config.Config{Browser: config.BrowserConfig{
		ConnectHost: "https://browser.internal:8443",
		ConnectPort: 80,
	}})
	stubAttachProbe(t, true, "1 open tab(s)")
	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})

	doJSON(t, srv, http.MethodPost, "/api/browser/verify", `{"port": 9222}`)
	cfg, _ := config.Load()
	if want := "https://browser.internal"; cfg.Browser.ConnectHost != want {
		t.Errorf("connect_host = %q, want %q (scheme kept, port dropped)", cfg.Browser.ConnectHost, want)
	}
}

// TestConnectByHost_RejectsMalformed pins the error path: a malformed authority
// must fail loudly instead of falling back to a port of 0, which means something
// else entirely (Chrome's own port auto-selection) downstream.
func TestConnectByHost_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{"host", "host:", "host:abc", "host:0", "host:99999"} {
		if _, err := browser.ConnectByHost(context.Background(), bad); err == nil {
			t.Errorf("ConnectByHost(%q) = nil error, want a rejection", bad)
		}
	}
}
