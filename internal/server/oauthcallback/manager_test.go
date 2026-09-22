package oauthcallback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		id          string
		redirectURI string
		wantErr     bool
		wantHost    string
		wantPort    string
		wantPath    string
	}{
		{
			name:        "codex",
			id:          ProviderCodex,
			redirectURI: "http://localhost:1455/auth/callback",
			wantHost:    "localhost",
			wantPort:    "1455",
			wantPath:    "/auth/callback",
		},
		{
			name:        "claude code",
			id:          ProviderClaudeCode,
			redirectURI: "http://localhost:54545/callback",
			wantHost:    "localhost",
			wantPort:    "54545",
			wantPath:    "/callback",
		},
		{
			name:        "antigravity",
			id:          ProviderAntigravity,
			redirectURI: "http://localhost:51121/oauth-callback",
			wantHost:    "localhost",
			wantPort:    "51121",
			wantPath:    "/oauth-callback",
		},
		{
			name:        "xai uses ipv4 loopback",
			id:          ProviderXAI,
			redirectURI: "http://127.0.0.1:56121/callback",
			wantHost:    "127.0.0.1",
			wantPort:    "56121",
			wantPath:    "/callback",
		},
		{
			name:        "rejects https",
			id:          "x",
			redirectURI: "https://localhost:1455/auth/callback",
			wantErr:     true,
		},
		{
			name:        "rejects non-loopback host",
			id:          "x",
			redirectURI: "http://example.com:1455/auth/callback",
			wantErr:     true,
		},
		{
			name:        "rejects missing port",
			id:          "x",
			redirectURI: "http://localhost/auth/callback",
			wantErr:     true,
		},
		{
			name:        "rejects missing path",
			id:          "x",
			redirectURI: "http://localhost:1455",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseProvider(tt.id, tt.redirectURI)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseProvider(%q, %q) expected error, got nil", tt.id, tt.redirectURI)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseProvider(%q, %q) unexpected error: %v", tt.id, tt.redirectURI, err)
			}

			if got.host != tt.wantHost {
				t.Errorf("host = %q, want %q", got.host, tt.wantHost)
			}

			if got.port != tt.wantPort {
				t.Errorf("port = %q, want %q", got.port, tt.wantPort)
			}

			if got.path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.path, tt.wantPath)
			}
		})
	}
}

func TestDefaultProvidersCoverEverySubscriptionProvider(t *testing.T) {
	t.Parallel()

	providers := DefaultProviders()
	if len(providers) != 4 {
		t.Fatalf("DefaultProviders() returned %d providers, want 4", len(providers))
	}

	seen := make(map[string]string, len(providers))
	for _, p := range providers {
		seen[p.ID] = p.RedirectURI
	}

	for _, id := range []string{ProviderCodex, ProviderClaudeCode, ProviderAntigravity, ProviderXAI} {
		if _, ok := seen[id]; !ok {
			t.Errorf("DefaultProviders() is missing provider %q", id)
		}
	}

	if got, want := seen[ProviderXAI], "http://127.0.0.1:56121/callback"; got != want {
		t.Errorf("xai redirect uri = %q, want %q", got, want)
	}
}

// newTestManager builds a manager whose listener binds an ephemeral port, so
// tests never contend for the provider-fixed ports.
func newTestManager(t *testing.T, providerID string) *Manager {
	t.Helper()

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}

	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()

	p, err := ParseProvider(providerID, fmt.Sprintf("http://127.0.0.1:%d/callback", port))
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}

	m := &Manager{
		listeners: make(map[string]*listener),
		captured:  make(map[string]captured),
		providers: []Provider{p},
		now:       time.Now,
	}

	if err := m.ensureStarted(context.Background(), p); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}

	t.Cleanup(func() {
		if err := m.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	return m
}

// addr exposes the bound address of the single test listener.
func (m *Manager) addr() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, l := range m.listeners {
		return l.addr
	}

	return ""
}

func TestManagerCapturesRedirectAndCapturePost(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, ProviderCodex)
	base := "http://" + m.addr() + "/callback"

	client := &probeClient{}

	// A provider that puts state in the query string is captured straight from
	// the redirect.
	queryURL := base + "?code=abc&state=sess-1"

	if err := client.get(queryURL); err != nil {
		t.Fatalf("get %s: %v", queryURL, err)
	}

	got, ok := m.Poll(ProviderCodex, "sess-1")
	if !ok {
		t.Fatal("Poll() after redirect = not ready, want ready")
	}

	if got != queryURL {
		t.Errorf("Poll() = %q, want %q", got, queryURL)
	}

	// A mismatched session must not claim the capture.
	if _, ok := m.Poll(ProviderCodex, "other-session"); ok {
		t.Error("Poll(other-session) = ready, want not ready")
	}

	// Claude Code style: state only in the fragment, delivered by CapturePath.
	m.Discard(ProviderCodex)

	fragmentURL := base + "?code=xyz#sess-2"

	status, err := client.postCapture("http://"+m.addr()+CapturePath, fragmentURL)
	if err != nil {
		t.Fatalf("postCapture: %v", err)
	}

	if status != http.StatusNoContent {
		t.Fatalf("postCapture status = %d, want %d", status, http.StatusNoContent)
	}

	got, ok = m.Poll(ProviderCodex, "sess-2")
	if !ok {
		t.Fatal("Poll() after capture post = not ready, want ready")
	}

	if got != fragmentURL {
		t.Errorf("Poll() = %q, want %q", got, fragmentURL)
	}
}

func TestManagerRejectsForeignCallbackURL(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, ProviderCodex)
	p := m.providers[0]
	addr := m.addr()

	// The posted URL must belong to this provider's own port and path.
	if m.acceptable(p, "http://localhost:9999/callback?code=a") {
		t.Error("acceptable() accepted a callback on a different port")
	}

	if m.acceptable(p, "http://"+addr+"/other?code=a") {
		t.Error("acceptable() accepted a callback on a different path")
	}

	if m.acceptable(p, "https://"+addr+"/callback?code=a") {
		t.Error("acceptable() accepted a non-http scheme")
	}

	if m.acceptable(p, "") {
		t.Error("acceptable() accepted an empty url")
	}

	client := &probeClient{}
	status, err := client.postCapture("http://"+addr+CapturePath, "not-a-url")
	if err != nil {
		t.Fatalf("postCapture: %v", err)
	}

	if status != http.StatusBadRequest {
		t.Errorf("postCapture status for an invalid url = %d, want %d", status, http.StatusBadRequest)
	}

	if _, ok := m.Poll(ProviderCodex, ""); ok {
		t.Error("Poll() accepted an invalid captured url")
	}

	if m.acceptable(p, "http://"+addr+"/callback?code="+strings.Repeat("a", maxCallbackURLLen+1)) {
		t.Error("acceptable() accepted an oversized url")
	}
}

func TestManagerRedirectReportsUpstreamError(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, ProviderCodex)
	addr := m.addr()
	client := &probeClient{}

	if err := client.get("http://" + addr + "/callback?error=access_denied&error_description=denied"); err != nil {
		t.Fatalf("get: %v", err)
	}

	if _, ok := m.Poll(ProviderCodex, ""); ok {
		t.Error("Poll() = ready after an error redirect, want not ready")
	}

	if err := client.get("http://" + addr + "/callback?code="); err != nil {
		t.Fatalf("get: %v", err)
	}

	if _, ok := m.Poll(ProviderCodex, ""); ok {
		t.Error("Poll() = ready after a code-less redirect, want not ready")
	}
}

func TestManagerCaptureExpires(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, ProviderCodex)

	now := time.Now()
	m.now = func() time.Time { return now }

	m.record(ProviderCodex, "http://127.0.0.1:1455/callback?code=a", "")

	if _, ok := m.Poll(ProviderCodex, ""); !ok {
		t.Fatal("Poll() = not ready immediately after record, want ready")
	}

	now = now.Add(captureTTL + time.Second)

	if _, ok := m.Poll(ProviderCodex, ""); ok {
		t.Error("Poll() = ready after the capture TTL, want expired")
	}
}

func TestManagerStartIsBestEffortWhenPortIsTaken(t *testing.T) {
	t.Parallel()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() { _ = occupied.Close() }()

	port := occupied.Addr().(*net.TCPAddr).Port

	p, err := ParseProvider(ProviderCodex, fmt.Sprintf("http://127.0.0.1:%d/auth/callback", port))
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}

	m := &Manager{
		listeners: make(map[string]*listener),
		captured:  make(map[string]captured),
		providers: []Provider{p},
		now:       time.Now,
	}

	// Start must not panic or block when the port is unavailable.
	m.Start(context.Background())

	if m.Active(ProviderCodex) {
		t.Error("Active() = true for a port that is already taken, want false")
	}

	// The status surface must still describe the provider, marked inactive, so
	// the console can fall back to the paste flow.
	statuses := m.ProviderStatuses()
	if len(statuses) != 1 {
		t.Fatalf("ProviderStatuses() returned %d entries, want 1", len(statuses))
	}

	if statuses[0].Active {
		t.Error("ProviderStatuses()[0].Active = true, want false")
	}

	if statuses[0].RedirectURI == "" {
		t.Error("ProviderStatuses()[0].RedirectURI is empty, want the redirect uri")
	}
}

func TestCSPHashesAreBase64SHA256(t *testing.T) {
	t.Parallel()

	for name, hash := range map[string]string{"style": styleHash, "script": scriptHash} {
		raw, err := base64.StdEncoding.DecodeString(hash)
		if err != nil {
			t.Errorf("%s hash is not valid base64: %v", name, err)

			continue
		}

		if len(raw) != sha256.Size {
			t.Errorf("%s hash decoded to %d bytes, want %d", name, len(raw), sha256.Size)
		}
	}
}

func TestResultPageSetsHardeningHeaders(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeResultPage(w, true, "done")

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q, want default-src 'none'", csp)
	}

	if !strings.Contains(csp, "sha256-"+styleHash) || !strings.Contains(csp, "sha256-"+scriptHash) {
		t.Errorf("Content-Security-Policy = %q, want both inline hashes", csp)
	}

	if !strings.Contains(w.Body.String(), `data-autoclose="1"`) {
		t.Error("success page should request auto close")
	}

	w = httptest.NewRecorder()
	writeResultPage(w, false, "failed")

	if !strings.Contains(w.Body.String(), `data-autoclose="0"`) {
		t.Error("failure page should not auto close")
	}

	// Messages are escaped, never reflected raw.
	w = httptest.NewRecorder()
	writeResultPage(w, false, "<script>alert(1)</script>")

	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
		t.Error("failure message was reflected without escaping")
	}
}

func TestCallbackCaptureIsTransient(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, ProviderCodex)

	m.record(ProviderCodex, "http://127.0.0.1:1455/callback?code=secret", "")

	// The capture lives only in memory, keyed per provider.
	if len(m.captured) != 1 {
		t.Fatalf("captured map has %d entries, want 1", len(m.captured))
	}

	m.Discard(ProviderCodex)

	if len(m.captured) != 0 {
		t.Errorf("captured map has %d entries after Discard, want 0", len(m.captured))
	}
}

// probeClient exercises the loopback listeners over real HTTP.
type probeClient struct{}

func (c *probeClient) get(rawURL string) error {
	resp, err := http.Get(rawURL)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}

	return nil
}

func (c *probeClient) postCapture(rawURL, callbackURL string) (int, error) {
	body, err := json.Marshal(map[string]string{"url": callbackURL})
	if err != nil {
		return 0, err
	}

	resp, err := http.Post(rawURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode, nil
}
