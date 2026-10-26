package oidc

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"math/big"
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

// A test SAML federation and IdP, after go-authn/bridge's own harness
// (internal/app/harness_test.go, v0.23.4), which is unexported test code:
// the federation's metadata and every Response are signed, and every
// assertion encrypted (AES-128-GCM, RSA-OAEP: a Shibboleth IdP v5's
// defaults), by xmlsec1 -- nothing on the SAML side is produced by
// go-authn/saml, which only reads. Attributes are eduPersonPrincipalName
// (urn:oid:1.3.6.1.4.1.5923.1.1.1.6), subject-id and eduPersonEntitlement
// (urn:oid:1.3.6.1.4.1.5923.1.1.1.7), whose values are the person's groups.

const (
	idpEntity = "https://idp.univ-ex.example/idp/shibboleth"
	idpScope  = "univ-ex.example"
)

type party struct {
	cert              *x509.Certificate
	keyFile, certFile string
}

func newParty(t *testing.T, dir, name string) *party {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	p := &party{cert: c, keyFile: filepath.Join(dir, name+".key"), certFile: filepath.Join(dir, name+".crt")}
	os.WriteFile(p.keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	os.WriteFile(p.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	return p
}

func fingerprint(c *x509.Certificate) string {
	s := sha256.Sum256(c.Raw)
	return strings.ToUpper(hex.EncodeToString(s[:]))
}

type xmlsec struct {
	t    *testing.T
	path string
	lax  []string
	dir  string
	mu   sync.Mutex
	n    int
}

func newXMLSec(t *testing.T, dir string) *xmlsec {
	t.Helper()
	p, err := exec.LookPath("xmlsec1")
	if err != nil {
		t.Skip("xmlsec1 is not installed: it signs the test IdP's SAML")
	}
	x := &xmlsec{t: t, path: p, dir: dir}
	if out, _ := exec.Command(p, "--help-sign").CombinedOutput(); strings.Contains(string(out), "--lax-key-search") {
		x.lax = []string{"--lax-key-search"}
	}
	return x
}

func (x *xmlsec) run(cmd string, args ...string) (string, error) {
	all := append(append([]string{cmd}, x.lax...), args...)
	out, err := exec.Command(x.path, all...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("xmlsec1 %s: %v\n%s", strings.Join(all, " "), err, out)
	}
	s := strings.TrimSpace(string(out))
	if strings.HasPrefix(s, "<?xml") {
		s = strings.TrimSpace(s[strings.Index(s, "?>")+2:])
	}
	return s, nil
}

func (x *xmlsec) file(name, content string) string {
	x.mu.Lock()
	x.n++
	p := filepath.Join(x.dir, fmt.Sprintf("%d-%s", x.n, name))
	x.mu.Unlock()
	os.WriteFile(p, []byte(content), 0o644)
	return p
}

const sigTemplate = `<ds:Signature xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:SignedInfo><ds:CanonicalizationMethod Algorithm="http://www.w3.org/2001/10/xml-exc-c14n#"/><ds:SignatureMethod Algorithm="http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"/><ds:Reference URI="#%s"><ds:Transforms><ds:Transform Algorithm="http://www.w3.org/2000/09/xmldsig#enveloped-signature"/><ds:Transform Algorithm="http://www.w3.org/2001/10/xml-exc-c14n#"/></ds:Transforms><ds:DigestMethod Algorithm="http://www.w3.org/2001/04/xmlenc#sha256"/><ds:DigestValue/></ds:Reference></ds:SignedInfo><ds:SignatureValue/><ds:KeyInfo><ds:X509Data/></ds:KeyInfo></ds:Signature>`

// SAMLIdP is a federation (signed metadata) with one IdP that logs people
// in with a password form and answers the SP's AuthnRequest.
type SAMLIdP struct {
	t        *testing.T
	x        *xmlsec
	Fed      *party // signs the federation metadata
	idp, SP  *party
	Metadata *httptest.Server
	srv      *httptest.Server
	password string
	// SPEntity and ACS are the bridge's, set before the first login.
	SPEntity, ACS string
}

func NewSAMLIdP(t *testing.T, password string) *SAMLIdP {
	t.Helper()
	dir := t.TempDir()
	s := &SAMLIdP{t: t, x: newXMLSec(t, dir), password: password}
	s.Fed, s.idp, s.SP = newParty(t, dir, "federation"), newParty(t, dir, "idp"), newParty(t, dir, "sp")
	s.srv = httptest.NewServer(http.HandlerFunc(s.sso))
	t.Cleanup(s.srv.Close)
	md := `<md:EntitiesDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:shibmd="urn:mace:shibboleth:metadata:1.0" xmlns:mdui="urn:oasis:names:tc:SAML:metadata:ui" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" ID="_fed" validUntil="` +
		time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) + `">` + fmt.Sprintf(sigTemplate, "_fed") +
		`<md:EntityDescriptor entityID="` + idpEntity + `"><md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">` +
		`<md:Extensions><shibmd:Scope regexp="false">` + idpScope + `</shibmd:Scope><mdui:UIInfo><mdui:DisplayName xml:lang="fr">Université d'exemple</mdui:DisplayName></mdui:UIInfo></md:Extensions>` +
		`<md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + base64.StdEncoding.EncodeToString(s.idp.cert.Raw) + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>` +
		`<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="` + s.srv.URL + `/SSO"/></md:IDPSSODescriptor></md:EntityDescriptor>` +
		`</md:EntitiesDescriptor>`
	signed, err := s.x.run("--sign", "--privkey-pem", s.Fed.keyFile+","+s.Fed.certFile,
		"--id-attr:ID", "urn:oasis:names:tc:SAML:2.0:metadata:EntitiesDescriptor", "--output", "/dev/stdout", s.x.file("md.xml", md))
	if err != nil {
		t.Fatal(err)
	}
	s.Metadata = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, signed) }))
	t.Cleanup(s.Metadata.Close)
	return s
}

// sso: GET shows the login form for an AuthnRequest (HTTP-Redirect
// binding); POST checks the password and answers with an auto-submitted
// form to the SP's ACS (HTTP-POST binding).
func (s *SAMLIdP) sso(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	req, relay := r.Form.Get("SAMLRequest"), r.Form.Get("RelayState")
	if r.Method == http.MethodGet {
		fmt.Fprintf(w, `<html><body><form method="post" action="/SSO"><input type="hidden" name="SAMLRequest" value="%s"><input type="hidden" name="RelayState" value="%s"><input name="username"><input type="password" name="password"><button type="submit">Log in</button></form></body></html>`,
			html.EscapeString(req), html.EscapeString(relay))
		return
	}
	who, ok := people[r.Form.Get("username")]
	if !ok || r.Form.Get("password") != s.password {
		http.Error(w, "wrong user or password", http.StatusUnauthorized)
		return
	}
	id, err := requestID(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := s.respond(id, who)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, `<html><body><form method="post" action="%s"><input type="hidden" name="SAMLResponse" value="%s"><input type="hidden" name="RelayState" value="%s"><noscript><button type="submit">Continue</button></noscript></form></body></html>`,
		html.EscapeString(s.ACS), resp, html.EscapeString(relay))
}

// requestID reads the ID out of a deflated, base64 AuthnRequest.
func requestID(q string) (string, error) {
	z, err := base64.StdEncoding.DecodeString(q)
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(z)))
	if err != nil {
		return "", err
	}
	s := string(raw)
	i := strings.Index(s, ` ID="`)
	if i < 0 {
		return "", fmt.Errorf("no ID in the AuthnRequest")
	}
	s = s[i+5:]
	return s[:strings.Index(s, `"`)], nil
}

func (s *SAMLIdP) respond(requestID string, who person) (string, error) {
	now := time.Now().UTC()
	ts, later := now.Add(-time.Second).Format(time.RFC3339), now.Add(5*time.Minute).Format(time.RFC3339)
	attr := func(name string, vals ...string) string {
		if len(vals) == 0 {
			return ""
		}
		a := `<saml:Attribute Name="` + name + `" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:uri">`
		for _, v := range vals {
			a += `<saml:AttributeValue>` + html.EscapeString(v) + `</saml:AttributeValue>`
		}
		return a + `</saml:Attribute>`
	}
	eppn := who.name + "@" + idpScope
	assertion := `<saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="_a` + randomHex(10) + `" Version="2.0" IssueInstant="` + ts + `">` +
		`<saml:Issuer>` + idpEntity + `</saml:Issuer>` +
		`<saml:Subject><saml:NameID Format="urn:oasis:names:tc:SAML:2.0:nameid-format:transient">t` + randomHex(4) + `</saml:NameID>` +
		`<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData Recipient="` + s.ACS + `" InResponseTo="` + requestID + `" NotOnOrAfter="` + later + `"/></saml:SubjectConfirmation></saml:Subject>` +
		`<saml:Conditions NotBefore="` + ts + `" NotOnOrAfter="` + later + `"><saml:AudienceRestriction><saml:Audience>` + s.SPEntity + `</saml:Audience></saml:AudienceRestriction></saml:Conditions>` +
		`<saml:AuthnStatement AuthnInstant="` + ts + `" SessionIndex="_s` + randomHex(4) + `"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>` +
		`<saml:AttributeStatement>` +
		attr("urn:oid:1.3.6.1.4.1.5923.1.1.1.6", eppn) +
		attr("urn:oasis:names:tc:SAML:attribute:subject-id", eppn) +
		attr("urn:oid:1.3.6.1.4.1.5923.1.1.1.7", who.groups...) +
		attr("urn:oid:2.16.840.1.113730.3.1.241", who.name) +
		attr("urn:oid:0.9.2342.19200300.100.1.3", who.name+"@"+idpScope) +
		`</saml:AttributeStatement></saml:Assertion>`
	tmpl := s.x.file("enc.xml", `<xenc:EncryptedData xmlns:xenc="http://www.w3.org/2001/04/xmlenc#" Type="http://www.w3.org/2001/04/xmlenc#Element"><xenc:EncryptionMethod Algorithm="http://www.w3.org/2009/xmlenc11#aes128-gcm"/><ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><xenc:EncryptedKey><xenc:EncryptionMethod Algorithm="http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p"/><xenc:CipherData><xenc:CipherValue/></xenc:CipherData></xenc:EncryptedKey></ds:KeyInfo><xenc:CipherData><xenc:CipherValue/></xenc:CipherData></xenc:EncryptedData>`)
	data := s.x.file("data.xml", `<saml:EncryptedAssertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion">`+assertion+`</saml:EncryptedAssertion>`)
	body, err := s.x.run("--encrypt", "--pubkey-cert-pem", s.SP.certFile, "--session-key", "aes-128",
		"--xml-data", data, "--node-name", "urn:oasis:names:tc:SAML:2.0:assertion:Assertion", "--output", "/dev/stdout", tmpl)
	if err != nil {
		return "", err
	}
	rid := "_r" + randomHex(10)
	resp := `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="` + rid + `" Version="2.0" IssueInstant="` + ts + `" Destination="` + s.ACS + `" InResponseTo="` + requestID + `">` +
		`<saml:Issuer>` + idpEntity + `</saml:Issuer>` + fmt.Sprintf(sigTemplate, rid) +
		`<samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>` + body + `</samlp:Response>`
	signed, err := s.x.run("--sign", "--privkey-pem", s.idp.keyFile+","+s.idp.certFile,
		"--id-attr:ID", "urn:oasis:names:tc:SAML:2.0:protocol:Response", "--output", "/dev/stdout", s.x.file("resp.xml", resp))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(signed)), nil
}
