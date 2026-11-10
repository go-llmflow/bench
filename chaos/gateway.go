package chaos

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Orgs of the simulated consortium: three owners with shares 50/30/20 of one
// pool, and an ILaaS third party that holds no share and borrows the spare.
var (
	Owners      = []string{"etabA", "etabB", "etabC"}
	OwnerShares = map[string]float64{"etabA": 0.5, "etabB": 0.3, "etabC": 0.2}
	Borrower    = "tiers"
	AllOrgs     = []string{"etabA", "etabB", "etabC", "tiers"}
	// TeamOf is the team each organisation's clients belong to.
	TeamOf = map[string]string{"etabA": "a1", "etabB": "b1", "etabC": "c1", "tiers": "t1"}
)

// Model prices (units per million tokens) and the bound L of the model: the
// worst case W of a request follows gateway/bound.go (docs: Hard budgets).
const (
	PriceIn, PriceOut, PriceCached = 1000.0, 5000.0, 100.0
	MaxModelLen                    = 512
	PoolCapacity                   = 8
	ModelName                      = "chat"
	FedPrefix                      = "/fed/ilaas"
)

// WorstCase is W in units for one prompt: max((L-M)·p_in + n·M·p_out, L·p_in).
func WorstCase(maxTokens, n int) float64 {
	m := min(maxTokens, MaxModelLen)
	pin := max(PriceIn, PriceCached)
	return max(float64(MaxModelLen-m)*pin+float64(n*m)*PriceOut, float64(MaxModelLen)*pin) / 1e6
}

// Cost is the cost in units of a usage.
func Cost(prompt, completion int) float64 {
	return (float64(prompt)*PriceIn + float64(completion)*PriceOut) / 1e6
}

// Topology is everything a gateway configuration needs from the world.
type Topology struct {
	UpstreamURL string
	DCGMURL     string
	BMCURL      string // https base URL of the BMC, "" for none
	BMCCAFile   string
	BMCUser     string
	BMCPassword string
	IdPs        []IdPRef // two institutions' providers and the federation issuer
	Fluids      bool
	Federation  bool
	NoModelLen  bool // leave max_model_len out: learnt from /v1/models
	// Extra HCL appended verbatim (a scenario's own settings).
	Extra string
	// Clock: the gateway follows a clock file (a -tags chaosclock build);
	// ClockOffset is its starting offset.
	Clock       bool
	ClockOffset time.Duration
	// SpoolDir overrides the spool's directory (a tiny filesystem).
	SpoolDir string
	// Tag is the gateway release under test ("" : the latest): a configuration
	// key that a release does not know is left out for it (OrgDomains).
	Tag string
	// OrgDomains: the domains an organisation owns (org { domains }, v0.39.0).
	OrgDomains map[string][]string
	// StoreExtra is HCL inside the store block (PostgreSQL only).
	StoreExtra string
	// ModelExtra is HCL inside the main model's block (fallbacks, say).
	ModelExtra string
	// PoolExtra is HCL inside the main pool's block (per-pool shares, say).
	PoolExtra string
	// PG is a PostgreSQL DSN; "" means the memory store.
	PG string
	// HA is set for each replica of an HA pair.
	HA *HAPeer
}

// HAPeer places a replica in an HA group.
type HAPeer struct {
	Listen, ReplicaID string
	// Advertise is the address the other replicas dial: a proxy's, so that a
	// network partition between replicas can be injected.
	Advertise string
}

// IdPRef is the part of an identity provider the configuration names.
type IdPRef struct {
	Name, Issuer, Audience, Org, Team, Domain string
	Accept                                    []string // schac_home_organization values (federation issuer)
	// AcceptClaim: the claim accept names (default schac_home_organization);
	// MapClaim/MapValue: a map on another claim (default: groups = users);
	// Domains: several domains.
	AcceptClaim, MapClaim, MapValue string
	Domains                         []string
	// Legacy: the issuer's unscoped names are this domain's (only one issuer may).
	Legacy bool
}

// Gateway is one llmflow-gateway process.
type Gateway struct {
	Bin    string
	Dir    string
	Listen string
	URL    string
	Master string
	topo   Topology
	cmd    *exec.Cmd
	log    *lockedBuf
	mu     sync.Mutex
	done   chan struct{}
	fd0    int // descriptors after provisioning, the baseline of the leak check
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() > 4<<20 {
		return len(p), nil
	}
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// FreeAddr returns a loopback address that was free a moment ago.
func FreeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

// NewGateway writes the secret files and the configuration in dir; Start runs it.
func NewGateway(bin, dir string, t Topology) (*Gateway, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, f := range []string{"master.key", "salt.key", "upstream.key", "ha.secret", "scim.token"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("chaos-"+randHex(24)+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	if t.BMCURL != "" {
		if err := os.WriteFile(filepath.Join(dir, "bmc.pw"), []byte(t.BMCPassword+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	if t.PG != "" {
		if err := os.WriteFile(filepath.Join(dir, "pg.dsn"), []byte(t.PG+"\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(cmpOr(t.SpoolDir, filepath.Join(dir, "spool")), 0o700); err != nil {
			return nil, err
		}
	}
	listen, err := FreeAddr()
	for err == nil && t.HA != nil && listen == t.HA.Listen {
		// the kernel hands a just-freed port out again: the client and the
		// replicas' channel must not share one (seen once in CI: gw1's check-config)
		listen, err = FreeAddr()
	}
	if err != nil {
		return nil, err
	}
	m, _ := os.ReadFile(filepath.Join(dir, "master.key"))
	g := &Gateway{Bin: bin, Dir: dir, Listen: listen, URL: "http://" + listen, Master: strings.TrimSpace(string(m)), topo: t, log: &lockedBuf{}}
	if err := os.WriteFile(g.ConfigPath(), []byte(g.config()), 0o600); err != nil {
		return nil, err
	}
	if t.Clock {
		if err := g.SetClock(t.ClockOffset); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// ClockPath is the file a -tags chaosclock gateway reads its clock offset from.
func (g *Gateway) ClockPath() string { return filepath.Join(g.Dir, "clock") }

// SetClock moves the gateway's clock to real time + off (atomically: a rename).
func (g *Gateway) SetClock(off time.Duration) error {
	tmp := g.ClockPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatFloat(off.Seconds(), 'f', 3, 64)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.ClockPath())
}

// ConfigPath is the HCL file.
func (g *Gateway) ConfigPath() string { return filepath.Join(g.Dir, "gateway.hcl") }

// config renders the HCL of the simulated deployment.
func (g *Gateway) config() string {
	t := g.topo
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("listen = %q\nmaster_key_file = %q\ncache_isolation = \"org\"\ncache_salt_key_file = %q\nauth_failures_per_minute = 0\nshutdown_grace = \"2s\"\nbudget_refresh = \"1s\"\n",
		g.Listen, filepath.Join(g.Dir, "master.key"), filepath.Join(g.Dir, "salt.key"))
	if t.PG != "" {
		p("store {\n  kind = \"postgres\"\n  dsn_file = %q\n  fail_closed = true\n  spool_dir = %q\n%s}\n", filepath.Join(g.Dir, "pg.dsn"), cmpOr(t.SpoolDir, filepath.Join(g.Dir, "spool")), t.StoreExtra)
	} else {
		p("store {\n  kind = \"memory\"\n}\n")
	}
	if t.HA != nil {
		p("ha {\n  listen = %q\n  peer_networks = [\"127.0.0.0/8\"]\n  secret_file = %q\n  replica_id = %q\n", t.HA.Listen, filepath.Join(g.Dir, "ha.secret"), t.HA.ReplicaID)
		if t.HA.Advertise != "" {
			p("  advertise = %q\n", t.HA.Advertise)
		}
		p("}\n")
	}
	p("health {\n  path = \"/health\"\n  interval = \"1s\"\n  timeout = \"1s\"\n  cooldown = \"1s\"\n}\n")
	p("limits {\n  upstream_header_timeout = \"6s\"\n}\n")
	p("budgets {\n  grant_idle = \"1s\"\n}\n")
	for _, o := range Owners {
		dom := ""
		if d := t.OrgDomains[o]; len(d) > 0 && TagAtLeast(t.Tag, "v0.39.0") { // org { domains } exists from v0.39.0
			dom = fmt.Sprintf("  domains = [%s]\n", quoteList(d))
		}
		p("org %q {\n  share = %v\n%s  team %q {}\n}\n", o, OwnerShares[o], dom, TeamOf[o])
	}
	p("org %q {\n  share = 0\n  team %q {}\n  billing {\n    markup = 0.15\n  }\n}\n", Borrower, TeamOf[Borrower])
	p("pool \"gpu\" {\n  capacity = %d\n  borrow = \"spare\"\n%s}\n", PoolCapacity, t.PoolExtra)
	p("model %q {\n  upstream_model = \"fake\"\n  pool = \"gpu\"\n  class = \"interactive\"\n  key_file = %q\n  max_output_tokens = 256\n", ModelName, filepath.Join(g.Dir, "upstream.key"))
	if !t.NoModelLen { // else the gateway must learn L from GET /v1/models
		p("  max_model_len = %d\n", MaxModelLen)
	}
	b.WriteString(t.ModelExtra)
	p("  cost {\n    input_per_mtok = %v\n    output_per_mtok = %v\n    cached_per_mtok = %v\n  }\n", PriceIn, PriceOut, PriceCached)
	p("  upstream \"n1\" {\n    base_url = %q\n", t.UpstreamURL)
	if t.DCGMURL != "" {
		p("    dcgm_url = %q\n", t.DCGMURL+"/metrics")
	}
	if t.BMCURL != "" {
		p("    redfish_url = %q\n    redfish_user = %q\n    redfish_password_file = %q\n    redfish_ca_file = %q\n", t.BMCURL, t.BMCUser, filepath.Join(g.Dir, "bmc.pw"), t.BMCCAFile)
	}
	p("  }\n}\n")
	if t.Fluids {
		p(`fluids {
  pue = 1.3
  price_per_kwh = 0.18
  currency = "EUR"
  idle_energy = "by_usage"
  idle_watts = 100
  interval = "1s"
  settle = "1s"
  timezone = "Europe/Paris"
  node_overhead_factor = 1.12
  fixed_cost "racks" {
    monthly = 1800
    pool = "gpu"
  }
  fixed_cost "depreciation" {
    monthly = 4200
    pool = "gpu"
    charge = "non_shareholders"
  }
  tariff "peak" {
    price_per_kwh = 0.25
    from = "07:00"
    to = "23:00"
  }
  tariff "night" {
    price_per_kwh = 0.08
    from = "23:00"
    to = "07:00"
  }
}
`)
	}
	for _, i := range t.IdPs {
		p("jwt %q {\n  issuer = %q\n  audience = %q\n  orgs = [%q]\n", i.Name, i.Issuer, i.Audience, i.Org)
		switch {
		case i.Domain != "" && i.Legacy:
			p("  domain = %q\n", i.Domain)
		case i.Domain != "":
			p("  domains = [%q]\n", i.Domain)
		}
		if len(i.Domains) > 0 {
			p("  domains = [%s]\n", quoteList(i.Domains))
		}
		if len(i.Accept) > 0 {
			claim := i.AcceptClaim
			if claim == "" {
				claim = "schac_home_organization"
			}
			p("  accept {\n    claim = %q\n    values = [%s]\n  }\n", claim, quoteList(i.Accept))
		}
		mc, mv := "groups", "users"
		if i.MapClaim != "" {
			mc, mv = i.MapClaim, i.MapValue
		}
		p("  clock_skew = \"5m\"\n  min_refresh = \"5s\"\n  map {\n    claim = %q\n    value = %q\n    team = %q\n  }\n}\n", mc, mv, i.Team)
	}
	if t.Federation {
		p("federation \"ilaas\" {\n  prefix = %q\n  org = %q\n  metrics_networks = [\"127.0.0.0/8\"]\n}\n", FedPrefix, Borrower)
	}
	b.WriteString(strings.ReplaceAll(t.Extra, "@DIR@", g.Dir))
	return b.String()
}

func quoteList(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

// CheckConfig runs `check-config` and returns its output.
func (g *Gateway) CheckConfig() (string, error) {
	out, err := exec.Command(g.Bin, "check-config", "--config", g.ConfigPath()).CombinedOutput()
	return string(out), err
}

// Start runs the gateway and waits until /healthz answers.
func (g *Gateway) Start(wait time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cmd != nil && g.done != nil {
		select {
		case <-g.done:
		default:
			// a second process would leave the first one untracked and running
			return fmt.Errorf("gateway already running (pid %d)", g.cmd.Process.Pid)
		}
	}
	cmd := exec.Command(g.Bin, "serve", "--config", g.ConfigPath())
	if g.topo.Clock { // a gateway built with -tags chaosclock follows this file
		cmd.Env = append(os.Environ(), "LLMFLOW_TEST_CLOCK_FILE="+g.ClockPath())
	}
	g.log = &lockedBuf{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, g.log, g.log
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	g.cmd = cmd
	g.done = make(chan struct{})
	go func(c *exec.Cmd, d chan struct{}) { _ = c.Wait(); close(d) }(cmd, g.done)
	deadline := time.Now().Add(wait)
	for {
		// no keep-alive: an idle connection kept by the default client is a
		// goroutine and a descriptor the leak check would count against us.
		if resp, err := adminClient.Get(g.URL + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-g.done:
			return fmt.Errorf("gateway exited at start: %s", g.log.String())
		default:
		}
		if time.Now().After(deadline) {
			// not g.Kill(): Start holds g.mu, which signal takes
			signalGroup(cmd, false)
			<-g.done
			return fmt.Errorf("gateway not healthy after %s: %s", wait, g.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// PID is the process id, 0 when not running.
func (g *Gateway) PID() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cmd == nil || g.cmd.Process == nil {
		return 0
	}
	select {
	case <-g.done:
		return 0
	default:
	}
	return g.cmd.Process.Pid
}

// Kill stops the process with SIGKILL (a crash) and waits for it.
func (g *Gateway) Kill() error { return g.signal(false) }

// Stop asks for a graceful shutdown (SIGTERM) and waits up to 10 s.
func (g *Gateway) Stop() error { return g.signal(true) }

func (g *Gateway) signal(graceful bool) error {
	g.mu.Lock()
	cmd, done := g.cmd, g.done
	g.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	signalGroup(cmd, graceful)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		signalGroup(cmd, false)
		<-done
	}
	return nil
}

// Log is what the process wrote.
func (g *Gateway) Log() string { return g.log.String() }

// Admin is a call to the admin API with the master key.
func (g *Gateway) Admin(method, path, body string) (int, []byte, error) {
	return g.AdminAs(g.Master, method, path, body)
}

// AdminAs is a call to the admin API with the given bearer (a user's admin token).
func (g *Gateway) AdminAs(bearer, method, path, body string) (int, []byte, error) {
	req, err := http.NewRequest(method, g.URL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := adminClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, nil
}

var adminClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

// CreateKey makes a virtual key for a team and returns its id and secret.
func (g *Gateway) CreateKey(team, name string) (id, secret string, err error) {
	code, b, err := g.Admin("POST", "/admin/keys", fmt.Sprintf(`{"team":%q,"name":%q}`, team, name))
	if err != nil {
		return "", "", err
	}
	var k struct{ ID, Key string }
	if code != 201 || json.Unmarshal(b, &k) != nil || k.Key == "" {
		return "", "", fmt.Errorf("create key for %s: %d %s", team, code, b)
	}
	return k.ID, k.Key, nil
}

// SetBudget sets a hard budget (soft > 0 adds a soft limit).
func (g *Gateway) SetBudget(kind, name string, period string, hard, soft float64) error {
	body := map[string]any{"kind": kind, "name": name, "period": period, "max_cost": hard}
	if soft > 0 {
		body["soft_limit"] = soft
	}
	bb, _ := json.Marshal(body)
	code, b, err := g.Admin("PUT", "/admin/budgets", string(bb))
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("budget %s:%s: %d %s", kind, name, code, b)
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
