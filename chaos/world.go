package chaos

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

// Options of a run.
type Options struct {
	GatewayBin string
	WorkDir    string // durable directory of the run (never a temporary one)
	Seed       uint64
	PG         string // PostgreSQL DSN of the CI scenarios ("" locally)
	Tag        string // the gateway release under test (vX.Y.Z), "" for the latest
	// ClockBuild: the binary is a -tags chaosclock build; SmallFS, SlowFS: empty
	// directories on a tiny / a slow filesystem (see Scenario.Needs "clock", "fs", "slowfs").
	ClockBuild      bool
	SmallFS, SlowFS string
	DryRun          bool // build the fakes and the configurations, run check-config, start nothing
	Log             func(format string, a ...any)
}

// BudgetSpec is a budget the world set, with the scope's ground truth.
type BudgetSpec struct {
	Kind, Name string
	Period     string
	Hard, Soft float64
}

// Client is one caller of the gateway.
type Client struct {
	Org, Team, Who string
	Cred           Cred
	KeyID          string
	Prefix         string
	IdP            string // "" for a virtual key
	Scoped         string // the person name a JWT is attributed to, when known
}

// World is the simulated deployment of one scenario.
type World struct {
	Opt     Options
	Sc      *Scenario
	Up      *Upstream
	DCGM    *fakeupstream.DCGM
	dcgmSrv *httptest.Server
	BMC     *BMC
	IdPs    map[string]*IdP
	GW      []*Gateway
	Clients []Client
	Budgets []BudgetSpec
	Ledger  Ledger
	Notes   []string
	mu      sync.Mutex
	started time.Time
	closers []func()
	// DCGMTruth is the energy the exporter was told to move, in millijoules.
	baseGoroutines int
	baseFDs        int
	leakNotes      []string
	leakParts      []string
	// PGProxy relays the gateways' PostgreSQL traffic; HAProxy[i] relays what
	// the other replicas send to replica i.
	PGProxy *Proxy
	HAProxy []*Proxy
	spans   []faultSpan
	closed  *closedMonth
	// killCovered counts answered requests whose bill the documented loss of a
	// killed replica's queue may cover.
	killCovered int
	// fedProbes counts the harness's own probe requests (not in the ledger) that
	// went through the federation prefix and were answered or refused: the
	// dispatcher counts them, invariant 9 must too.
	fedProbes float64
	// samples of the gateway's in-flight gauges (control n-slots)
	samples []inflightSample
	// Guard is the counting fake guard model, Small the fallback model's upstream.
	satAt time.Time // when the saturate fault started
	// round 2: the relay in front of the upstream, the hostile clients' answers,
	// the schac the federation IdP states, the long run's samples
	UpProxy       *Proxy
	pgAdmin, pgDB string        // the admin DSN and the run's database (fault pg-readonly)
	clockOff      time.Duration // the gateways' clock offset now (faults clock-step, clock-real)
	adminFlood    floodStats
	fedFlood      floodStats
	schacAt       time.Time
	schacOverride string
	trends        []trendSample
	Guard         *fakeGuard
	Small         *Upstream
	// oldToken is a token signed before a hard rotation, per IdP, kept to be
	// presented afterwards.
	oldToken map[string]string
}

func osPid() int { return os.Getpid() }

// Logf writes a progress line.
func (w *World) Logf(format string, a ...any) {
	if w.Opt.Log != nil {
		w.Opt.Log(format, a...)
	}
}

// Note records a fact a verdict may cite.
func (w *World) Note(format string, a ...any) {
	w.mu.Lock()
	w.Notes = append(w.Notes, fmt.Sprintf(format, a...))
	w.mu.Unlock()
}

// Primary is the first gateway.
func (w *World) Primary() *Gateway { return w.GW[0] }

// Setup builds the fakes, the gateway(s), the keys and the budgets.
func Setup(ctx context.Context, opt Options, sc *Scenario) (*World, error) {
	w := &World{Opt: opt, Sc: sc, IdPs: map[string]*IdP{}}
	dir := filepath.Join(opt.WorkDir, sc.Name)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w.baseGoroutines = goroutines()
	w.baseFDs = fdCount(os.Getpid())
	fail := func(err error) (*World, error) { w.Close(); return nil, err }

	w.Up = NewUpstream(24)
	w.closers = append(w.closers, w.Up.Close)
	if sc.PrioritySlots > 0 {
		w.Up.SetSlots(sc.PrioritySlots)
	}
	if sc.UpstreamLatency > 0 {
		w.Up.Update(func(f *UpstreamFaults) { f.Latency = sc.UpstreamLatency })
	}
	if sc.F.ModelsDownAtStart {
		w.Up.Update(func(f *UpstreamFaults) { f.ModelsDown = true })
	}
	topo := Topology{Tag: opt.Tag, UpstreamURL: w.Up.URL(), Fluids: sc.F.Fluids, Federation: sc.F.Federation, NoModelLen: sc.F.ModelsDownAtStart}
	if sc.F.Clock != "" {
		topo.Clock = true
		w.clockOff = startClockOffset(sc.F.Clock, time.Now())
		topo.ClockOffset = w.clockOff
	}
	switch {
	case sc.F.SmallFS:
		topo.SpoolDir = filepath.Join(opt.SmallFS, sc.Name+fmt.Sprintf("-%d", opt.Seed))
	case sc.F.SlowFS:
		topo.SpoolDir = filepath.Join(opt.SlowFS, sc.Name+fmt.Sprintf("-%d", opt.Seed))
	}
	if err := w.setupControlFakes(&topo); err != nil {
		return fail(err)
	}
	if sc.F.Fluids {
		w.DCGM = fakeupstream.NewDCGM("GPU-aaa", "GPU-bbb")
		if sc.F.DCGMGlitch { // 2.6e12 mJ: 1 kW for 30 days, as in the gateway's own proof
			w.DCGM.Preload("GPU-aaa", 2_600_000_000_000)
			w.DCGM.Preload("GPU-bbb", 2_600_000_000_000)
		}
		w.dcgmSrv = httptest.NewServer(w.DCGM)
		w.closers = append(w.closers, w.dcgmSrv.Close)
		topo.DCGMURL = w.dcgmSrv.URL
	}
	if sc.F.BMC {
		b, ca, err := NewBMC(dir, "admin", "chaos-bmc-pw")
		if err != nil {
			return fail(err)
		}
		w.BMC = b
		w.closers = append(w.closers, b.Close)
		topo.BMCURL, topo.BMCCAFile, topo.BMCUser, topo.BMCPassword = b.URL(), ca, "admin", "chaos-bmc-pw"
	}
	if sc.F.IdPs {
		for _, d := range []struct{ name, org, team, domain string }{
			{"idpA", "etabA", TeamOf["etabA"], "a.example"},
			{"idpB", "etabB", TeamOf["etabB"], "b.example"},
			{"fed", Borrower, TeamOf[Borrower], ""},
		} {
			p := NewIdP(d.name, "llmflow-"+d.name)
			w.IdPs[d.name] = p
			w.closers = append(w.closers, p.Close)
			ref := IdPRef{Name: d.name, Issuer: p.Issuer(), Audience: p.Audience, Org: d.org, Team: d.team, Domain: d.domain, Legacy: d.name == "idpA" && !sc.F.SCIM && !sc.F.SharedSCIM} // a SCIM domain and a legacy domain cannot both be (config refuses two)
			if d.name == "fed" {
				ref.Accept = []string{"univ-c.example"}
			}
			topo.IdPs = append(topo.IdPs, ref)
		}
		if sc.F.SchacInst {
			c, d := "univ-c.example", "univ-d.example"
			for _, r := range []IdPRef{
				{Name: "instA", Accept: []string{c, d}},
				{Name: "instB", Accept: []string{c, d}, AcceptClaim: "schacHomeOrganization"},
				{Name: "instC", Domains: []string{c, d}},
			} {
				p := NewIdP(r.Name, "llmflow-"+r.Name)
				w.IdPs[r.Name] = p
				w.closers = append(w.closers, p.Close)
				r.Issuer, r.Audience, r.Org, r.Team = p.Issuer(), p.Audience, Borrower, TeamOf[Borrower]
				r.MapClaim, r.MapValue = "schac_home_organization", c
				topo.IdPs = append(topo.IdPs, r)
			}
		}
	}
	nrep := max(1, sc.Replicas)
	if nrep > 1 && opt.PG == "" && !opt.DryRun {
		return fail(fmt.Errorf("%d replicas need PostgreSQL (--pg-dsn-file)", nrep))
	}
	if sc.needs("pg") {
		dsn, drop := "postgres://u:p@127.0.0.1:1/d", func() {}
		if !opt.DryRun {
			var err error
			if dsn, drop, err = FreshDB(ctx, opt.PG); err != nil {
				return fail(err)
			}
		}
		w.closers = append(w.closers, drop)
		via, px, err := viaProxy(dsn, NewProxy)
		if err != nil {
			return fail(err)
		}
		w.closers = append(w.closers, px.Close)
		w.PGProxy = px
		w.pgAdmin = opt.PG
		if u, err := url.Parse(dsn); err == nil {
			w.pgDB = strings.TrimPrefix(u.Path, "/")
		}
		topo.PG = via
	}
	for i := 0; i < nrep; i++ {
		t := topo
		if nrep > 1 {
			a, err := FreeAddr()
			if err != nil {
				return fail(err)
			}
			hp, err := NewProxy(a)
			if err != nil {
				return fail(err)
			}
			w.closers = append(w.closers, hp.Close)
			w.HAProxy = append(w.HAProxy, hp)
			t.HA = &HAPeer{Listen: a, ReplicaID: fmt.Sprintf("r%d", i), Advertise: hp.Addr()}
		}
		g, err := NewGateway(opt.GatewayBin, filepath.Join(dir, fmt.Sprintf("gw%d", i)), t)
		if err != nil {
			return fail(err)
		}
		if out, err := g.CheckConfig(); err != nil {
			return fail(fmt.Errorf("check-config: %v\n%s", err, out))
		}
		w.GW = append(w.GW, g)
	}
	// Replicas share their secret files (one HA group, one master key).
	for i := 1; i < len(w.GW); i++ {
		for _, f := range []string{"master.key", "salt.key", "ha.secret"} {
			b, _ := os.ReadFile(filepath.Join(w.GW[0].Dir, f))
			if err := os.WriteFile(filepath.Join(w.GW[i].Dir, f), b, 0o600); err != nil {
				return fail(err)
			}
		}
		w.GW[i].Master = w.GW[0].Master
	}
	if opt.DryRun { // the configurations passed check-config: nothing is started
		return w, nil
	}
	startWait := 40 * time.Second
	for _, g := range w.GW {
		if err := g.Start(startWait); err != nil {
			return fail(err)
		}
		g := g
		w.closers = append(w.closers, func() { _ = g.Kill() })
	}
	if err := w.provision(); err != nil {
		return fail(err)
	}
	for _, g := range w.GW {
		g.fd0 = fdCount(g.PID())
	}
	w.started = time.Now()
	return w, nil
}

// provision creates the keys and budgets.
func (w *World) provision() error {
	g := w.Primary()
	prefix := func(org string) string {
		if org == Borrower && w.Sc.F.Federation {
			return FedPrefix
		}
		return ""
	}
	for _, org := range AllOrgs {
		n := 1
		if org == "etabA" {
			n = 2 // the second key of etabA carries the key budget
		}
		for i := 0; i < n; i++ {
			id, secret, err := g.CreateKey(TeamOf[org], fmt.Sprintf("chaos-%s-%d", org, i))
			if err != nil {
				return err
			}
			w.Clients = append(w.Clients, Client{Org: org, Team: TeamOf[org], Who: "key:" + id, Cred: Cred{Bearer: secret, Who: "key:" + id}, KeyID: id, Prefix: prefix(org)})
		}
	}
	if w.Sc.F.IdPs {
		for _, d := range []struct{ idp, org, sub string }{{"idpA", "etabA", "alice"}, {"idpB", "etabB", "bob"}, {"fed", Borrower, "carol"}} {
			c := Client{Org: d.org, Team: TeamOf[d.org], IdP: d.idp, Prefix: prefix(d.org), Who: "jwt:" + d.idp + ":" + d.sub}
			c.Scoped = d.sub + "@" + map[string]string{"idpA": "a.example", "idpB": "b.example", "fed": "univ-c.example"}[d.idp]
			w.Clients = append(w.Clients, c)
		}
	}
	if w.Sc.F.SpoolFair || w.Sc.F.SmallFS || w.Sc.F.SlowFS {
		// generous budgets on the two organisations that share the spool: a
		// budgeted request is what the spool's fairness refuses
		for _, org := range []string{"etabA", "etabB"} {
			w.Budgets = append(w.Budgets, BudgetSpec{"org", org, "day", 1e6, 5e5})
			if err := g.SetBudget("org", org, "day", 1e6, 5e5); err != nil {
				return err
			}
		}
	}
	if !w.Sc.F.Budgets {
		return nil
	}
	set := func(kind, name string, hard, soft float64) error {
		w.Budgets = append(w.Budgets, BudgetSpec{kind, name, "day", hard, soft})
		return g.SetBudget(kind, name, "day", hard, soft)
	}
	edge := w.EdgeClient()
	for _, b := range []struct {
		kind, name string
		hard, soft float64
	}{
		{"key", edge.KeyID, 3.0, 2.0},
		{"team", TeamOf["etabB"], 25.0, 15.0},
		{"org", "etabC", 30.0, 20.0},
		{"org", Borrower, 25.0, 15.0},
		{"user", "alice@a.example", 12.0, 8.0},
	} {
		if b.kind == "user" && !w.Sc.F.IdPs {
			continue
		}
		if err := set(b.kind, b.name, b.hard, b.soft); err != nil {
			return err
		}
	}
	return nil
}

// EdgeClient is the key whose hard budget bursts aim at.
func (w *World) EdgeClient() Client {
	for _, c := range w.Clients {
		if c.Org == "etabA" && c.KeyID != "" && c.IdP == "" {
			last := c
			for _, d := range w.Clients {
				if d.Org == "etabA" && d.KeyID != "" {
					last = d
				}
			}
			return last
		}
	}
	return Client{}
}

// Token returns the bearer of a client: its key, or a fresh token from its IdP.
func (w *World) Token(c Client) (string, error) {
	if c.IdP == "" {
		return c.Cred.Bearer, nil
	}
	return w.mintFor(c.IdP, strings.TrimPrefix(c.Who, "jwt:"+c.IdP+":"), c.Scoped, time.Minute)
}

// mintFor signs a token of idp for subject sub. One minute of life by default:
// a token minted on a clock that is minutes behind is then really expired, so
// the gateway's skew allowance is what decides.
func (w *World) mintFor(idp, sub, scoped string, ttl time.Duration) (string, error) {
	extra := map[string]any{}
	if idp == "fed" {
		extra["schac_home_organization"] = "univ-c.example"
		w.mu.Lock()
		if w.schacOverride != "" {
			extra["schac_home_organization"] = w.schacOverride
		}
		w.mu.Unlock()
		// gateway v0.38.0 (breaking): the scope of the person's name must equal a
		// value of the schac claim, so a federation proxy's token carries a
		// scoped name. v0.37.1 accepts it too: the scope is in the accept list.
		extra["preferred_username"] = sub + "@univ-c.example"
	}
	if idp == "idpB" {
		extra["preferred_username"] = scoped // idpB asserts scoped names: bob@b.example
		if scoped == "" {
			extra["preferred_username"] = sub + "@b.example"
		}
	}
	return w.IdPs[idp].Mint(sub, ttl, extra)
}

// faultSpan is when a fault was in force.
type faultSpan struct {
	FaultAt
	From, To time.Time
}

func (w *World) addSpan(s faultSpan) {
	w.mu.Lock()
	w.spans = append(w.spans, s)
	w.mu.Unlock()
}

// Close stops everything, in reverse order.
func (w *World) Close() {
	for i := len(w.closers) - 1; i >= 0; i-- {
		w.closers[i]()
	}
	w.closers = nil
}

// Sorted client list of an organisation.
func (w *World) ClientsOf(org string) []Client {
	var out []Client
	for _, c := range w.Clients {
		if c.Org == org {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Who < out[j].Who })
	return out
}
