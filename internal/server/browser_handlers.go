package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
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

// browserPortReachable reports whether something is listening on the debug port,
// using a plain TCP connect. We deliberately do NOT open a CDP WebSocket here:
// a real debug connection makes Chrome pop an authorization dialog, and the
// status check runs on every Settings page load. TCP liveness is the same
// popup-free signal the web-access skill uses. Package var so tests can fake it.
var browserPortReachable = realBrowserPortReachable

func realBrowserPortReachable(port int, timeout time.Duration) bool {
	return realBrowserAddrTCP(fmt.Sprintf("127.0.0.1:%d", port), timeout)
}

// browserAddrReachable reports whether the connect_url endpoint accepts a TCP
// connect — the same popup-free liveness signal as browserPortReachable, for a
// host[:port] or URL-style address. Package var so tests can fake it.
var browserAddrReachable = realBrowserAddrReachable

func realBrowserAddrReachable(urlSpec string, timeout time.Duration) bool {
	return realBrowserAddrTCP(browserAddrHostPort(urlSpec), timeout)
}

// realBrowserAddrTCP opens a plain TCP connection to hostPort within timeout.
func realBrowserAddrTCP(hostPort string, timeout time.Duration) bool {
	if hostPort == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// browserAddrHostPort extracts host:port from a connect_url-ish value ("ws://…",
// "http(s)://…", "host:port", "host"). Defaults to 9222; empty when unparseable.
func browserAddrHostPort(urlSpec string) string {
	lower := strings.ToLower(urlSpec)
	if i := strings.IndexAny(lower, " \t"); i >= 0 {
		return "" // whitespace: bogus
	}
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

// browserAttachProbe opens a real CDP connection the way the browser tool does —
// connect_port first, then default-profile discovery — and confirms a
// page-level call works. This is what triggers Chrome's authorization prompt,
// so it is used only by verify, never by the status poll. Package var so tests
// can substitute a fake (the real one needs a live browser).
var browserAttachProbe = realBrowserAttachProbe

func realBrowserAttachProbe(port int, timeout time.Duration) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	b, err := browser.ConnectByPort(ctx, port)
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

// browserAttachProbeURL probes a connect_url endpoint (remote/tunnel). No
// default-profile fallback: that discovery only ever finds local Chrome, which
// would mask a genuinely unreachable remote address.
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
	// Configured is true once setup has wired a connection (connect_port,
	// connect_url or attach_running). This is the "set up vs not" distinction
	// the panel shows.
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
	configured := cfg.Browser.ConnectPort != 0 || cfg.Browser.ConnectURL != "" || cfg.Browser.AttachRunning
	resp := browserStatusResponse{
		Configured:      configured,
		Port:            browserEffectivePort(cfg),
		URL:             cfg.Browser.ConnectURL,
		AttachRunning:   cfg.Browser.AttachRunning,
		ChromeAvailable: browser.ChromeAvailable(cfg.Browser.ExecPath),
	}
	if configured {
		if cfg.Browser.ConnectURL != "" {
			resp.Connected = browserAddrReachable(cfg.Browser.ConnectURL, browserStatusProbeTimeout)
		} else {
			resp.Connected = browserPortReachable(resp.Port, browserStatusProbeTimeout)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── POST /api/browser/verify ────────────────────────────────────────────────

// browserVerifyRequest carries what to probe. URL (a connect_url form — ws://…,
// http://…, host[:port]) takes precedence; otherwise Port, with 0 meaning "use
// the configured/default port".
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
			cfg.Browser.ConnectURL = url
			cfg.Browser.ConnectPort = 0 // one attach path wins
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

	// The long timeout gives the user time to approve Chrome's first-connection
	// authorization prompt — the single dial blocks until they click allow.
	ok, detail := browserAttachProbe(port, browserVerifyProbeTimeout)
	resp := browserVerifyResponse{OK: ok, Port: port, Detail: detail}
	// On success, persist connect_port so the tool reuses this Chrome on every
	// future session — same wiring `octo browser setup` does on connect.
	if ok && cfg.Browser.ConnectPort != port {
		cfg.Browser.ConnectPort = port
		if err := cfg.Save(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("save config: %v", err))
			return
		}
		resp.Saved = true
	}
	writeJSON(w, http.StatusOK, resp)
}
