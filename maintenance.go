// Package traefik_plugin_maintenance implements a Traefik middleware that,
// for a given service, queries an external status service (maintenance-state)
// and, if that service is marked "in maintenance", responds with an HTML page
// (modal) instead of proxying the request.
//
// IMPORTANT: this code runs inside Traefik's Yaegi interpreter, so it can only
// use Go's standard library (no third-party modules).
package traefik_plugin_maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"text/template"
	"time"
)

// Config is the configuration defined per middleware instance
// (typically via Docker labels or a Traefik dynamic config file).
type Config struct {
	// ServiceName is the identifier Jenkins will use in the webhook
	// (POST /maintenance/start {"service": "<ServiceName>"}).
	ServiceName string `json:"serviceName,omitempty"`
	// StateURL is the base URL of the maintenance-state service, e.g.
	// http://maintenance-state:8080
	StateURL string `json:"stateUrl,omitempty"`
	// PollIntervalSeconds: how often the browser re-checks (via fetch)
	// whether maintenance has finished.
	PollIntervalSeconds int `json:"pollIntervalSeconds,omitempty"`
	// CacheSeconds: how long the plugin caches the maintenance-state response
	// before querying it again, to avoid hitting that service on every request.
	CacheSeconds int `json:"cacheSeconds,omitempty"`
	// RequestTimeoutMs: timeout for the HTTP call to maintenance-state.
	RequestTimeoutMs int `json:"requestTimeoutMs,omitempty"`
	// FailOpen: if maintenance-state cannot be queried (timeout, network error,
	// etc.), true = let traffic through normally,
	// false = show the modal anyway (fail-closed).
	FailOpen bool `json:"failOpen,omitempty"`
	// Title and Message customize the modal text.
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

// CreateConfig creates the configuration with its default values.
func CreateConfig() *Config {
	return &Config{
		PollIntervalSeconds: 5,
		CacheSeconds:        3,
		RequestTimeoutMs:    1500,
		FailOpen:            true,
		Title:               "We are updating this service",
		Message:             "We'll be back shortly. This page will refresh automatically when the service is ready.",
	}
}

// statusResponse reflects the JSON returned by maintenance-state at
// /maintenance/status?service=...
type statusResponse struct {
	Service string `json:"service"`
	Active  bool   `json:"active"`
}

// cachedResult stores the last queried result, to avoid calling
// maintenance-state on every request.
type cachedResult struct {
	active    bool
	err       error
	fetchedAt time.Time
}

// Maintenance is the middleware handler.
type Maintenance struct {
	next   http.Handler
	name   string
	config *Config
	client *http.Client
	tmpl   *template.Template

	mu    sync.Mutex
	cache cachedResult
}

// New builds a new middleware instance. Traefik calls this function once for
// each middleware usage in the dynamic configuration.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.ServiceName == "" {
		return nil, fmt.Errorf("traefik-plugin-maintenance: 'serviceName' is required")
	}
	if config.StateURL == "" {
		return nil, fmt.Errorf("traefik-plugin-maintenance: 'stateUrl' is required")
	}
	if config.PollIntervalSeconds <= 0 {
		config.PollIntervalSeconds = 5
	}
	if config.CacheSeconds < 0 {
		config.CacheSeconds = 0
	}
	if config.RequestTimeoutMs <= 0 {
		config.RequestTimeoutMs = 1500
	}

	tmpl, err := template.New("maintenance").Parse(maintenanceHTML)
	if err != nil {
		return nil, fmt.Errorf("traefik-plugin-maintenance: failed to parse template: %w", err)
	}

	return &Maintenance{
		next:   next,
		name:   name,
		config: config,
		client: &http.Client{
			Timeout: time.Duration(config.RequestTimeoutMs) * time.Millisecond,
		},
		tmpl: tmpl,
	}, nil
}

// ServeHTTP decides whether to let the request through or show the
// maintenance modal.
func (m *Maintenance) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	active, err := m.checkActive()
	if err != nil {
		if m.config.FailOpen {
			// Could not query status: prioritize availability.
			m.next.ServeHTTP(rw, req)
			return
		}
		// fail-closed: show the modal anyway, to avoid exposing a backend
		// that may be mid-restart.
		m.renderModal(rw)
		return
	}

	if active {
		m.renderModal(rw)
		return
	}

	m.next.ServeHTTP(rw, req)
}

// checkActive queries (with a short cache) whether the service is in
// maintenance.
func (m *Maintenance) checkActive() (bool, error) {
	m.mu.Lock()
	if m.config.CacheSeconds > 0 && time.Since(m.cache.fetchedAt) < time.Duration(m.config.CacheSeconds)*time.Second {
		active, err := m.cache.active, m.cache.err
		m.mu.Unlock()
		return active, err
	}
	m.mu.Unlock()

	active, err := m.fetchActive()

	m.mu.Lock()
	m.cache = cachedResult{active: active, err: err, fetchedAt: time.Now()}
	m.mu.Unlock()

	return active, err
}

func (m *Maintenance) fetchActive() (bool, error) {
	url := strings.TrimRight(m.config.StateURL, "/") + "/maintenance/status?service=" + m.config.ServiceName

	resp, err := m.client.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("traefik-plugin-maintenance: unexpected status %d from %s", resp.StatusCode, url)
	}

	var sr statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return false, err
	}
	return sr.Active, nil
}

// renderModal writes the maintenance modal HTML page.
func (m *Maintenance) renderModal(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Retry-After", "5")
	rw.WriteHeader(http.StatusServiceUnavailable)

	data := struct {
		Title         string
		Message       string
		PollInterval  int
	}{
		Title:        m.config.Title,
		Message:      m.config.Message,
		PollInterval: m.config.PollIntervalSeconds,
	}

	if err := m.tmpl.Execute(rw, data); err != nil {
		// If the template fails (it shouldn't), at least return some text.
		_, _ = rw.Write([]byte(m.config.Message))
	}
}

// maintenanceHTML is the page the end user sees. It polls via fetch to the
// same URL: while the plugin keeps returning 503, it keeps waiting; as soon as
// the backend responds with another status code, it reloads the real page.
const maintenanceHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root { color-scheme: light dark; }
  body {
    margin: 0;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0f172a;
    color: #e2e8f0;
  }
  .card {
    max-width: 420px;
    margin: 24px;
    padding: 32px;
    border-radius: 16px;
    background: #1e293b;
    box-shadow: 0 20px 50px rgba(0,0,0,0.35);
    text-align: center;
  }
  .spinner {
    width: 42px;
    height: 42px;
    margin: 0 auto 20px;
    border-radius: 50%;
    border: 4px solid rgba(148, 163, 184, 0.25);
    border-top-color: #38bdf8;
    animation: spin 0.9s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  h1 { font-size: 1.25rem; margin: 0 0 10px; }
  p { font-size: 0.95rem; line-height: 1.5; color: #94a3b8; margin: 0; }
</style>
</head>
<body>
  <div class="card">
    <div class="spinner"></div>
    <h1>{{.Title}}</h1>
    <p>{{.Message}}</p>
  </div>
  <script>
    (function () {
      var intervalMs = {{.PollInterval}} * 1000;
      function check() {
        fetch(window.location.href, { method: "GET", cache: "no-store" })
          .then(function (res) {
            if (res.status !== 503) {
              window.location.reload();
            }
          })
          .catch(function () {
            // if the network fails, simply retry on the next tick
          });
      }
      setInterval(check, intervalMs);
    })();
  </script>
</body>
</html>
`
