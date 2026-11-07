package chaos

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-redfish/redfish/mockbmc"
)

// BMC is a node's Redfish BMC over TLS. The service (service root, chassis,
// authentication) is go-redfish/redfish's mockbmc; what mockbmc does not have
// is a controllable energy counter, so the chassis' EnvironmentMetrics
// resource is served here: an EnergykWh counter the scenario moves, resets,
// replaces with a power-only reading, or takes offline.
type BMC struct {
	srv  *httptest.Server
	user string
	pass string
	mu   sync.Mutex
	kwh  float64
	// total is every kWh Add put in, for the arithmetic of a scenario; a Reset
	// does not change it.
	total     float64
	watts     float64
	powerOnly bool
	offline   bool
	hits      int
}

// NewBMC starts a TLS BMC; its certificate is written to dir/bmc-ca.pem.
func NewBMC(dir, user, pass string) (*BMC, string, error) {
	m, err := mockbmc.New(mockbmc.Options{User: user, Password: pass})
	if err != nil {
		return nil, "", err
	}
	b := &BMC{user: user, pass: pass}
	inner := m.Handler()
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/EnvironmentMetrics") {
			b.metrics(w, r)
			return
		}
		b.mu.Lock()
		off := b.offline
		b.mu.Unlock()
		if off {
			http.Error(w, "BMC offline", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	ca := filepath.Join(dir, "bmc-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.srv.Certificate().Raw}), 0o600); err != nil {
		b.srv.Close()
		return nil, "", err
	}
	return b, ca, nil
}

func (b *BMC) metrics(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hits++
	if b.offline {
		http.Error(w, "BMC offline", http.StatusServiceUnavailable)
		return
	}
	if !ok || u != b.user || p != b.pass {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"@odata.id": r.URL.Path, "Id": "EnvironmentMetrics", "Name": "Environment Metrics"}
	if b.powerOnly {
		body["PowerWatts"] = map[string]any{"Reading": b.watts}
	} else {
		body["EnergykWh"] = map[string]any{"Reading": b.kwh}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// URL is the BMC's address.
func (b *BMC) URL() string { return b.srv.URL }

// Close stops it.
func (b *BMC) Close() { b.srv.Close() }

// Add moves the counter forward.
func (b *BMC) Add(kwh float64) { b.mu.Lock(); b.kwh += kwh; b.total += kwh; b.mu.Unlock() }

// Reset restarts the counter at kwh (a BMC reboot).
func (b *BMC) Reset(kwh float64) { b.mu.Lock(); b.kwh = kwh; b.mu.Unlock() }

// SetOffline makes every request fail with 503.
func (b *BMC) SetOffline(off bool) { b.mu.Lock(); b.offline = off; b.mu.Unlock() }

// SetPowerOnly makes the BMC report a power and no energy counter.
func (b *BMC) SetPowerOnly(on bool, watts float64) {
	b.mu.Lock()
	b.powerOnly, b.watts = on, watts
	b.mu.Unlock()
}

// Total is the energy Add moved in, in kWh.
func (b *BMC) Total() float64 { b.mu.Lock(); defer b.mu.Unlock(); return b.total }
