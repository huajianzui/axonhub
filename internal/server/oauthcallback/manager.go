// Package oauthcallback implements loopback OAuth callback listeners for
// subscription channel authorization (Codex, Claude Code, Antigravity, xAI).
//
// Subscription providers redirect the browser to a fixed loopback URL after the
// operator approves access. AxonHub requires the operator to copy that URL out
// of the browser address bar and paste it back into the channel dialog. This
// package removes that step: it listens on the provider's fixed callback
// address, captures the redirect, and exposes it to the console, which polls
// until it appears and then runs the existing exchange flow unchanged.
//
// The capture is deliberately transient: it lives in memory, expires after a
// short TTL, and is never persisted. The existing exchange endpoints remain the
// only code path that turns a callback into stored credentials, so all of their
// state validation still applies.
//
// The listeners bind loopback only. Binding is best effort: when a port is
// already taken (for example by the provider's official CLI), the listener is
// skipped and the console falls back to pasting the callback URL by hand.
package oauthcallback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/log"
)

const (
	// CapturePath receives the fragment-bearing callback URL from the result
	// page. Providers that put OAuth state in the URL fragment (Claude Code)
	// never send it to the server, so the page posts window.location.href back.
	CapturePath = "/_capture"

	// captureTTL bounds how long a captured callback stays available to the
	// console before it is treated as abandoned.
	captureTTL = 5 * time.Minute

	// maxCaptureBody bounds the size of a posted callback URL.
	maxCaptureBody = 8 << 10

	// maxCallbackURLLen bounds a callback URL accepted from a redirect.
	maxCallbackURLLen = 8 << 10

	shutdownTimeout = 3 * time.Second
)

// Provider describes one subscription provider's fixed loopback callback.
type Provider struct {
	// ID is the console-facing provider key, e.g. "codex".
	ID string

	// RedirectURI is the provider-fixed redirect target. Both the listen
	// address and the accepted request path are derived from it.
	RedirectURI string

	// host is the hostname the listener binds, taken from RedirectURI.
	host string

	// port is the port the listener binds, taken from RedirectURI.
	port string

	// path is the request path the provider redirects to.
	path string
}

// ParseProvider validates a redirect URI and derives its bind address.
func ParseProvider(id, redirectURI string) (Provider, error) {
	u, err := url.Parse(strings.TrimSpace(redirectURI))
	if err != nil {
		return Provider{}, fmt.Errorf("parse %s redirect uri: %w", id, err)
	}

	if u.Scheme != "http" {
		return Provider{}, fmt.Errorf("%s redirect uri must use http: %q", id, redirectURI)
	}

	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return Provider{}, fmt.Errorf("%s redirect uri must target loopback: %q", id, redirectURI)
	}

	port := u.Port()
	if port == "" {
		return Provider{}, fmt.Errorf("%s redirect uri must carry an explicit port: %q", id, redirectURI)
	}

	path := u.EscapedPath()
	if path == "" {
		return Provider{}, fmt.Errorf("%s redirect uri must carry a path: %q", id, redirectURI)
	}

	return Provider{ID: id, RedirectURI: redirectURI, host: host, port: port, path: path}, nil
}

// captured is one callback observed on a listener.
type captured struct {
	url        string
	state      string
	receivedAt time.Time
}

// listener is one bound loopback callback server.
type listener struct {
	provider Provider
	server   *http.Server
	netLn    net.Listener
	addr     string
}

// Manager owns the loopback callback listeners and the transient capture of
// callback URLs they observe.
type Manager struct {
	mu        sync.RWMutex
	listeners map[string]*listener
	captured  map[string]captured
	providers []Provider
	now       func() time.Time
}

// Providers resolves the provider set a Manager should serve. Tests provide
// their own value; production uses BuiltinProviders.
type Providers []Provider

// BuiltinProviders is the production provider set.
var BuiltinProviders = fx.Annotate(
	func() Providers { return DefaultProviders() },
	fx.ResultTags(`name:"oauth_callback_providers"`),
)

// Params configures a Manager. The listeners are started and stopped with the
// server lifecycle, so they are only bound while AxonHub is serving.
type Params struct {
	fx.In

	Lifecycle fx.Lifecycle

	// Enabled mirrors server.oauth_callback.enabled.
	Enabled bool `name:"oauth_callback_enabled"`

	// Providers carries the callback providers to serve. It is named so a test
	// can supply its own set without touching the production wiring.
	Providers Providers `name:"oauth_callback_providers"`
}

// NewManager builds the manager and binds its listeners with the server
// lifecycle.
func NewManager(params Params) *Manager {
	m := &Manager{
		listeners: make(map[string]*listener, len(params.Providers)),
		captured:  make(map[string]captured, len(params.Providers)),
		providers: params.Providers,
		now:       time.Now,
	}

	if !params.Enabled {
		log.Info(context.Background(), "oauth callback listeners disabled by configuration")

		return m
	}

	params.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			m.Start(context.Background())

			return nil
		},
		OnStop: func(ctx context.Context) error {
			return m.Stop(ctx)
		},
	})

	return m
}

// Start binds every configured listener. Binding is best effort: a port that is
// already in use is logged and skipped, leaving the manual paste flow intact.
func (m *Manager) Start(ctx context.Context) {
	for _, p := range m.providers {
		if err := m.ensureStarted(ctx, p); err != nil {
			log.Warn(ctx, "oauth callback listener unavailable",
				log.String("provider", p.ID),
				log.String("redirect_uri", p.RedirectURI),
				log.Cause(err),
			)
		}
	}
}

// Stop shuts down every bound listener.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	listeners := m.listeners
	m.listeners = make(map[string]*listener, len(m.providers))
	m.mu.Unlock()

	var errs []error

	for id, l := range listeners {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)

		if err := l.server.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown %s callback listener: %w", id, err))
		}

		cancel()
	}

	return errors.Join(errs...)
}

// ensureStarted binds one listener if it is not already bound.
func (m *Manager) ensureStarted(ctx context.Context, p Provider) error {
	m.mu.RLock()
	_, ok := m.listeners[p.ID]
	m.mu.RUnlock()

	if ok {
		return nil
	}

	ln, addr, err := listenLoopback(p)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           m.handler(p),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          stdlog.New(io.Discard, "", 0),
	}

	l := &listener{provider: p, server: srv, netLn: ln, addr: addr}

	m.mu.Lock()
	m.listeners[p.ID] = l
	m.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error(ctx, "oauth callback listener panicked",
					log.String("provider", p.ID),
					log.Any("panic", r),
				)
			}
		}()

		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn(ctx, "oauth callback listener stopped",
				log.String("provider", p.ID),
				log.Cause(err),
			)
		}
	}()

	log.Info(ctx, "oauth callback listener started",
		log.String("provider", p.ID),
		log.String("addr", addr),
		log.String("path", p.path),
	)

	return nil
}

// listenLoopback binds the provider's loopback address, falling back to IPv4
// loopback when the hostname form does not bind.
func listenLoopback(p Provider) (net.Listener, string, error) {
	addr := net.JoinHostPort(p.host, p.port)

	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, addr, nil
	}

	if p.host == "127.0.0.1" {
		return nil, "", err
	}

	fallback := net.JoinHostPort("127.0.0.1", p.port)
	if ln, fallbackErr := net.Listen("tcp", fallback); fallbackErr == nil {
		return ln, fallback, nil
	}

	return nil, "", err
}

// ProviderStatus describes one provider's callback listener.
type ProviderStatus struct {
	// Provider is the console provider key.
	Provider string

	// Active reports whether the loopback listener is bound.
	Active bool

	// RedirectURI is the address the provider redirects the browser to.
	RedirectURI string
}

// ProviderStatuses reports the availability of every configured listener.
func (m *Manager) ProviderStatuses() []ProviderStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]ProviderStatus, 0, len(m.providers))

	for _, p := range m.providers {
		_, active := m.listeners[p.ID]

		out = append(out, ProviderStatus{
			Provider:    p.ID,
			Active:      active,
			RedirectURI: p.RedirectURI,
		})
	}

	return out
}

// Active reports whether the provider's listener is bound.
func (m *Manager) Active(providerID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, ok := m.listeners[providerID]

	return ok
}

// Poll returns a fresh captured callback URL for the provider. When sessionID is
// non-empty and the capture carries a state, the two must match so that one
// console session cannot claim another's authorization.
func (m *Manager) Poll(providerID, sessionID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.captured[providerID]
	if !ok {
		return "", false
	}

	if m.now().Sub(c.receivedAt) > captureTTL {
		delete(m.captured, providerID)

		return "", false
	}

	if c.state != "" && sessionID != "" && c.state != sessionID {
		return "", false
	}

	return c.url, true
}

// Discard drops any captured callback for the provider.
func (m *Manager) Discard(providerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.captured, providerID)
}

// handler routes requests for one provider's listener.
func (m *Manager) handler(p Provider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == CapturePath:
			m.handleCapture(p, w, r)
		case r.Method == http.MethodGet && r.URL.Path == p.path:
			m.handleRedirect(p, w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// handleRedirect records the callback observed on the redirect target and
// renders the result page.
func (m *Manager) handleRedirect(p Provider, w http.ResponseWriter, r *http.Request) {
	// Build the URL from the provider's own fixed callback address rather than
	// the request Host header: the value is handed to the console and submitted
	// upstream, so a forged Host must not be able to redirect it elsewhere.
	full := "http://" + net.JoinHostPort(p.host, p.port) + r.URL.RequestURI()
	if len(full) > maxCallbackURLLen {
		writeResultPage(w, false, "The callback URL is too long to process.")

		return
	}

	query := r.URL.Query()
	if errParam := query.Get("error"); errParam != "" {
		desc := query.Get("error_description")
		if desc == "" {
			desc = errParam
		}

		writeResultPage(w, false, "Authorization failed: "+desc)

		return
	}

	if query.Get("code") == "" {
		writeResultPage(w, false, "Authorization failed: no code in the callback.")

		return
	}

	// The fragment is not sent to the server, so state may be missing here.
	// The result page posts it back through /_capture.
	m.record(p.ID, full, query.Get("state"))

	log.Info(r.Context(), "captured oauth callback",
		log.String("provider", p.ID),
		log.Bool("carries_state", query.Get("state") != ""),
	)

	writeResultPage(w, true, "Authorization received. You can close this window.")
}

// handleCapture accepts the fragment-bearing callback URL from the result page.
func (m *Manager) handleCapture(p Provider, w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxCaptureBody+1))
	if err != nil || len(body) > maxCaptureBody {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	var payload struct {
		URL string `json:"url"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	raw := strings.TrimSpace(payload.URL)
	if !m.acceptable(p, raw) {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	state := ""
	if u, err := url.Parse(raw); err == nil {
		state = u.Query().Get("state")
		if state == "" {
			state = u.Fragment
		}
	}

	m.record(p.ID, raw, state)

	w.WriteHeader(http.StatusNoContent)
}

// acceptable reports whether a posted URL looks like this provider's callback.
func (m *Manager) acceptable(p Provider, raw string) bool {
	if raw == "" || len(raw) > maxCallbackURLLen {
		return false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return false
	}

	if u.Scheme != "http" {
		return false
	}

	if u.Port() != p.port {
		return false
	}

	return u.EscapedPath() == p.path
}

// record stores one captured callback, replacing any previous capture.
func (m *Manager) record(providerID, callbackURL, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.captured[providerID] = captured{
		url:        callbackURL,
		state:      state,
		receivedAt: m.now(),
	}
}
