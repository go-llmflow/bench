package e2e

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHAMutualTLSLive proves that two llmflow-gateway replicas complete a
// mutual TLS 1.3 handshake on the HA internal port, with certificate FILES
// (multi-line PEM) loaded through the configuration: the path v0.16.0 broke
// (it read them with the one-line secret reader) and v0.16.1 fixed.
//
// What it does, against a real PostgreSQL (a throw-away docker container)
// and a fake vLLM:
//
//  1. writes a CA and two replica certificates (SAN DNS:llmflow-ha; one ECDSA,
//     one Ed25519) as PEM files, keys mode 0600, and points both replicas at
//     the files;
//  2. starts replica A (it takes the pool's lease) and replica B;
//  3. sends requests through B, the non-owner: all succeed at priority 0,
//     B's llmflow_ha_unowned_total stays 0, and while three of them are held
//     at the fake vLLM the OWNER's llmflow_inflight reads 3 - the grants
//     crossed the mTLS channel;
//  4. control: restarts B with a certificate another CA signed (it loads
//     fine). Its requests are now admitted without an owner:
//     llmflow_ha_unowned_total rises and the priority is no longer 0. If this
//     control did not fail, step 3 would prove nothing about the handshake.
//
// It needs docker (a PostgreSQL image, BENCH_PG_IMAGE, default postgres:18)
// and a gateway of v0.16.1 or later (BENCH_GATEWAY_BIN, default: PATH). It
// skips when either is missing. No vLLM is needed.
func TestHAMutualTLSLive(t *testing.T) {
	bin := gatewayBin(t)
	dsn := startPostgres(t)
	dir := t.TempDir()

	// The fake vLLM, with a gate: while closed-over `hold` is non-nil a
	// request waits on it, so a test can keep requests in flight.
	up := &gatedUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	// PKI, as PEM files an operator would write.
	ca := newTestCA(t, "bench ha ca")
	rogue := newTestCA(t, "bench rogue ca")
	caFile := writePEM(t, dir, "ha-ca.pem", ca.pem(), 0o644)
	certA, keyA := ca.leaf(t, "llmflow-ha", "ecdsa")
	certB, keyB := ca.leaf(t, "llmflow-ha", "ed25519")
	certR, keyR := rogue.leaf(t, "llmflow-ha", "ecdsa")

	master := "e2e-" + randomHex(16)
	for name, v := range map[string]string{
		"master": master, "vllm": "e2e-" + randomHex(16),
		"salt": "e2e-" + randomHex(24), "ha": "e2e-ha-" + randomHex(24), "dsn": dsn,
	} {
		writePEM(t, dir, name+".key", []byte(v+"\n"), 0o600)
	}
	kf := func(n string) string { return filepath.Join(dir, n+".key") }

	conf := func(id, listen, haListen string, cert, key []byte) string {
		cf := writePEM(t, dir, id+"-cert.pem", cert, 0o644)
		pf := writePEM(t, dir, id+"-key.pem", key, 0o600)
		cfg := fmt.Sprintf(`
listen              = %q
master_key_file     = %q
cache_salt_key_file = %q
budget_refresh      = "1h"
key_cache_ttl       = "1h"
org "a" {
  share = 1
  team "ta" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool = "p"
  upstream "u" {
    base_url = %q
    key_file = %q
  }
}
store {
  kind      = "postgres"
  dsn_file  = %q
  spool_dir = %q
}
ha {
  replica_id      = %q
  listen          = %q
  peer_networks   = ["127.0.0.0/8"]
  secret_file     = %q
  lease_ttl       = "1500ms"
  heartbeat       = "200ms"
  forward_timeout = "200ms"
  usage_persist   = "300ms"
  tls_cert_file   = %q
  tls_key_file    = %q
  tls_ca_file     = %q
}
`, listen, kf("master"), kf("salt"), srv.URL, kf("vllm"), kf("dsn"),
			filepath.Join(dir, "spool-"+id), id, haListen, kf("ha"), cf, pf, caFile)
		p := filepath.Join(dir, id+".hcl")
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Each replica gets its own client port and internal (HA) port.
	startReplica := func(id string, cert, key []byte) *haReplica {
		listen := freePort(t)
		return startHAReplica(t, bin, conf(id, listen, freePort(t), cert, key), listen)
	}

	for _, p := range []struct {
		id        string
		cert, key []byte
	}{{"A", certA, keyA}, {"B", certB, keyB}, {"R", certR, keyR}} {
		cfg := conf("check-"+p.id, freePort(t), freePort(t), p.cert, p.key)
		if out, err := exec.Command(bin, "check-config", "-c", cfg).CombinedOutput(); err != nil {
			t.Fatalf("check-config with PEM files (%s): %v\n%s", p.id, err, out)
		}
	}

	a := startReplica("A", certA, keyA)
	waitFor(t, "A owns pool p", 20*time.Second, func() bool {
		return a.metric(t, `llmflow_ha_owner{pool="p"}`) == 1
	})
	var k struct{ Key string }
	if code, err := postJSON(a.URL+"/admin/keys", master, `{"team":"ta","name":"ha"}`, nil, &k); err != nil || code != 201 || k.Key == "" {
		t.Fatalf("create key: %d %v", code, err)
	}

	// 3. The positive: requests through the non-owner B.
	b := startReplica("B", certB, keyB)
	waitFor(t, "B reads the lease", 20*time.Second, func() bool { return b.metric(t, `llmflow_ha_epoch{pool="p"}`) >= 1 })
	if b.metric(t, `llmflow_ha_owner{pool="p"}`) != 0 {
		t.Fatal("B claims to own the pool")
	}
	for i := range 5 {
		if code, prio := chatVia(t, b.URL, k.Key); code != 200 || prio != "0" {
			t.Fatalf("request %d through B: HTTP %d, priority %q (want 200, 0: forwarded to the owner)", i, code, prio)
		}
	}
	if n := b.metric(t, "llmflow_ha_unowned_total"); n != 0 {
		t.Fatalf("B admitted %v requests without the owner: the mTLS handshake failed\n%s", n, b.log.String())
	}
	// Three requests held at the fake vLLM: the owner must count them.
	release := up.hold()
	var wg sync.WaitGroup
	results := make(chan string, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, prio := chatVia(t, b.URL, k.Key)
			results <- fmt.Sprintf("%d/%s", code, prio)
		}()
	}
	waitFor(t, "three requests at the upstream", 20*time.Second, func() bool { return up.arrived() >= 3 })
	waitFor(t, "the owner counts B's three requests in flight", 10*time.Second, func() bool {
		return sumMatching(a.metrics(t), "llmflow_inflight", `pool="p"`) >= 3
	})
	if g := b.metric(t, `llmflow_ha_local_grants{pool="p"}`); g < 3 {
		t.Errorf("B's llmflow_ha_local_grants = %v while 3 requests are in flight", g)
	}
	if n := sumMatching(b.metrics(t), "llmflow_inflight", `pool="p"`); n != 0 {
		t.Errorf("B (not the owner) publishes llmflow_inflight = %v", n)
	}
	close(release)
	wg.Wait()
	close(results)
	for r := range results {
		if r != "200/0" {
			t.Errorf("held request through B finished %s, want 200/0", r)
		}
	}
	if n := b.metric(t, "llmflow_ha_unowned_total"); n != 0 {
		t.Fatalf("B admitted %v requests without the owner", n)
	}
	t.Logf("positive: B (Ed25519 leaf) forwarded 8 of 8 admissions to A over mTLS; owner's llmflow_inflight reached 3; unowned_total 0")

	// 4. The planted negative control: B restarts with a certificate another
	// CA signed. It loads (check-config passed above) but the owner does not
	// answer it.
	b.stop()
	r := startReplica("R", certR, keyR)
	waitFor(t, "R reads the lease", 20*time.Second, func() bool { return r.metric(t, `llmflow_ha_epoch{pool="p"}`) >= 1 })
	for i := range 3 {
		code, prio := chatVia(t, r.URL, k.Key)
		if code != 200 {
			t.Fatalf("control request %d: HTTP %d", i, code)
		}
		if prio == "0" {
			t.Fatalf("control request %d: priority 0, i.e. the owner decided it for a replica whose certificate another CA signed: the control did not fail, the positive proves nothing", i)
		}
	}
	if n := r.metric(t, "llmflow_ha_unowned_total"); n < 3 {
		t.Fatalf("control: llmflow_ha_unowned_total = %v, want >= 3: the rogue replica reached the owner", n)
	}
	t.Logf("control: R (certificate from another CA) was refused: unowned_total %v, priority != 0", r.metric(t, "llmflow_ha_unowned_total"))
}

// haReplica is one llmflow-gateway process.
type haReplica struct {
	URL string
	cmd *exec.Cmd
	log *bytes.Buffer
	t   *testing.T
	one sync.Once
}

func startHAReplica(t *testing.T, bin, cfg, listen string) *haReplica {
	t.Helper()
	r := &haReplica{URL: "http://" + listen, log: &bytes.Buffer{}, t: t}
	r.cmd = exec.Command(bin, "serve", "--config", cfg)
	r.cmd.Stdin = nil
	r.cmd.Stdout, r.cmd.Stderr = r.log, r.log
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.stop()
		if t.Failed() {
			t.Logf("replica %s log:\n%s", cfg, r.log.String())
		}
	})
	waitFor(t, "replica healthy", 30*time.Second, func() bool {
		resp, err := http.Get(r.URL + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return r
}

func (r *haReplica) stop() {
	r.one.Do(func() {
		_ = r.cmd.Process.Kill()
		_ = r.cmd.Wait()
	})
}

func (r *haReplica) metrics(t *testing.T) map[string]float64 { return Metrics(t, r.URL+"/metrics") }

// metric reads one series by its exact key ("name" or `name{labels}`); -1 if
// absent, so a renamed metric cannot read as 0.
func (r *haReplica) metric(t *testing.T, key string) float64 {
	t.Helper()
	if v, ok := r.metrics(t)[key]; ok {
		return v
	}
	return -1
}

// chatVia returns the status and the X-Llmflow-Priority of one chat request.
func chatVia(t *testing.T, base, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("chat on %s: %v", base, err)
		return 0, ""
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Llmflow-Priority")
}

// gatedUpstream is a fake vLLM whose answers can be held.
type gatedUpstream struct {
	mu   sync.Mutex
	gate chan struct{}
	n    int
}

func (u *gatedUpstream) hold() (release chan struct{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gate = make(chan struct{})
	u.n = 0
	return u.gate
}

func (u *gatedUpstream) arrived() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

func (u *gatedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	u.mu.Lock()
	g := u.gate
	if g != nil {
		u.n++
	}
	u.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
}

// startPostgres runs a throw-away PostgreSQL in docker with trust
// authentication (no password exists, so none can leak) and returns its DSN.
func startPostgres(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found: no PostgreSQL for the HA test")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable: %v %s", err, bytes.TrimSpace(out))
	}
	image := os.Getenv("BENCH_PG_IMAGE")
	if image == "" {
		image = "postgres:18"
	}
	name := "bench-hamtls-" + randomHex(4)
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-p", "127.0.0.1::5432", image).CombinedOutput(); err != nil {
		t.Skipf("cannot start %s: %v %s", image, err, bytes.TrimSpace(out))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	out, err := exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	// "127.0.0.1:49153" possibly followed by an IPv6 line.
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	_, port, err := net.SplitHostPort(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("docker port %q: %v", out, err)
	}
	waitFor(t, "postgres ready", 90*time.Second, func() bool {
		// TCP inside the container: the init phase listens on the socket only.
		return exec.Command("docker", "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "postgres").Run() == nil
	})
	return "postgres://postgres@127.0.0.1:" + port + "/postgres?sslmode=disable"
}

// --- a small PKI, in memory, written out as PEM files ---

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func serial(t *testing.T) *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: k, der: der}
}

func (c *testCA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})
}

// leaf issues a certificate valid as server and client, with the given DNS
// SAN, and returns it with its PKCS #8 key, both PEM.
func (c *testCA) leaf(t *testing.T, san, alg string) (certPEM, keyPEM []byte) {
	t.Helper()
	var priv crypto.Signer
	var err error
	switch alg {
	case "ecdsa":
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ed25519":
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("algorithm %q", alg)
	}
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: san},
		DNSNames: []string{san}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, priv.Public(), c.key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
}

// writePEM writes a file with the given mode, whatever the umask.
func writePEM(t *testing.T, dir, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}
