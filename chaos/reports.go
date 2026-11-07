package chaos

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SpendRow is one row of GET /admin/spend.
type SpendRow struct {
	Org               string  `json:"org"`
	Team              string  `json:"team"`
	Key               string  `json:"key"`
	User              string  `json:"user"`
	Requests          int64   `json:"requests"`
	PromptTokens      int64   `json:"prompt_tokens"`
	CompletionTokens  int64   `json:"completion_tokens"`
	Cost              float64 `json:"cost"`
	EstimatedRequests int64   `json:"estimated_requests"`
}

func window() (string, string) {
	d := time.Now().UTC()
	return d.AddDate(0, 0, -1).Format("2006-01-02"), d.AddDate(0, 0, 2).Format("2006-01-02")
}

func (w *World) spend(dims string) ([]SpendRow, error) {
	from, to := window()
	code, b, err := w.Primary().Admin("GET", "/admin/spend?group_by="+dims+"&from="+from+"&to="+to, "")
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("spend: %d %s", code, b)
	}
	var out struct {
		Rows []SpendRow `json:"rows"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}

func (w *World) spendTotal() (float64, error) {
	rows, err := w.spend("org")
	if err != nil {
		return 0, err
	}
	var t float64
	for _, r := range rows {
		t += r.Cost + float64(r.Requests)
	}
	return t, nil
}

// BudgetRow is one row of GET /admin/budgets.
type BudgetRow struct {
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	MaxCost   float64 `json:"max_cost"`
	Limit     float64 `json:"limit"`
	Spent     float64 `json:"spent"`
	Committed float64 `json:"committed"`
	Held      float64 `json:"held"`
	Arming    bool    `json:"arming"`
}

func (w *World) budgets() ([]BudgetRow, error) {
	code, b, err := w.Primary().Admin("GET", "/admin/budgets", "")
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("budgets: %d %s", code, b)
	}
	var out []BudgetRow
	return out, json.Unmarshal(b, &out)
}

// FluidsRow is one row of GET /admin/fluids.
type FluidsRow struct {
	Org        string  `json:"org"`
	Pool       string  `json:"pool"`
	GPUHours   float64 `json:"gpu_hours"`
	KWhActive  float64 `json:"kwh_active"`
	KWhIdle    float64 `json:"kwh_idle"`
	KWh        float64 `json:"kwh"`
	FluidsCost float64 `json:"fluids_cost"`
	FixedCost  float64 `json:"fixed_cost"`
	Credit     float64 `json:"credit"`
	Markup     float64 `json:"markup"`
	TotalCost  float64 `json:"total_cost"`
	NetworkCst float64 `json:"network_cost"`
	StorageCst float64 `json:"storage_cost"`
}

// FluidsOut is the fluids report.
type FluidsOut struct {
	Rows         []FluidsRow `json:"rows"`
	NodeOverhead []struct {
		Month     string  `json:"month"`
		Pool      string  `json:"pool"`
		Source    string  `json:"source"`
		Intervals int     `json:"intervals"`
		KWh       float64 `json:"kwh"`
	} `json:"node_overhead"`
	ClosedMonths []string        `json:"closed_months"`
	Raw          json.RawMessage `json:"-"`
}

func (w *World) fluids(groupBy string) (FluidsOut, error) {
	from, to := window()
	code, b, err := w.Primary().Admin("GET", "/admin/fluids?group_by="+groupBy+"&from="+from+"&to="+to, "")
	if err != nil {
		return FluidsOut{}, err
	}
	if code != 200 {
		return FluidsOut{}, fmt.Errorf("fluids: %d %s", code, b)
	}
	var out FluidsOut
	out.Raw = b
	return out, json.Unmarshal(b, &out)
}

// gatewayMetrics reads the gateway's own /metrics (loopback is allowed).
func (w *World) gatewayMetrics() (string, error) {
	return httpGet(w.Primary().URL + "/metrics")
}

func httpGet(u string) (string, error) {
	resp, err := adminClient.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return string(b), fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	return string(b), nil
}

// metricValue returns the sum of the samples of a metric in an exposition.
func metricValue(text, name string) (float64, bool) {
	var sum float64
	found := false
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, name) {
			continue
		}
		rest := l[len(name):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '{') {
			continue
		}
		f := strings.Fields(l)
		var v float64
		if _, err := fmt.Sscan(f[len(f)-1], &v); err == nil {
			sum += v
			found = true
		}
	}
	return sum, found
}
