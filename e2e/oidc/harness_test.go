// Package oidc checks llmflow-gateway's OpenID Connect paths (JWT access
// tokens on the inference API, SSO for administrators, SCIM provisioning)
// against REAL identity providers: dex (BENCH_DEX_IMAGE, a container
// image) and go-authn/bridge (BENCH_BRIDGE_BIN, in front of a test SAML
// IdP signed by xmlsec1). Each test skips itself when its provider is not
// given. The tests start the providers, a fake upstream and the gateway.
package oidc

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

const (
	envGateway = "BENCH_GATEWAY_BIN" // llmflow-gateway binary (default: PATH)

	clockSkew  = 5 * time.Second  // gateway jwt.clock_skew
	minRefresh = 20 * time.Second // gateway jwt.min_refresh

	apiClient   = "llmflow-api"   // the audience of API tokens
	adminClient = "llmflow-admin" // the SSO client

	orgA, orgB   = "inst-alpha", "inst-beta"
	teamA, teamB = "lab-alpha", "lab-beta"
)

// person is a user of both providers. Their groups are dex groups or SAML
// eduPersonEntitlement values.
type person struct {
	email, name string
	groups      []string
}

var people = map[string]person{
	"alice": {"alice@example.org", "alice", []string{"grp-alpha", "admins-alpha"}},
	"bob":   {"bob@example.org", "bob", []string{"grp-beta"}},
	"carol": {"carol@example.org", "carol", nil},
}

// IdP is what the checks need from a provider.
type IdP interface {
	Name() string
	Issuer() string                            // what the gateway trusts (a counting proxy)
	APIToken(t *testing.T, user string) string // aud = apiClient
	WrongAudienceToken(t *testing.T, user string) string
	OtherKeyToken(t *testing.T, user string) string // same issuer, another key
	// Rotate makes the provider sign with a new key while it still
	// publishes the old one.
	Rotate(t *testing.T)
	Browser(user string) *Browser // ready to log in as user
	Password() string
	// GatewayName is what the gateway calls the person after an SSO
	// login: preferred_username.
	GatewayName(user string) string
	TTL() time.Duration // access token lifetime
}

func randomHex(n int) string { return hex.EncodeToString(randomBytes(n)) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeFile(t *testing.T, dir, name, v string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(v), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// Proxy counts the key-set fetches on their way to a provider. Its URL is
// the provider's issuer; the provider listens behind it.
type Proxy struct {
	Port        int
	JWKSFetches atomic.Int64
	target      atomic.Pointer[url.URL]
}

func NewProxy(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{Port: freePort(t)}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/keys") || strings.HasSuffix(r.URL.Path, "/jwks") || strings.Contains(r.URL.Path, "jwks") {
			p.JWKSFetches.Add(1)
		}
		u := p.target.Load()
		if u == nil {
			http.Error(w, "no target", 502)
			return
		}
		httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return p
}

func (p *Proxy) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", p.Port) }

func (p *Proxy) To(base string) {
	u, _ := url.Parse(base)
	p.target.Store(u)
}

// Gateway is one llmflow-gateway in front of the fake upstream, trusting
// one provider for both JWTs and SSO.
type Gateway struct {
	URL, Master, SCIMToken string
	log                    *bytes.Buffer
}

func gatewayBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(envGateway); p != "" {
		return p
	}
	p, err := exec.LookPath("llmflow-gateway")
	if err != nil {
		t.Skipf("%s not set and llmflow-gateway not on PATH", envGateway)
	}
	return p
}

// GatewayPort is chosen before the provider starts, because the provider
// must know the gateway's redirect URI.
func StartGateway(t *testing.T, port int, issuer, adminSecret string) *Gateway {
	t.Helper()
	bin := gatewayBin(t)
	g := &Gateway{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Master: "e2e-" + randomHex(32), SCIMToken: "scim-" + randomHex(32), log: &bytes.Buffer{}}
	up := httptest.NewServer(fakeupstream.NewServer(fakeupstream.Options{Model: "fake", Reply: "ok fine"}, nil))
	t.Cleanup(up.Close)
	dir := t.TempDir()
	w := func(name, v string) string { return writeFile(t, dir, name, v+"\n", 0o600) }
	conf := fmt.Sprintf(`listen = %q
master_key_file = %q
cache_isolation = "org"
cache_salt_key_file = %q
auth_failures_per_minute = 0
store {
  kind = "memory"
}
org %q {
  share = 0.5
  team %q {}
}
org %q {
  share = 0.5
  team %q {}
}
pool "p" { capacity = 4 }
model "chat" {
  upstream_model = "fake"
  pool           = "p"
  class          = "interactive"
  key_file       = %q
  upstream "u" { base_url = %q }
}
jwt {
  issuer      = %q
  audience    = %q
  clock_skew  = %q
  min_refresh = %q
  map {
    value = "grp-alpha"
    team  = %q
  }
  map {
    value = "grp-beta"
    team  = %q
  }
}
sso {
  issuer             = %q
  client_id          = %q
  client_secret_file = %q
  redirect_url       = %q
  scopes             = ["openid", "profile", "email", "groups"]
  role {
    value = "admins-alpha"
    role  = "org_admin"
    org   = %q
  }
}
scim {
  token_file = %q
  base_url   = %q
  group "Beta Leads" {
    team = %q
    role = "team_admin"
  }
}
`, fmt.Sprintf("127.0.0.1:%d", port), w("master.key", g.Master), w("salt.key", randomHex(32)),
		orgA, teamA, orgB, teamB, w("upstream.key", randomHex(16)), up.URL,
		issuer, apiClient, clockSkew.String(), minRefresh.String(), teamA, teamB,
		issuer, adminClient, w("sso.secret", adminSecret), g.URL+"/admin/callback", orgA,
		w("scim.token", g.SCIMToken), g.URL+"/scim/v2", teamB)
	cfgPath := w("gateway.hcl", conf)
	if out, err := exec.Command(bin, "check-config", "--config", cfgPath).CombinedOutput(); err != nil {
		t.Fatalf("check-config: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "serve", "--config", cfgPath)
	cmd.Stdout, cmd.Stderr = g.log, g.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("gateway log:\n%s", g.log.String())
		}
	})
	waitHTTP(t, g.URL+"/healthz", 30*time.Second)
	return g
}

func waitHTTP(t *testing.T, u string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready: %v", u, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Chat calls the inference API with a bearer and returns status and body.
func (g *Gateway) Chat(t *testing.T, bearer string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"one two three"}],"max_tokens":4}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Get is an unauthenticated GET (/metrics, /healthz: loopback routes).
func (g *Gateway) Get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(g.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Admin calls an admin route with the master key.
func (g *Gateway) Admin(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, g.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+g.Master)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// jwtParts decodes the header and claims of a compact JWS, unverified.
func jwtParts(t *testing.T, tok string) (hdr, claims map[string]any) {
	t.Helper()
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		t.Fatalf("not a JWS: %d parts", len(p))
	}
	dec := func(s string, v any) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || json.Unmarshal(b, v) != nil {
			t.Fatalf("bad segment %q", s)
		}
	}
	dec(p[0], &hdr)
	dec(p[1], &claims)
	return
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}
