package chaos

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the tariff bands are Europe/Paris wall-clock times

	"github.com/jackc/pgx/v5"
)

// Round 3: a movable clock (a -tags chaosclock gateway), a spool on a tiny or a
// slow filesystem, and a PostgreSQL that turns read-only.

// startClockOffset is where a scenario starts its gateway's clock, as an offset
// from real time at now.
//
//	prev-month-20th: noon UTC on the 20th of the month that has just ended, so
//	  that the traffic is stamped in a month that can be closed once the clock is
//	  back to real time: a closed month WITH content;
//	tariff-edge: 15 s before the next 23:00 in Europe/Paris, where the tariff
//	  changes from "peak" to "night".
func startClockOffset(kind string, now time.Time) time.Duration {
	switch kind {
	case "prev-month-20th":
		u := now.UTC()
		t := time.Date(u.Year(), u.Month(), 1, 12, 0, 0, 0, time.UTC).AddDate(0, -1, 19)
		return t.Sub(now)
	case "tariff-edge":
		loc, err := time.LoadLocation("Europe/Paris")
		if err != nil {
			return 0
		}
		p := now.In(loc)
		t := time.Date(p.Year(), p.Month(), p.Day(), 22, 59, 45, 0, loc)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t.Sub(now)
	}
	return 0
}

func (w *World) setClock(off time.Duration) error {
	w.mu.Lock()
	w.clockOff = off
	w.mu.Unlock()
	for _, g := range w.GW {
		if err := g.SetClock(off); err != nil {
			return err
		}
	}
	return nil
}

func (w *World) pgExec(ctx context.Context, sql ...string) error {
	c, err := pgx.Connect(ctx, w.pgAdmin)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	for _, s := range sql {
		if _, err := c.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	// an NTP step or a DST change: the gateway's clock moves by Arg seconds
	faultTable["clock-step"] = func(w *World, f FaultAt) (func(), error) {
		w.mu.Lock()
		off := w.clockOff
		w.mu.Unlock()
		if !w.Sc.needs("clock") {
			return nil, fmt.Errorf("no movable clock in this scenario")
		}
		return nil, w.setClock(off + time.Duration(f.Arg*float64(time.Second)))
	}
	// the clock is real time again
	faultTable["clock-real"] = func(w *World, f FaultAt) (func(), error) {
		if !w.Sc.needs("clock") {
			return nil, fmt.Errorf("no movable clock in this scenario")
		}
		return nil, w.setClock(0)
	}
	// the database turns read-only (a failover to a standby, a protective
	// switch): new sessions are read-only and every open one is cut, so that
	// the gateway reconnects into it. New sessions only is what
	// default_transaction_read_only does, hence the cut.
	faultTable["pg-readonly"] = func(w *World, f FaultAt) (func(), error) {
		if w.pgAdmin == "" || !dbName.MatchString(w.pgDB) {
			return nil, fmt.Errorf("no PostgreSQL database in this scenario")
		}
		cut := fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()", w.pgDB)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := w.pgExec(ctx, "ALTER DATABASE "+w.pgDB+" SET default_transaction_read_only = on", cut); err != nil {
			return nil, err
		}
		return func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := w.pgExec(ctx, "ALTER DATABASE "+w.pgDB+" RESET default_transaction_read_only", cut); err != nil {
				w.Note("pg-readonly lift: %v", err)
			}
		}, nil
	}
}

// ---- judges ----

// slowFsyncProblems reports what slow spool writes did to the request path:
// the throughput and the 95th percentile of the requests that started inside
// [from, to]. What is JUDGED is invariant 2 (no answered request unbilled): a
// gateway may protect the records by slowing its admissions (v0.39.0: 5 % of the
// throughput, seen in CI) or lose them (v0.38.1: a third of them), and only the
// loss is a failure. The probe fails only when the run answered nothing at all.
func slowFsyncProblems(outs []Outcome, from, to time.Time) (problems, notes []string) {
	var d []time.Duration
	answered := 0
	for _, o := range outs {
		if o.Left || o.At.Before(from) || o.At.After(to) {
			continue
		}
		d = append(d, o.Dur)
		if o.Status == 200 {
			answered++
		}
	}
	if answered == 0 {
		return []string{"slow-fsync: no request was answered while the spool's writes were slow: the probe proves nothing"}, nil
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	p95 := d[len(d)*95/100]
	rate := float64(len(d)) / max(1, to.Sub(from).Seconds())
	return nil, []string{fmt.Sprintf("slow-fsync: %.0f requests/s completed and a p95 of %s while the spool's writes were slow (%d answered)", rate, p95.Round(time.Millisecond), answered)}
}

// spoolFullProblems is the probe's guard for a run whose spool filled up (a
// tiny filesystem) while PostgreSQL hung. What is judged is invariant 2: every
// answered request billed exactly once, once PostgreSQL is back, so none was
// lost with a spool that could not take it. The gateway may hold the records in
// memory (bounded) or refuse 503 for the spool; both are reported, neither is
// required. The probe only asks that the run answered something, and that the
// spool really was out of room: the gateway's log says so ("accounting_records"
// or "spool" errors), read by the caller.
func spoolFullProblems(outs []Outcome, sawNoSpace bool) (problems, notes []string) {
	var refused, served int
	for _, o := range outs {
		switch {
		case o.Status == 503 && strings.Contains(o.Body, "spool"):
			refused++
		case o.Status == 200:
			served++
		}
	}
	switch {
	case served < 50:
		return []string{fmt.Sprintf("spool-full: only %d requests were answered: the probe proves nothing", served)}, nil
	case !sawNoSpace:
		return []string{"spool-full: the gateway never reported the spool out of room: the filesystem did not fill (the probe proves nothing)"}, nil
	}
	return nil, []string{fmt.Sprintf("spool-full: the spool ran out of room; %d requests refused 503 for the spool, %d answered", refused, served)}
}

func (w *World) spoolFullProbe() (problems, notes []string) {
	if !w.Sc.expects("spool-full") {
		return nil, nil
	}
	// the gateway logs the failed spool write ("no space left on device")
	saw := false
	for _, g := range w.GW {
		l := strings.ToLower(g.Log())
		saw = saw || strings.Contains(l, "no space left") || strings.Contains(l, "spool")
	}
	return spoolFullProblems(w.Ledger.All(), saw)
}

func (w *World) slowFsyncProbe() (problems, notes []string) {
	if !w.Sc.expects("slow-fsync") {
		return nil, nil
	}
	w.mu.Lock()
	var from, to time.Time
	for _, sp := range w.spans {
		if sp.Kind == "pg-hang" {
			from, to = sp.From, sp.To
		}
	}
	w.mu.Unlock()
	if from.IsZero() {
		return []string{"slow-fsync: PostgreSQL never hung"}, nil
	}
	return slowFsyncProblems(w.Ledger.All(), from, to)
}
