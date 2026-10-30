package fakeupstream

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

// DCGM is a fake NVIDIA DCGM exporter: it serves
// DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION, the counter "since the driver was
// last reloaded (in mJ)" of dcgm-exporter's etc/default-counters.csv, in the
// Prometheus text format, one sample per GPU, and the power gauge next to it.
// The test moves the counters (Add), reloads a driver (Reset) and takes the
// exporter down (SetDown) to check what a gateway's fluids accounting does.
type DCGM struct {
	mu    sync.Mutex
	mj    map[string]uint64
	down  bool
	hits  atomic.Int64
	added uint64 // everything Add put in, for the test's arithmetic
}

// NewDCGM returns an exporter with the given GPU UUIDs, counters at 0.
func NewDCGM(gpus ...string) *DCGM {
	d := &DCGM{mj: map[string]uint64{}}
	for _, g := range gpus {
		d.mj[g] = 0
	}
	return d
}

// Add moves a GPU's counter forward by mj millijoules.
func (d *DCGM) Add(gpu string, mj uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mj[gpu] += mj
	d.added += mj
}

// Reset reloads a GPU's driver: its counter restarts at mj.
func (d *DCGM) Reset(gpu string, mj uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mj[gpu] = mj
}

// SetDown makes the exporter answer 503, as a node in reboot would.
func (d *DCGM) SetDown(down bool) {
	d.mu.Lock()
	d.down = down
	d.mu.Unlock()
}

// Hits is the number of scrapes received, down ones included.
func (d *DCGM) Hits() int64 { return d.hits.Load() }

// Added is the energy Add put in since the start, in millijoules.
func (d *DCGM) Added() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.added
}

// ServeHTTP answers GET /metrics.
func (d *DCGM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.hits.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down || r.URL.Path != "/metrics" {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	gpus := make([]string, 0, len(d.mj))
	for g := range d.mj {
		gpus = append(gpus, g)
	}
	sort.Strings(gpus)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, "# HELP DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION Total energy consumption since boot (in mJ).\n# TYPE DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION counter\n")
	for i, g := range gpus {
		fmt.Fprintf(w, "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{gpu=\"%d\",UUID=%q,device=\"nvidia%d\",Hostname=\"fake\"} %d\n", i, g, i, d.mj[g])
	}
	fmt.Fprint(w, "# HELP DCGM_FI_DEV_POWER_USAGE Power draw (in W).\n# TYPE DCGM_FI_DEV_POWER_USAGE gauge\n")
	for i, g := range gpus {
		fmt.Fprintf(w, "DCGM_FI_DEV_POWER_USAGE{gpu=\"%d\",UUID=%q} 300.5\n", i, g)
	}
}
