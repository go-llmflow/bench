package oidc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// go-authn/bridge (authn-bridge), configured from its README: issuer,
// signing_key_file and subject_salt_file from `authn-bridge keygen`, a
// saml block (SP key and certificate, federation metadata URL pinned by
// the fingerprint of its signing certificate, one IdP), claims (eppn ->
// preferred_username, eduPersonEntitlement -> groups), and clients. Key
// rotation is internal/app/config.go's retired_signing_key_files: a key
// that signed before the current one stays published.

const envBridge = "BENCH_BRIDGE_BIN" // authn-bridge binary

const bridgeTTL = 120 * time.Second // token_lifetime

type bridgeIdP struct {
	t                      *testing.T
	bin                    string
	proxy                  *Proxy
	saml                   *SAMLIdP
	dir                    string
	gwRedirect             string
	password               string
	apiSecret, adminSecret string
	key, retired           []string // current key file, retired ones
	listen                 int
	stop                   func()
	other                  string // host:port of bridge 2 (same issuer, other key)
	otherKey               string
}

func (b *bridgeIdP) Name() string       { return "authn-bridge" }
func (b *bridgeIdP) Issuer() string     { return b.proxy.URL() }
func (b *bridgeIdP) Password() string   { return b.password }
func (b *bridgeIdP) TTL() time.Duration { return bridgeTTL }
func (b *bridgeIdP) GatewayName(user string) string {
	return people[user].name + "@" + idpScope
}

func (b *bridgeIdP) keygen(t *testing.T, name string, salt bool) string {
	t.Helper()
	p := filepath.Join(b.dir, name)
	args := []string{"keygen", "--key", p}
	if salt {
		args = append(args, "--salt", filepath.Join(b.dir, "salt"))
	}
	if out, err := exec.Command(b.bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("authn-bridge keygen: %v %s", err, out)
	}
	return p
}

func (b *bridgeIdP) config(listen int, key string, retired []string) string {
	q := func(ss []string) string {
		var out []string
		for _, s := range ss {
			out = append(out, fmt.Sprintf("%q", s))
		}
		return "[" + strings.Join(out, ", ") + "]"
	}
	return fmt.Sprintf(`issuer            = %q
listen            = "127.0.0.1:%d"
signing_key_file  = %q
retired_signing_key_files = %s
subject_salt_file = %q
token_lifetime    = %q
requests_per_minute = 0

saml {
  key_file             = %q
  cert_file            = %q
  metadata_url         = %q
  metadata_cert_file   = %q
  metadata_fingerprint = %q
  idps                 = [%q]
  names                = { fr = "Passerelle de test", en = "Test bridge" }
  technical_contact    = "noc@example.org"
}

claims {
  username = "eppn"
  groups   = ["entitlement"]
}

client %q {
  secret_file     = %q
  redirect_uris   = [%q]
  id_token_claims = true
}

client %q {
  secret_file   = %q
  redirect_uris = ["http://127.0.0.1/callback"]
  audience      = [%q]
}
`, b.proxy.URL(), listen, key, q(retired), filepath.Join(b.dir, "salt"), bridgeTTL.String(),
		b.saml.SP.keyFile, b.saml.SP.certFile, b.saml.Metadata.URL, b.saml.Fed.certFile, colons(fingerprint(b.saml.Fed.cert)), idpEntity,
		adminClient, filepath.Join(b.dir, "admin.secret"), b.gwRedirect,
		apiClient, filepath.Join(b.dir, "api.secret"), apiClient)
}

func colons(hex string) string {
	var parts []string
	for i := 0; i+2 <= len(hex); i += 2 {
		parts = append(parts, hex[i:i+2])
	}
	return strings.Join(parts, ":")
}

// serve starts one authn-bridge process; it returns a stop function.
func (b *bridgeIdP) serve(t *testing.T, listen int, key string, retired []string) func() {
	t.Helper()
	cfg := writeFile(t, b.dir, fmt.Sprintf("bridge-%d-%s.hcl", listen, randomHex(3)), b.config(listen, key, retired), 0o600)
	if out, err := exec.Command(b.bin, "check", "--config", cfg).CombinedOutput(); err != nil {
		t.Fatalf("authn-bridge check: %v\n%s", err, out)
	}
	log := &bytes.Buffer{}
	cmd := exec.Command(b.bin, "--config", cfg)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cmd.Process.Kill()
			cmd.Wait()
		}
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("authn-bridge :%d log:\n%s", listen, log.String())
		}
	})
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/.well-known/openid-configuration", listen), 30*time.Second)
	return stop
}

func startBridge(t *testing.T, gwPort int) *bridgeIdP {
	t.Helper()
	bin := os.Getenv(envBridge)
	if bin == "" {
		t.Skipf("%s not set: no authn-bridge to test against", envBridge)
	}
	b := &bridgeIdP{t: t, bin: bin, proxy: NewProxy(t), dir: t.TempDir(), password: "pw-" + randomHex(12),
		apiSecret: randomHex(24), adminSecret: randomHex(24), gwRedirect: fmt.Sprintf("http://127.0.0.1:%d/admin/callback", gwPort)}
	b.saml = NewSAMLIdP(t, b.password)
	b.saml.SPEntity = b.proxy.URL() + "/saml"
	b.saml.ACS = b.proxy.URL() + "/saml/acs"
	writeFile(t, b.dir, "admin.secret", b.adminSecret+"\n", 0o600)
	writeFile(t, b.dir, "api.secret", b.apiSecret+"\n", 0o600)
	b.key = []string{b.keygen(t, "oidc-1.key", true)}
	b.listen = freePort(t)
	b.stop = b.serve(t, b.listen, b.key[0], nil)
	b.proxy.To(fmt.Sprintf("http://127.0.0.1:%d", b.listen))
	// Bridge 2: the same issuer, salt, SAML and clients, its own key. The
	// browser reaches it by rewriting the issuer's host:port.
	otherPort := freePort(t)
	b.otherKey = b.keygen(t, "other.key", false)
	b.serve(t, otherPort, b.otherKey, nil)
	b.other = fmt.Sprintf("127.0.0.1:%d", otherPort)
	return b
}

func (b *bridgeIdP) Browser(user string) *Browser { return NewBrowser(user, b.password) }

// apiToken runs the code flow for client against the provider at host
// (empty: the issuer itself) and returns one of the tokens.
func (b *bridgeIdP) apiToken(t *testing.T, client, secret, user, redirect, which, host string) string {
	t.Helper()
	br := b.Browser(user)
	if host != "" {
		br.Rewrite = map[string]string{strings.TrimPrefix(b.proxy.URL(), "http://"): host}
	}
	tok := br.CodeFlow(t, b.proxy.URL()+"/authorize", b.proxy.URL()+"/token", client, secret, redirect, "openid profile groups")
	s, _ := tok[which].(string)
	if s == "" {
		t.Fatalf("no %s in the token response", which)
	}
	return s
}

func (b *bridgeIdP) APIToken(t *testing.T, user string) string {
	return b.apiToken(t, apiClient, b.apiSecret, user, "http://127.0.0.1/callback", "access_token", "")
}

// WrongAudienceToken is an access token of the SSO client: its aud is
// llmflow-admin (the client ID, no audience configured).
func (b *bridgeIdP) WrongAudienceToken(t *testing.T, user string) string {
	return b.apiToken(t, adminClient, b.adminSecret, user, b.gwRedirect, "access_token", "")
}

func (b *bridgeIdP) OtherKeyToken(t *testing.T, user string) string {
	return b.apiToken(t, apiClient, b.apiSecret, user, "http://127.0.0.1/callback", "access_token", b.other)
}

// Rotate is the operator's rotation: keygen a new key, restart with it as
// signing_key_file and the old one in retired_signing_key_files.
func (b *bridgeIdP) Rotate(t *testing.T) {
	t.Helper()
	next := b.keygen(t, fmt.Sprintf("oidc-%d.key", len(b.key)+1), false)
	b.retired = append(b.retired, b.key[len(b.key)-1])
	b.key = append(b.key, next)
	b.stop()
	b.stop = b.serve(t, b.listen, next, b.retired)
}

func TestBridge(t *testing.T) {
	gwPort := freePort(t)
	b := startBridge(t, gwPort)
	g := StartGateway(t, gwPort, b.Issuer(), b.adminSecret)
	runChecks(t, b, b.proxy, g)
}
