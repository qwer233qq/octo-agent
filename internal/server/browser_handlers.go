package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-octo/octo-agent/internal/browser"
	"github.com/open-octo/octo-agent/internal/config"
)

// browserDefaultDebugPort is the conventional Chrome remote-debugging port —
// the value the chrome://inspect toggle serves on and the one `octo browser
// setup` defaults to. Kept in sync with cmd/octo/browser.go's defaultDebugPort.
const browserDefaultDebugPort = 9222

// Probe timeouts. The status check is a quick TCP liveness ping. Verify waits
// much longer: the first WebSocket connect to a chrome://inspect-enabled browser
// pops an in-browser authorization prompt, and a non-technical user needs time
// to switch to Chrome and click "allow" before the dial completes.
const (
	browserStatusProbeTimeout = 2 * time.Second
	browserVerifyProbeTimeout = 60 * time.Second
)

// browserPortReachable reports whether something is listening on the debug
// endpoint, using a plain TCP connect. We deliberately do NOT open a CDP
// WebSocket here: a real debug connection makes Chrome pop an authorization
// dialog, and the status check runs on every Settings page load. TCP liveness is
// the same popup-free signal the web-access skill uses. Package var so tests can
// fake it.
//
// dialHost is host[:port] as resolved from the config. Passing it through matters:
// with browser.connect_host set, the debug endpoint is remote (a bridged or
// genuinely remote Chrome), and probing 127.0.0.1 would report whatever local
// service happens to hold that port.
var browserPortReachable = realBrowserPortReachable

func realBrowserPortReachable(dialHost string, timeout time.Duration) bool {
	// An empty host means no endpoint was configured at all — nothing to dial, and
	// defaulting to the conventional port would probe a browser nobody set up.
	if dialHost == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", dialHost, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// browserAttachProbe opens a real CDP connection the way the browser tool does —
// connect_port first, then default-profile discovery — and confirms a
// page-level call works. This is what triggers Chrome's authorization prompt,
// so it is used only by verify, never by the status poll. Package var so tests
// can substitute a fake (the real one needs a live browser).
var browserAttachProbe = realBrowserAttachProbe

func realBrowserAttachProbe(dialHost string, timeout time.Duration) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	b, err := browser.ConnectByHost(ctx, dialHost)
	if err != nil {
		var derr error
		if b, derr = browser.DiscoverRunningChrome(ctx); derr != nil {
			return false, err.Error()
		}
	}
	defer b.Close()

	pages, err := b.Pages(ctx)
	if err != nil {
		return false, fmt.Sprintf("connected, but a CDP call failed: %v", err)
	}
	return true, fmt.Sprintf("%d open tab(s)", len(pages))
}

// browserEffectivePort is the port the tool would attach on: the configured
// connect_port, or the conventional default when unset.
func browserEffectivePort(cfg config.Config) int {
	if cfg.Browser.ConnectPort != 0 {
		return cfg.Browser.ConnectPort
	}
	return browserDefaultDebugPort
}

// ─── GET /api/browser/status ─────────────────────────────────────────────────

// browserStatusResponse is the JSON shape for GET /api/browser/status.
type browserStatusResponse struct {
	// Configured is true once setup has wired a connection (connect_port or
	// attach_running). This is the "set up vs not" distinction the panel shows.
	Configured bool `json:"configured"`
	// Connected reflects a popup-free TCP liveness check — whether the debug
	// port is reachable right now. Only probed when Configured (an un-set-up
	// browser is "not set up", not "down").
	Connected       bool   `json:"connected"`
	Port            int    `json:"port"`
	URL             string `json:"url,omitempty"`
	AttachRunning   bool   `json:"attach_running"`
	ChromeAvailable bool   `json:"chrome_available"`
}

func (s *Server) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load()
	configured := cfg.Browser.ConnectURL != "" || cfg.Browser.ConnectPort != 0 || cfg.Browser.AttachRunning
	resp := browserStatusResponse{
		Configured:      configured,
		Port:            browserEffectivePort(cfg),
		URL:             cfg.Browser.ConnectURL,
		AttachRunning:   cfg.Browser.AttachRunning,
		ChromeAvailable: browserChromeAvailable(cfg),
	}
	if configured {
		dialHost := browserDialHost(cfg)
		if cfg.Browser.ConnectURL != "" {
			dialHost = browserAddrHostPort(cfg.Browser.ConnectURL)
		}
		resp.Connected = browserPortReachable(dialHost, browserStatusProbeTimeout)
	}
	writeJSON(w, http.StatusOK, resp)
}

// browserAddrHostPort extracts host:port from a connect_url-ish value ("ws://…",
// "http(s)://…", "host:port", "host") for the TCP liveness probe. Defaults to
// 9222; the default dial host when unparseable.
func browserAddrHostPort(urlSpec string) string {
	lower := strings.ToLower(urlSpec)
	switch {
	case strings.HasPrefix(lower, "ws://"):
		urlSpec = urlSpec[len("ws://"):]
	case strings.HasPrefix(lower, "wss://"):
		urlSpec = urlSpec[len("wss://"):]
	case strings.HasPrefix(lower, "http://"):
		urlSpec = urlSpec[len("http://"):]
	case strings.HasPrefix(lower, "https://"):
		urlSpec = urlSpec[len("https://"):]
	}
	urlSpec = strings.TrimSuffix(urlSpec, "/")
	if i := strings.Index(urlSpec, "/"); i >= 0 {
		urlSpec = urlSpec[:i]
	}
	if urlSpec == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(urlSpec); err != nil {
		urlSpec = net.JoinHostPort(strings.Trim(urlSpec, "[]"), "9222")
	}
	return urlSpec
}

// browserDialHost is the host[:port] the tool attaches to, resolved from the
// config. This is the authority both the TCP liveness probe and the CDP dial use,
// so they must agree — probing a different address than the one the tool dials
// reports liveness that does not exist.
//
// The resolution mirrors browser.cdpDialHost: an explicit scheme is kept
// verbatim, a host already carrying a port is used as-is, otherwise the port is
// appended. The browser package keeps its own copy because the dial itself needs
// it; duplicating the few lines here avoids exporting an internal helper just
// for the status probe.
func browserDialHost(cfg config.Config) string {
	return browserDialHostWithPort(cfg, browserEffectivePort(cfg))
}

// browserDialHostWithPort resolves the dial authority using an explicit port,
// which is what verify needs: the setup form submits the port under test, and the
// probe has to reach that one rather than whatever the config already holds.
func browserDialHostWithPort(cfg config.Config, port int) string {
	host := cfg.Browser.ConnectHost
	if host == "" {
		host = "127.0.0.1"
	}
	// An explicit scheme is kept verbatim; the caller named a full endpoint.
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	// A host already carrying its own port keeps it: the endpoint it names is the
	// authority, and a request port would otherwise silently replace it.
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// browserDialHostname is the value to persist as browser.connect_host so a later
// connect_port resolves against the same endpoint verify just proved. Empty when
// the host is local, which is the config's zero value and means "this machine".
//
// A scheme the caller spelled out is kept: cdpDialHost treats a scheme-bearing
// host as a complete endpoint, so dropping it would silently downgrade an https
// endpoint to plain http on the next load.
func browserDialHostname(cfg config.Config) string {
	host := cfg.Browser.ConnectHost
	if host == "" {
		return ""
	}
	if i := strings.Index(host, "://"); i >= 0 {
		scheme := host[:i+3]
		rest := host[i+3:]
		if h, _, err := net.SplitHostPort(rest); err == nil {
			return scheme + h
		}
		return host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// browserChromeAvailable reports whether the tool could attach to a browser, so
// the panel can warn when setup is missing. A configured debug endpoint counts
// as available even when no local Chrome exists: with connect_host set the
// browser runs elsewhere and is reached over the network, so a local executable
// is neither required nor looked for. Only the local-launch case needs one.
func browserChromeAvailable(cfg config.Config) bool {
	if cfg.Browser.ConnectURL != "" || cfg.Browser.ConnectHost != "" || cfg.Browser.ConnectPort != 0 || cfg.Browser.AttachRunning {
		return true
	}
	return browser.ChromeAvailable(cfg.Browser.ExecPath)
}

// ─── POST /api/browser/verify ────────────────────────────────────────────────

// browserVerifyRequest carries what to probe. URL (a connect_url form — ws://…,
// http(s)://…, host[:port]) takes precedence; otherwise Port, with 0 meaning
// "use the configured/default port".
type browserVerifyRequest struct {
	Port int    `json:"port"`
	URL  string `json:"url,omitempty"`
}

type browserVerifyResponse struct {
	OK     bool   `json:"ok"`
	Port   int    `json:"port"`
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail"`
	// Saved is true when a successful probe wired connect_port/connect_url into
	// the config (the web equivalent of `octo browser setup`).
	Saved bool `json:"saved"`
}

// browserAttachProbeURL probes a connect_url endpoint (remote/tunnel). No
// default-profile fallback: that discovery only ever finds local Chrome, which
// would mask a genuinely unreachable remote address. Package var so tests can
// substitute a fake, mirroring browserAttachProbe.
var browserAttachProbeURL = realBrowserAttachProbeURL

func realBrowserAttachProbeURL(url string, timeout time.Duration) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	b, err := browser.ConnectByURL(ctx, url)
	if err != nil {
		return false, err.Error()
	}
	defer b.Close()

	pages, err := b.Pages(ctx)
	if err != nil {
		return false, fmt.Sprintf("connected, but a CDP call failed: %v", err)
	}
	return true, fmt.Sprintf("%d open tab(s)", len(pages))
}

func (s *Server) handleBrowserVerify(w http.ResponseWriter, r *http.Request) {
	var req browserVerifyRequest
	// An empty or malformed body just means "use the configured/default port".
	_ = readBodyJSON(r, &req)

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load config: %v", err))
		return
	}

	if url := strings.TrimSpace(req.URL); url != "" {
		// The long timeout gives the user time to approve Chrome's
		// first-connection authorization prompt — the single dial blocks until
		// they click allow.
		ok, detail := browserAttachProbeURL(url, browserVerifyProbeTimeout)
		resp := browserVerifyResponse{OK: ok, URL: url, Detail: detail}
		if ok && cfg.Browser.ConnectURL != url {
			// One attach path wins: an old local port/host pair must not
			// linger once the tool will always prefer connect_url.
			cfg.Browser.ConnectURL = url
			cfg.Browser.ConnectPort = 0
			cfg.Browser.ConnectHost = ""
			if err := cfg.Save(); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
				return
			}
			resp.Saved = true
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	port := req.Port
	if port <= 0 || port > 65535 {
		port = browserEffectivePort(cfg)
	}

	// The dial authority is the setup form's host with the port under test. An
	// explicit request port wins over the configured one: the form lets the user
	// point at a Chrome on a different port than the saved config, and verify has
	// to check the one they typed or it vouches for the wrong browser.
	dialHost := browserDialHostWithPort(cfg, port)

	// The long timeout gives the user time to approve Chrome's first-connection
	// authorization prompt — the single dial blocks until they click allow.
	ok, detail := browserAttachProbe(dialHost, browserVerifyProbeTimeout)
	resp := browserVerifyResponse{OK: ok, Port: port, Detail: detail}
	// On success, persist connect_port so the tool reuses this Chrome on every
	// future session — same wiring `octo browser setup` does on connect. The host
	// is saved alongside it: verify dialed the configured host, so a later
	// connect_port without the matching connect_host would reach a different
	// endpoint than the one just proven to work.
	if ok && cfg.Browser.ConnectPort != port {
		cfg.Browser.ConnectPort = port
		cfg.Browser.ConnectHost = browserDialHostname(cfg)
		if err := cfg.Save(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
			return
		}
		resp.Saved = true
	}
	writeJSON(w, http.StatusOK, resp)
}
