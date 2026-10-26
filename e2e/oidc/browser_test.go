package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Browser is a minimal user agent: it follows redirects by hand, keeps
// cookies per host:port and sends them back, and submits the forms it is
// shown. It ignores the Secure attribute on loopback, as browsers do
// (127.0.0.0/8 is a potentially trustworthy origin, W3C Secure Contexts
// §3.1 step 4), because Go's cookiejar would drop the gateway's __Host-
// cookies on http://127.0.0.1.
type Browser struct {
	jar  map[string]map[string]*http.Cookie // host -> name -> cookie
	c    *http.Client
	Seen []*http.Cookie // every Set-Cookie received, for attribute checks
	// Rewrite sends requests for one host:port to another: a second
	// provider that believes it has the first one's issuer URL.
	Rewrite map[string]string
	// User and Password fill a login form (fields "login"/"password" for
	// dex, "username"/"password" for the test SAML IdP).
	User, Password string
}

func NewBrowser(user, password string) *Browser {
	return &Browser{jar: map[string]map[string]*http.Cookie{}, User: user, Password: password, c: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *Browser) Cookie(host, name string) *http.Cookie { return b.jar[host][name] }

// Do sends one request with the cookies of its host and records the answer's.
func (b *Browser) Do(req *http.Request) (*http.Response, string, error) {
	if to, ok := b.Rewrite[req.URL.Host]; ok {
		req.URL.Host, req.Host = to, to
	}
	for _, c := range b.jar[req.URL.Host] {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	resp, err := b.c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	for _, c := range resp.Cookies() {
		b.Seen = append(b.Seen, c)
		if b.jar[req.URL.Host] == nil {
			b.jar[req.URL.Host] = map[string]*http.Cookie{}
		}
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.jar[req.URL.Host], c.Name)
		} else {
			b.jar[req.URL.Host][c.Name] = c
		}
	}
	return resp, string(body), nil
}

func (b *Browser) Get(u string) (*http.Response, string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	return b.Do(req)
}

var (
	formRe   = regexp.MustCompile(`(?is)<form([^>]*)>(.*?)</form>`)
	attrRe   = regexp.MustCompile(`(?is)\b(action|method)="([^"]*)"`)
	inputRe  = regexp.MustCompile(`(?is)<input([^>]*)>`)
	nameRe   = regexp.MustCompile(`(?is)\bname="([^"]*)"`)
	valueRe  = regexp.MustCompile(`(?is)\bvalue="([^"]*)"`)
	buttonRe = regexp.MustCompile(`(?is)<button([^>]*)>`)
)

// Until follows redirects and submits forms, starting at start, until a
// redirect points under stop; it returns that URL without requesting it.
// A form's hidden inputs are sent as they are; a login form gets User and
// Password.
func (b *Browser) Until(t *testing.T, start, stop string) string {
	t.Helper()
	next := start
	var resp *http.Response
	var body string
	var err error
	method, form := http.MethodGet, url.Values(nil)
	for hops := 0; hops < 20; hops++ {
		if strings.HasPrefix(next, stop) {
			return next
		}
		var req *http.Request
		if method == http.MethodPost {
			req, _ = http.NewRequest(method, next, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req, _ = http.NewRequest(method, next, nil)
		}
		// Relative URLs resolve against the address the page believes it
		// has, before any Rewrite.
		cp := *req.URL
		here := &cp
		resp, body, err = b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode/100 == 3 {
			loc, err := here.Parse(resp.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			next, method, form = loc.String(), http.MethodGet, nil
			continue
		}
		m := formRe.FindStringSubmatch(body)
		if resp.StatusCode != 200 || m == nil {
			t.Fatalf("%s %s: HTTP %d, no form:\n%.600s", req.Method, req.URL, resp.StatusCode, body)
		}
		action := ""
		method = http.MethodPost
		for _, a := range attrRe.FindAllStringSubmatch(m[1], -1) {
			switch strings.ToLower(a[1]) {
			case "action":
				action = html.UnescapeString(a[2])
			case "method":
				method = strings.ToUpper(a[2])
			}
		}
		form = url.Values{}
		for _, in := range append(inputRe.FindAllStringSubmatch(m[2], -1), buttonRe.FindAllStringSubmatch(m[2], -1)...) {
			n := nameRe.FindStringSubmatch(in[1])
			if n == nil {
				continue
			}
			v := ""
			if x := valueRe.FindStringSubmatch(in[1]); x != nil {
				v = html.UnescapeString(x[1])
			}
			switch n[1] {
			case "login", "username":
				v = b.User
			case "password":
				v = b.Password
			}
			if _, dup := form[n[1]]; !dup {
				form.Set(n[1], v)
			}
		}
		u, err := here.Parse(action)
		if err != nil || action == "" {
			u = here
		}
		next = u.String()
		if method == http.MethodGet {
			u.RawQuery = form.Encode()
			next, form = u.String(), nil
		}
	}
	t.Fatalf("no redirect under %s", stop)
	return ""
}

// Login runs the gateway's SSO from GET /admin/login up to (not including)
// its callback, and returns the callback URL so that a test can call it,
// and call it again (replay). It checks the request carries PKCE S256, a
// state and a nonce.
func (b *Browser) Login(t *testing.T, gw string) (callback string) {
	t.Helper()
	resp, _, err := b.Get(gw + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode/100 != 3 {
		t.Fatalf("/admin/login: HTTP %d, want a redirect to the provider", resp.StatusCode)
	}
	auth, _ := url.Parse(resp.Header.Get("Location"))
	if q := auth.Query(); q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorization request lacks PKCE S256, state or nonce: %s", auth)
	}
	return b.Until(t, auth.String(), gw+"/admin/callback")
}

// CodeFlow is a confidential client's authorization-code login with PKCE
// S256 (what an API client does to get an access token from a provider
// that has no password grant). It returns the token response.
func (b *Browser) CodeFlow(t *testing.T, authorize, tokenURL, client, secret, redirect, scope string) map[string]any {
	t.Helper()
	verifier := randomHex(32)
	sum := sha256.Sum256([]byte(verifier))
	state, nonce := randomHex(8), randomHex(8)
	q := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirect}, "scope": {scope},
		"state": {state}, "nonce": {nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	back := b.Until(t, authorize+"?"+q.Encode(), redirect)
	u, _ := url.Parse(back)
	if u.Query().Get("state") != state || u.Query().Get("code") == "" {
		t.Fatalf("redirect %s: wrong state or no code", back)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	req, _ := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(client), url.QueryEscape(secret))
	resp, body, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok map[string]any
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &tok) != nil || tok["access_token"] == nil {
		t.Fatalf("token endpoint: HTTP %d %s", resp.StatusCode, body)
	}
	return tok
}

// Session calls a gateway route with the browser's cookies; for a state
// change it adds the CSRF token and the gateway's Origin.
func (b *Browser) Session(t *testing.T, method, gw, path, csrf, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, gw+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Origin", gw)
	}
	resp, out, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func describe(c *http.Cookie) string {
	return fmt.Sprintf("%s Secure=%v HttpOnly=%v SameSite=%v Path=%q Domain=%q", c.Name, c.Secure, c.HttpOnly, c.SameSite, c.Path, c.Domain)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}
