package chaos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The evidence of a run is written next to its configuration, in the run's
// durable directory: what the clients saw (ledger.jsonl), what the upstream
// witnessed (upstream.jsonl), the gateway's reports at the end, and its logs.
// A failing invariant is reduced to a gateway test from these.

// One directory per seed: a later seed must not overwrite the evidence of the
// seed that failed (CI uploads them all).
func (w *World) evDir() string {
	return filepath.Join(w.Opt.WorkDir, w.Sc.Name, fmt.Sprintf("evidence-seed%d", w.Opt.Seed))
}

func (w *World) writeJSONL(name string, rows []any) {
	if err := os.MkdirAll(w.evDir(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(w.evDir(), name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rows {
		_ = enc.Encode(r)
	}
}

func (w *World) dumpEvidence() {
	var rows []any
	for _, o := range w.Ledger.All() {
		// the dispatcher's tokens are credentials: never written.
		o.Cred.Bearer = ""
		rows = append(rows, o)
	}
	w.writeJSONL("ledger.jsonl", rows)
	rows = nil
	for _, s := range w.Up.Seen() {
		rows = append(rows, s)
	}
	w.writeJSONL("upstream.jsonl", rows)
	if f, err := w.fluids("org,pool"); err == nil {
		w.writeRaw("fluids.json", f.Raw)
	}
	if sp, err := w.spend("org,team,key"); err == nil {
		b, _ := json.MarshalIndent(sp, "", " ")
		w.writeRaw("spend.json", b)
	}
	if b, err := w.budgets(); err == nil {
		bb, _ := json.MarshalIndent(b, "", " ")
		w.writeRaw("budgets.json", bb)
	}
	w.sampleRefusals()
}

func (w *World) writeRaw(name string, b []byte) {
	if err := os.MkdirAll(w.evDir(), 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(w.evDir(), name), b, 0o600)
	}
}

func (w *World) dumpLogs() {
	for i, g := range w.GW {
		w.writeRaw(fmt.Sprintf("gateway-r%d.log", i), []byte(g.Log()))
	}
}

// sampleRefusals notes one example of each distinct refusal, with the headers
// a client would read: the evidence for invariant 6.
func (w *World) sampleRefusals() {
	seen := map[string]bool{}
	var keys []string
	samples := map[string]string{}
	for _, o := range w.Ledger.All() {
		if o.Status < 400 || o.Left {
			continue
		}
		k := fmt.Sprintf("%d %.60s", o.Status, firstLine(o.Body))
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
		samples[k] = fmt.Sprintf("refusal sample: %d Retry-After=%q X-Llmflow-Budget-Reset=%q body=%.200s", o.Status, o.RetryAfter, o.Header.Get("X-Llmflow-Budget-Reset"), o.Body)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i >= 5 {
			break
		}
		w.Note("%s", samples[k])
	}
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
