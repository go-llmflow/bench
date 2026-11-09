package chaos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// Operations injected during traffic (PostgreSQL scenarios: CI).

// prevMonth is the month that has just ended, the only kind that can be closed.
func prevMonth() (key, from, to string) { return prevMonthAt(time.Now()) }

// gwNow is the time the gateways believe it is.
func (w *World) gwNow() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Now().Add(w.clockOff)
}

func (w *World) prevMonth() (key, from, to string) { return prevMonthAt(w.gwNow()) }

// prevMonthAt is the month that has just ended at t.
func prevMonthAt(t time.Time) (key, from, to string) {
	n := t.UTC()
	first := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	p := first.AddDate(0, -1, 0)
	return p.Format("2006-01"), p.Format("2006-01-02"), first.Format("2006-01-02")
}

// closedView is what a reader sees of a month: the fluids report and the spend
// report over it.
func (w *World) closedView() (string, error) {
	_, from, to := w.prevMonth()
	var out bytes.Buffer
	for _, q := range []string{"/admin/fluids?group_by=org,pool&from=" + from + "&to=" + to, "/admin/spend?group_by=org,team&from=" + from + "&to=" + to} {
		code, b, err := w.Primary().Admin("GET", q, "")
		if err != nil || code != 200 {
			return "", fmt.Errorf("GET %s: %d %v %s", q, code, err, b)
		}
		out.Write(b)
	}
	return out.String(), nil
}

func init() {
	faultTable["month-close"] = func(w *World, f FaultAt) (func(), error) {
		m, _, _ := w.prevMonth()
		code, b, err := w.Primary().Admin("POST", "/admin/billing/close?month="+m, "")
		if err != nil {
			return nil, err
		}
		if code != 200 && code != 201 {
			return nil, fmt.Errorf("closing %s: %d %s", m, code, b)
		}
		view, err := w.closedView()
		if err != nil {
			return nil, err
		}
		spent, err := w.monthSpend()
		if err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.closed = &closedMonth{Month: m, View: view, Spend: spent}
		w.mu.Unlock()
		return nil, nil
	}
	faultTable["migrate-users"] = func(w *World, f FaultAt) (func(), error) {
		// Users created through the API are unscoped; migrate-users renames them
		// to name@domain and everything keyed by the name (the budgets of a
		// person), against the same database, while traffic goes on.
		for _, name := range []string{"mig-a", "mig-b"} {
			if code, b, err := w.Primary().Admin("POST", "/admin/users", fmt.Sprintf(`{"name":%q}`, name)); err != nil || code != 201 {
				return nil, fmt.Errorf("create user %s: %d %v %s", name, code, err, b)
			}
		}
		out, err := exec.Command(w.Opt.GatewayBin, "migrate-users", "--config", w.Primary().ConfigPath(), "--apply").CombinedOutput()
		w.Note("migrate-users --apply: err=%v output=%.300s", err, string(out))
		if err != nil {
			return nil, fmt.Errorf("migrate-users: %v: %s", err, out)
		}
		return nil, nil
	}
}

type closedMonth struct {
	Month, View string
	Spend       float64 // the month's spend when it was closed
}

// monthSpend is the total cost the spend report holds over the month that has
// just ended.
func (w *World) monthSpend() (float64, error) {
	_, from, to := w.prevMonth()
	code, b, err := w.Primary().Admin("GET", "/admin/spend?group_by=org&from="+from+"&to="+to, "")
	if err != nil || code != 200 {
		return 0, fmt.Errorf("spend of the closed month: %d %v %s", code, err, b)
	}
	var out struct {
		Rows []SpendRow `json:"rows"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return 0, err
	}
	var t float64
	for _, r := range out.Rows {
		t += r.Cost
	}
	return t, nil
}

// closedContentProblem: a scenario that closes a month WITH content (it expects
// "month-content") must have closed a month that holds money; otherwise the
// immutability it judged was the immutability of an empty report.
func closedContentProblem(expect bool, spend float64) string {
	if expect && spend <= 0 {
		return "the closed month held no spend: invariant 5 judged an empty month (the clock did not put the traffic in it)"
	}
	return ""
}

// month-close judgement: invariant 5.
func inv5Closed(w *World) Verdict {
	w.mu.Lock()
	c := w.closed
	w.mu.Unlock()
	if c == nil {
		return verdict(5, NA, "no scenario step closed a month")
	}
	if p := closedContentProblem(w.Sc.expects("month-content"), c.Spend); p != "" {
		return verdict(5, Fail, "%s", p)
	}
	after, err := w.closedView()
	if err != nil {
		return verdict(5, Fail, "cannot read the closed month %s: %v", c.Month, err)
	}
	if after != c.View {
		return verdict(5, Fail, "the closed month %s changed under traffic: its reports differ before and after", c.Month)
	}
	code, _, err := w.Primary().Admin("POST", "/admin/billing/close?month="+c.Month, "")
	if err != nil || code != 409 {
		return verdict(5, Fail, "closing %s again answered %d (%v), want 409: a closed month cannot be reopened", c.Month, code, err)
	}
	return verdict(5, Pass, "month %s closed during traffic (spend %.3f in it): reports byte-identical afterwards, a second close refused with 409", c.Month, c.Spend)
}
