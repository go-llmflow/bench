package oidc

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-authn/saml/samltest"
)

// go-authn/bridge (authn-bridge), configured from its README: issuer,
// signing_key_file and subject_salt_file from `authn-bridge keygen`, a
// saml block (SP key and certificate, federation metadata URL pinned by
// the fingerprint of its signing certificate, one IdP), claims (eppn ->
// preferred_username, eduPersonEntitlement -> groups), and clients. Key
// rotation is internal/app/config.go's retired_signing_key_files: a key
// listed there is published and never signs.
//
// The federation and the IdP are go-authn/saml/samltest: signed metadata,
// signed Responses, assertions encrypted to the bridge's SAML key.

const envBridge = "BENCH_BRIDGE_BIN" // authn-bridge binary

const (
	bridgeTTL = 120 * time.Second // token_lifetime
	idpEntity = "https://idp.univ-ex.example/idp/shibboleth"
	idpScope  = "univ-ex.example"
)

type bridgeIdP struct {
	bin                    string
	proxy                  *Proxy
	dir                    string
	gwRedirect             string
	password               string
	apiSecret, adminSecret string
	spKey, spCert          string
	fedCert, fingerprint   string
	metadataURL            string
	key                    string   // current signing key file
	retired                []string // published, never signing
	listen                 int
	stop                   func()
	other                  string // host:port of bridge 2 (same issuer, other key)
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
	var r []string
	for _, s := range retired {
		r = append(r, fmt.Sprintf("%q", s))
	}
	return fmt.Sprintf(`issuer            = %q
listen            = "127.0.0.1:%d"
signing_key_file  = %q
retired_signing_key_files = [%s]
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
`, b.proxy.URL(), listen, key, strings.Join(r, ", "), filepath.Join(b.dir, "salt"), bridgeTTL.String(),
		b.spKey, b.spCert, b.metadataURL, b.fedCert, b.fingerprint, idpEntity,
		adminClient, filepath.Join(b.dir, "admin.secret"), b.gwRedirect,
		apiClient, filepath.Join(b.dir, "api.secret"), apiClient)
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
	b := &bridgeIdP{bin: bin, proxy: NewProxy(t), dir: t.TempDir(), password: "pw-" + randomHex(12),
		apiSecret: randomHex(24), adminSecret: randomHex(24), gwRedirect: fmt.Sprintf("http://127.0.0.1:%d/admin/callback", gwPort)}

	// The federation and its IdP.
	fed, err := samltest.NewParty("federation")
	if err != nil {
		t.Fatal(err)
	}
	idpKey, _ := samltest.NewParty("idp")
	sp, _ := samltest.NewParty("sp")
	if b.spKey, b.spCert, err = sp.WriteFiles(b.dir, "sp"); err != nil {
		t.Fatal(err)
	}
	_, b.fedCert, _ = fed.WriteFiles(b.dir, "federation")
	b.fingerprint = fed.Fingerprint()
	idpSrv := httptest.NewServer(nil)
	t.Cleanup(idpSrv.Close)
	idp := &samltest.IdP{EntityID: idpEntity, Scope: idpScope, SSO: idpSrv.URL + "/SSO", Name: "Université d'exemple", Signer: idpKey}
	idpSrv.Config.Handler = idp.Handler(b.proxy.URL()+"/saml", b.proxy.URL()+"/saml/acs", sp.Cert, func(user, pw string) ([]samltest.Attribute, bool) {
		p, ok := people[user]
		if !ok || pw != b.password {
			return nil, false
		}
		eppn := p.name + "@" + idpScope
		return []samltest.Attribute{
			{Name: samltest.EPPN, Values: []string{eppn}},
			{Name: samltest.SubjectID, Values: []string{eppn}},
			{Name: samltest.Entitlement, Values: p.groups},
			{Name: samltest.DisplayName, Values: []string{p.name}},
			{Name: samltest.Mail, Values: []string{eppn}},
		}, true
	})
	md, err := (&samltest.Federation{Signer: fed, IdPs: []*samltest.IdP{idp}}).Metadata(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(md) }))
	t.Cleanup(mdSrv.Close)
	b.metadataURL = mdSrv.URL

	writeFile(t, b.dir, "admin.secret", b.adminSecret+"\n", 0o600)
	writeFile(t, b.dir, "api.secret", b.apiSecret+"\n", 0o600)
	b.key = b.keygen(t, "oidc-1.key", true)
	b.listen = freePort(t)
	b.stop = b.serve(t, b.listen, b.key, nil)
	b.proxy.To(fmt.Sprintf("http://127.0.0.1:%d", b.listen))
	// Bridge 2: the same issuer, salt, SAML and clients, its own key. The
	// browser reaches it by rewriting the issuer's host:port.
	otherPort := freePort(t)
	b.serve(t, otherPort, b.keygen(t, "other.key", false), nil)
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

// kids are the key IDs the provider publishes.
func (b *bridgeIdP) kids(t *testing.T) []string {
	t.Helper()
	var set struct {
		Keys []struct{ Kid string } `json:"keys"`
	}
	getJSON(t, fmt.Sprintf("http://127.0.0.1:%d/jwks", b.listen), &set)
	var out []string
	for _, k := range set.Keys {
		out = append(out, k.Kid)
	}
	return out
}

// Rotate is the operator's rotation, in two steps:
//
//  1. keygen the next key and PUBLISH it first, in retired_signing_key_files
//     (published, never signing), restart: relying parties that cache the
//     key set learn it before any token is signed with it;
//  2. make it signing_key_file and move the old key to
//     retired_signing_key_files, restart: tokens signed by the old key
//     still verify until they expire.
func (b *bridgeIdP) Rotate(t *testing.T) {
	t.Helper()
	before := b.kids(t)
	next := b.keygen(t, fmt.Sprintf("oidc-%s.key", randomHex(3)), false)
	b.stop()
	b.stop = b.serve(t, b.listen, b.key, append(append([]string{}, b.retired...), next))
	published := b.kids(t)
	signing, _ := jwtParts(t, b.APIToken(t, "bob"))
	t.Logf("step 1, next key published: JWKS kids %v -> %v; tokens still signed by %v", before, published, signing["kid"])
	if len(published) != len(before)+1 {
		t.Fatalf("step 1 did not publish the next key: %v -> %v", before, published)
	}
	if !contains(before, signing["kid"]) {
		t.Fatalf("step 1 signs with a new key already: %v", signing["kid"])
	}
	b.retired = append(b.retired, b.key)
	b.key = next
	b.stop()
	b.stop = b.serve(t, b.listen, b.key, b.retired)
	after := b.kids(t)
	t.Logf("step 2, next key signing: JWKS kids %v", after)
	if len(after) != len(published) {
		t.Fatalf("step 2 changed the published set: %v -> %v", published, after)
	}
}

func contains(list []string, v any) bool {
	s, _ := v.(string)
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestBridge(t *testing.T) {
	gwPort := freePort(t)
	b := startBridge(t, gwPort)
	g := StartGateway(t, gwPort, b.Issuer(), b.adminSecret)
	runChecks(t, b, b.proxy, g)
}
