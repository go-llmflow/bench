package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// dex: https://dexidp.io, configuration from its config.yaml.dist (v2.46.1):
// staticClients, staticPasswords (with groups and preferredUsername),
// oauth2.passwordConnector for the password grant, expiry.idTokens, and
// signer.config.keysRotationPeriod. dex checks for a rotation every 30 s
// (server/signer/local.go) and keeps the previous key published while a
// token it signed may still be valid (server/signer/rotation.go).

const envDexImage = "BENCH_DEX_IMAGE" // e.g. ghcr.io/dexidp/dex:v2.46.1

const (
	dexTTL      = 120 * time.Second
	dexRotation = 4 * time.Minute // long enough for the SSO checks to run on one key first
)

type dexIdP struct {
	image                  string
	proxy                  *Proxy
	issuer                 string
	dex1, dex2             string // direct base URLs
	password               string
	apiSecret, adminSecret string
	started                time.Time
}

func (d *dexIdP) Name() string       { return "dex" }
func (d *dexIdP) Issuer() string     { return d.issuer }
func (d *dexIdP) Password() string   { return d.password }
func (d *dexIdP) TTL() time.Duration { return dexTTL }
func (d *dexIdP) GatewayName(user string) string {
	return people[user].name
}

func (d *dexIdP) config(redirect string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(d.password), 10)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `issuer: %s
storage:
  type: memory
web:
  http: 0.0.0.0:5556
expiry:
  idTokens: %q
signer:
  type: local
  config:
    keysRotationPeriod: %q
oauth2:
  skipApprovalScreen: true
  passwordConnector: local
enablePasswordDB: true
staticClients:
  - id: %s
    name: API audience
    secret: %s
    redirectURIs: ["http://127.0.0.1/unused"]
  - id: %s
    name: llmflow admin
    secret: %s
    redirectURIs: [%q]
staticPasswords:
`, d.issuer, dexTTL.String(), dexRotation.String(), apiClient, d.apiSecret, adminClient, d.adminSecret, redirect)
	i := 0
	for _, name := range []string{"alice", "bob", "carol"} {
		p := people[name]
		i++
		fmt.Fprintf(&b, "  - email: %q\n    hash: %q\n    username: %q\n    preferredUsername: %q\n    userID: \"%08d-0000-4000-8000-000000000000\"\n",
			p.email, string(hash), p.name, p.name, i)
		if len(p.groups) > 0 {
			b.WriteString("    groups:\n")
			for _, g := range p.groups {
				fmt.Fprintf(&b, "      - %q\n", g)
			}
		}
	}
	return b.String(), nil
}

func (d *dexIdP) run(t *testing.T, cfg string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", cfg, 0o644)
	port := freePort(t)
	name := "bench-e2e-dex-" + randomHex(4)
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:5556", port),
		"-v", dir+":/etc/bench-dex:ro", d.image, "dex", "serve", "/etc/bench-dex/config.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run dex: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "30", name).CombinedOutput()
			t.Logf("dex %s log tail:\n%s", name, logs)
		}
		exec.Command("docker", "rm", "-f", name).Run()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHTTP(t, base+"/dex/.well-known/openid-configuration", 60*time.Second)
	return base
}

// startDex starts dex 1 behind the counting proxy and dex 2 (same issuer
// string and clients, its own keys). gwPort is the gateway's future port.
func startDex(t *testing.T, gwPort int) *dexIdP {
	t.Helper()
	image := os.Getenv(envDexImage)
	if image == "" {
		t.Skipf("%s not set: no dex to test against", envDexImage)
	}
	d := &dexIdP{image: image, proxy: NewProxy(t), password: "pw-" + randomHex(12), apiSecret: randomHex(16), adminSecret: randomHex(16)}
	d.issuer = d.proxy.URL() + "/dex"
	cfg, err := d.config(fmt.Sprintf("http://127.0.0.1:%d/admin/callback", gwPort))
	if err != nil {
		t.Fatal(err)
	}
	d.dex1 = d.run(t, cfg)
	d.proxy.To(d.dex1)
	d.started = time.Now()
	d.dex2 = d.run(t, cfg)
	return d
}

// token is the password grant (RFC 6749 §4.3), which dex offers through
// oauth2.passwordConnector.
func (d *dexIdP) token(t *testing.T, base, client, secret, user string) string {
	t.Helper()
	form := url.Values{"grant_type": {"password"}, "username": {people[user].email}, "password": {d.password},
		"scope": {"openid profile email groups"}}
	req, _ := http.NewRequest(http.MethodPost, base+"/dex/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(client, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(b, &tok) != nil || tok.AccessToken == "" {
		t.Fatalf("dex token for %s: HTTP %d %s", user, resp.StatusCode, b)
	}
	return tok.AccessToken
}

func (d *dexIdP) APIToken(t *testing.T, user string) string {
	return d.token(t, d.proxy.URL(), apiClient, d.apiSecret, user)
}

func (d *dexIdP) WrongAudienceToken(t *testing.T, user string) string {
	return d.token(t, d.proxy.URL(), adminClient, d.adminSecret, user)
}

func (d *dexIdP) OtherKeyToken(t *testing.T, user string) string {
	return d.token(t, d.dex2, apiClient, d.apiSecret, user)
}

// Rotate waits for dex's own rotation: its key changes after
// keysRotationPeriod, checked every 30 s.
func (d *dexIdP) Rotate(t *testing.T) {
	t.Helper()
	h0, _ := jwtParts(t, d.APIToken(t, "bob"))
	deadline := d.started.Add(dexRotation + 90*time.Second)
	for time.Now().Before(deadline) {
		h, _ := jwtParts(t, d.APIToken(t, "bob"))
		if h["kid"] != h0["kid"] {
			return
		}
		sleep(context.Background(), 5*time.Second)
	}
	t.Fatalf("dex did not rotate its key by %s", deadline.Format("15:04:05"))
}

func (d *dexIdP) Browser(user string) *Browser {
	return NewBrowser(people[user].email, d.password)
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func TestDex(t *testing.T) {
	gwPort := freePort(t)
	d := startDex(t, gwPort)
	g := StartGateway(t, gwPort, d.Issuer(), d.adminSecret)
	runChecks(t, d, d.proxy, g)
}
