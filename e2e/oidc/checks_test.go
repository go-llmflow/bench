package oidc

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runChecks runs every check against one provider and one gateway, in an
// order that respects the provider's timing (token TTL, key rotation) and
// the gateway's min_refresh window. What the verifier must enforce is OIDC
// Core 1.0 §3.1.3.7: iss exact (item 2), aud contains the client (item 3),
// the signature with the header's alg (items 6-7), exp (item 9); §2
// forbids alg none for ID tokens.
func runChecks(t *testing.T, idp IdP, proxy *Proxy, g *Gateway) {
	ctx := context.Background()
	host := strings.TrimPrefix(g.URL, "http://")
	alice := idp.APIToken(t, "alice")
	bob := idp.APIToken(t, "bob")
	hdr, claims := jwtParts(t, alice)
	t.Logf("%s access token header %v; iss=%v aud=%v groups=%v preferred_username=%v lifetime=%vs",
		idp.Name(), hdr, claims["iss"], claims["aud"], claims["groups"], claims["preferred_username"], num(claims["exp"])-num(claims["iat"]))
	expiring := alice
	expAt := time.Unix(int64(num(claims["exp"])), 0)
	var generic string
	var lastFlood time.Time

	t.Run("a/accepted-and-charged", func(t *testing.T) {
		for _, c := range []struct{ who, tok string }{{"alice", alice}, {"bob", bob}} {
			if code, body := g.Chat(t, c.tok); code != 200 {
				t.Fatalf("%s's token: HTTP %d %s", c.who, code, body)
			}
		}
		// The gateway's own counter, by team.
		_, metrics := g.Get(t, "/metrics")
		for _, team := range []string{teamA, teamB} {
			if !strings.Contains(metrics, `llmflow_requests_total{team="`+team+`",model="chat",code="200"} 1`) {
				t.Errorf("llmflow_requests_total has no 200 for %s", team)
			}
		}
		// The spend report reads the stored records, written asynchronously:
		// poll a little.
		day := time.Now().UTC()
		q := "/admin/spend?group_by=org,team&format=json&from=" + day.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + day.AddDate(0, 0, 1).Format("2006-01-02")
		var body string
		for i := 0; i < 30; i++ {
			_, body = g.Admin(t, "GET", q, "")
			if spendHas(body, orgA, teamA) && spendHas(body, orgB, teamB) {
				break
			}
			sleep(ctx, 500*time.Millisecond)
		}
		t.Logf("spend: %s", body)
		for _, want := range [][2]string{{orgA, teamA}, {orgB, teamB}} {
			if !spendHas(body, want[0], want[1]) {
				t.Errorf("no request charged to %s/%s in the spend report after 15 s", want[0], want[1])
			}
		}
	})

	t.Run("b/refusals", func(t *testing.T) {
		code, body := g.Chat(t, "not-a-token")
		if code != 401 {
			t.Fatalf("garbage bearer: HTTP %d", code)
		}
		generic = body
		refuse := func(name, tok string) {
			t.Helper()
			code, body := g.Chat(t, tok)
			t.Logf("%-24s HTTP %d", name, code)
			if code != 401 || body != generic {
				t.Errorf("%s: HTTP %d %q, want 401 %q", name, code, body, generic)
			}
		}
		refuse("wrong audience", idp.WrongAudienceToken(t, "alice"))
		refuse("signed by another key", idp.OtherKeyToken(t, "alice"))
		refuse("alg none", b64(map[string]any{"alg": "none", "kid": hdr["kid"], "typ": hdr["typ"]})+"."+b64(claims)+".")
		pub := publicKey(t, idp.Issuer(), hdr["kid"].(string))
		der, _ := x509.MarshalPKIXPublicKey(pub)
		pemKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		for _, s := range []struct {
			name   string
			secret []byte
		}{{"HS256 keyed with PEM", pemKey}, {"HS256 keyed with DER", der}} {
			in := b64(map[string]any{"alg": "HS256", "kid": hdr["kid"], "typ": hdr["typ"]}) + "." + b64(claims)
			m := hmac.New(sha256.New, s.secret)
			m.Write([]byte(in))
			refuse(s.name, in+"."+base64.RawURLEncoding.EncodeToString(m.Sum(nil)))
		}
		// Unknown-kid flood: 40 tokens signed by a key nobody published.
		// Wait out the window the refusals above may have opened, so that
		// exactly one fetch is allowed: 0 would mean the counter is blind,
		// more than 1 that the bound does not hold.
		sleep(ctx, minRefresh+time.Second)
		own, _ := rsa.GenerateKey(rand.Reader, 2048)
		before := proxy.JWKSFetches.Load()
		for i := 0; i < 40; i++ {
			tok := signRS256(own, map[string]any{"alg": "RS256", "typ": hdr["typ"], "kid": "flood-" + randomHex(6)}, claims)
			if code, body := g.Chat(t, tok); code != 401 || body != generic {
				t.Fatalf("flood token %d: HTTP %d %q", i, code, body)
			}
		}
		lastFlood = time.Now()
		fetched := proxy.JWKSFetches.Load() - before
		t.Logf("unknown-kid flood: 40 tokens, %d key-set fetches at the provider (min_refresh %v)", fetched, minRefresh)
		if fetched != 1 {
			t.Errorf("%d key-set fetches for one flood after a quiet min_refresh, want exactly one", fetched)
		}
	})

	t.Run("d/sso", func(t *testing.T) {
		b := idp.Browser("alice")
		cb := b.Login(t, g.URL)
		resp, body, err := b.Get(cb)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode/100 != 3 {
			t.Fatalf("callback: HTTP %d %s", resp.StatusCode, body)
		}
		sess := b.Cookie(host, "__Host-llmflow-session")
		if sess == nil {
			t.Fatalf("no __Host-llmflow-session cookie after the callback")
		}
		for _, c := range b.Seen {
			if c.Name == "__Host-llmflow-session" {
				t.Logf("session cookie: %s", describe(c))
				if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
					t.Errorf("session cookie attributes: %s", describe(c))
				}
			}
		}
		code, who := b.Session(t, "GET", g.URL, "/admin/whoami", "", "")
		t.Logf("whoami: HTTP %d %s", code, who)
		if code != 200 || !strings.Contains(who, "org_admin") || !strings.Contains(who, orgA) {
			t.Fatalf("whoami lacks org_admin on %s", orgA)
		}
		resp, body, _ = b.Get(cb)
		t.Logf("replayed callback, same browser: HTTP %d %s", resp.StatusCode, strings.TrimSpace(body))
		if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 {
			t.Errorf("replayed state accepted: HTTP %d", resp.StatusCode)
		}
		other := NewBrowser("", "")
		resp, _, _ = other.Get(cb)
		t.Logf("callback in another browser: HTTP %d", resp.StatusCode)
		if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 || other.Cookie(host, "__Host-llmflow-session") != nil {
			t.Errorf("callback accepted in a browser that did not start the login: HTTP %d", resp.StatusCode)
		}
		if code, _ := b.Session(t, "POST", g.URL, "/admin/logout", "", ""); code != 403 {
			t.Errorf("logout without CSRF token: HTTP %d, want 403", code)
		}
		csrf := csrfToken(t, b, g.URL)
		code, out := b.Session(t, "POST", g.URL, "/admin/logout", csrf, "")
		t.Logf("logout: HTTP %d %s", code, out)
		if code/100 != 2 {
			t.Fatalf("logout: HTTP %d", code)
		}
		b.jar[host]["__Host-llmflow-session"] = sess // a copy kept by an attacker
		if code, _ := b.Session(t, "GET", g.URL, "/admin/whoami", "", ""); code != 401 {
			t.Errorf("whoami with the logged-out cookie: HTTP %d, want 401", code)
		}
		bb := idp.Browser("bob")
		resp, body, _ = bb.Get(bb.Login(t, g.URL))
		t.Logf("bob (no role binding) at the callback: HTTP %d %s", resp.StatusCode, strings.TrimSpace(body))
		if bb.Cookie(host, "__Host-llmflow-session") != nil {
			t.Errorf("bob, with no role, got a session")
		}
	})

	t.Run("e/org-admin-cannot-see-other-org", func(t *testing.T) {
		b := login(t, idp, g, "alice")
		csrf := csrfToken(t, b, g.URL)
		if _, all := g.Admin(t, "GET", "/admin/orgs", ""); !strings.Contains(all, orgB) {
			t.Fatalf("control: the master key does not see %s: %s", orgB, all)
		}
		day := time.Now().UTC()
		span := "&from=" + day.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + day.AddDate(0, 0, 1).Format("2006-01-02")
		if _, all := g.Admin(t, "GET", "/admin/spend?group_by=org,team&format=json"+span, ""); !strings.Contains(all, orgB) {
			t.Logf("control: the master key's spend report lacks %s, so the spend rows below prove nothing", orgB)
		}
		for _, p := range []string{"/admin/orgs", "/admin/teams", "/admin/teams?org=" + orgB, "/admin/keys?team=" + teamB,
			"/admin/spend?group_by=org,team&format=json" + span, "/admin/budgets", "/admin/fairshare", "/admin/users",
			"/admin/teams/" + teamB + "/projects", "/admin/teams/" + teamB + "/models", "/admin/teams/" + teamB + "/tools",
			"/admin/allowlists", "/admin/audit"} {
			code, body := b.Session(t, "GET", g.URL, p, "", "")
			leak := strings.Contains(body, orgB) || strings.Contains(body, teamB)
			t.Logf("GET %-50s HTTP %d leak=%v", p, code, leak)
			if code == 200 && leak {
				t.Errorf("org_admin of %s sees %s/%s at %s: %.300s", orgA, orgB, teamB, p, body)
			}
		}
		// Control of the writes: the same body on the own organisation's
		// team is accepted, so a 403 below is the scope, not the shape.
		if code, body := b.Session(t, "PUT", g.URL, "/admin/budgets", csrf, `{"kind":"team","name":"`+teamA+`","period":"day","max_cost":1000}`); code/100 != 2 {
			t.Errorf("control: org_admin of %s cannot set a budget on its own team %s: HTTP %d %s", orgA, teamA, code, body)
		}
		for _, w := range []struct{ method, path, body string }{
			{"POST", "/admin/keys", `{"team":"` + teamB + `","name":"idor"}`},
			{"POST", "/admin/teams", `{"name":"idor-team","org":"` + orgB + `"}`},
			{"DELETE", "/admin/teams/" + teamB, ""},
			{"PUT", "/admin/users/" + idp.GatewayName("alice") + "/roles", `{"role":"org_admin","org":"` + orgB + `"}`},
			{"PUT", "/admin/budgets", `{"kind":"team","name":"` + teamB + `","period":"day","max_cost":0.001}`},
			{"PUT", "/admin/budgets", `{"kind":"org","name":"` + orgB + `","period":"day","max_cost":0.001}`},
		} {
			code, body := b.Session(t, w.method, g.URL, w.path, csrf, w.body)
			t.Logf("%-6s %-28s HTTP %d %s", w.method, w.path, code, strings.TrimSpace(body))
			if code/100 == 2 {
				t.Errorf("org_admin of %s could %s %s", orgA, w.method, w.path)
			}
		}
		code, home := b.Session(t, "GET", g.URL, "/ui/", "", "")
		if code != 200 {
			t.Fatalf("/ui/: HTTP %d", code)
		}
		pages := map[string]bool{"/ui/": true}
		for _, m := range uiLink.FindAllStringSubmatch(home, -1) {
			pages[strings.SplitN(m[1], "?", 2)[0]] = true
		}
		seen := 0
		for p := range pages {
			for _, q := range []string{"", "?team=" + teamB, "?org=" + orgB, "?group_by=org,team" + span} {
				code, body := b.Session(t, "GET", g.URL, p+q, "", "")
				if code == 200 {
					seen++
				}
				if code == 200 && (strings.Contains(body, orgB) || strings.Contains(body, teamB)) {
					t.Errorf("UI %s%s shows %s/%s to the org_admin of %s", p, q, orgB, teamB, orgA)
				}
			}
		}
		t.Logf("UI: %d pages, %d answers checked", len(pages), seen)
	})

	t.Run("f/scim", func(t *testing.T) {
		pre := idp.Browser("carol")
		pre.Get(pre.Login(t, g.URL))
		if pre.Cookie(host, "__Host-llmflow-session") != nil {
			t.Fatalf("carol got a session before any provisioning")
		}
		carol := idp.GatewayName("carol")
		id := scim(t, g, "POST", "/scim/v2/Users", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"`+carol+`","active":true}`)
		scim(t, g, "POST", "/scim/v2/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Beta Leads","members":[{"value":"`+id+`"}]}`)
		code, roles := g.Admin(t, "GET", "/admin/users/"+carol+"/roles", "")
		t.Logf("carol's roles after SCIM: HTTP %d %s", code, roles)
		if code != 200 || !strings.Contains(roles, "team_admin") || !strings.Contains(roles, teamB) {
			t.Fatalf("the SCIM group did not give team_admin on %s", teamB)
		}
		b := login(t, idp, g, "carol")
		code, who := b.Session(t, "GET", g.URL, "/admin/whoami", "", "")
		t.Logf("carol's whoami: HTTP %d %s", code, who)
		if code != 200 || !strings.Contains(who, "team_admin") {
			t.Fatalf("carol's session lacks the provisioned role")
		}
	})

	t.Run("b/expired", func(t *testing.T) {
		wait := time.Until(expAt.Add(clockSkew + 3*time.Second))
		t.Logf("waiting %v for the first token to expire (exp %s + skew %v)", wait.Round(time.Second), expAt.Format("15:04:05"), clockSkew)
		sleep(ctx, wait)
		code, body := g.Chat(t, expiring)
		t.Logf("expired token: HTTP %d", code)
		if code != 401 || body != generic {
			t.Errorf("expired token: HTTP %d %q", code, body)
		}
	})

	t.Run("c/key-rotation", func(t *testing.T) {
		t1 := idp.APIToken(t, "alice")
		h1, _ := jwtParts(t, t1)
		idp.Rotate(t)
		t2 := idp.APIToken(t, "bob")
		h2, _ := jwtParts(t, t2)
		if h2["kid"] == h1["kid"] {
			t.Fatalf("still the same kid %v after the rotation", h1["kid"])
		}
		t.Logf("%s rotated: kid %v -> %v", idp.Name(), h1["kid"], h2["kid"])
		sleep(ctx, time.Until(lastFlood.Add(minRefresh+time.Second)))
		before := proxy.JWKSFetches.Load()
		if code, body := g.Chat(t, t1); code != 200 {
			t.Errorf("token signed with the previous key, not expired: HTTP %d %s", code, body)
		}
		if code, body := g.Chat(t, t2); code != 200 {
			t.Errorf("token signed with the new key: HTTP %d %s", code, body)
		}
		fetched := proxy.JWKSFetches.Load() - before
		t.Logf("old and new tokens accepted with %d key-set fetch(es)", fetched)
		if fetched > 1 {
			t.Errorf("%d key-set fetches to learn one new key", fetched)
		}
		// SSO verifies ID tokens with its own key set: a login right after
		// the rotation must work too.
		b := idp.Browser("alice")
		resp, body, _ := b.Get(b.Login(t, g.URL))
		t.Logf("SSO login after the rotation: HTTP %d %s", resp.StatusCode, strings.TrimSpace(body))
		if b.Cookie(host, "__Host-llmflow-session") == nil {
			t.Errorf("SSO login with an ID token signed by the new key failed: HTTP %d", resp.StatusCode)
		}
	})
}

var uiLink = regexp.MustCompile(`href="(/ui/[^"]*)"`)

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// spendHas reports whether the JSON spend report has a row for org/team
// with at least one request.
func spendHas(body, org, team string) bool {
	var rows []map[string]any
	if json.Unmarshal([]byte(body), &rows) != nil {
		var wrapped map[string]json.RawMessage
		if json.Unmarshal([]byte(body), &wrapped) != nil {
			return false
		}
		for _, raw := range wrapped {
			var r []map[string]any
			if json.Unmarshal(raw, &r) == nil {
				rows = append(rows, r...)
			}
		}
	}
	for _, r := range rows {
		if r["org"] == org && r["team"] == team && num(r["requests"]) >= 1 {
			return true
		}
	}
	return false
}

// publicKey fetches the provider's RSA key kid through discovery.
func publicKey(t *testing.T, issuer, kid string) *rsa.PublicKey {
	t.Helper()
	var disc struct {
		JWKS string `json:"jwks_uri"`
	}
	getJSON(t, issuer+"/.well-known/openid-configuration", &disc)
	var set struct {
		Keys []struct{ Kid, Kty, N, E string } `json:"keys"`
	}
	getJSON(t, disc.JWKS, &set)
	for _, k := range set.Keys {
		if k.Kid == kid && k.Kty == "RSA" {
			n, _ := base64.RawURLEncoding.DecodeString(k.N)
			x, _ := base64.RawURLEncoding.DecodeString(k.E)
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(x).Int64())}
		}
	}
	t.Fatalf("kid %s not in the provider's key set", kid)
	return nil
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || json.Unmarshal(b, v) != nil {
		t.Fatalf("GET %s: HTTP %d %s", u, resp.StatusCode, b)
	}
}

func signRS256(k *rsa.PrivateKey, hdr, claims map[string]any) string {
	in := b64(hdr) + "." + b64(claims)
	h := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, h[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func csrfToken(t *testing.T, b *Browser, gw string) string {
	t.Helper()
	code, body := b.Session(t, "GET", gw, "/admin/session", "", "")
	var s struct {
		CSRF string `json:"csrf_token"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &s) != nil || s.CSRF == "" {
		t.Fatalf("/admin/session: HTTP %d %s", code, body)
	}
	return s.CSRF
}

func login(t *testing.T, idp IdP, g *Gateway, user string) *Browser {
	t.Helper()
	b := idp.Browser(user)
	resp, body, err := b.Get(b.Login(t, g.URL))
	if err != nil || resp.StatusCode/100 != 3 || b.Cookie(strings.TrimPrefix(g.URL, "http://"), "__Host-llmflow-session") == nil {
		t.Fatalf("%s: login failed: %v HTTP %d %s", user, err, resp.StatusCode, body)
	}
	return b
}

// scim sends one SCIM request with the provider's token and returns the id
// of the created resource.
func scim(t *testing.T, g *Gateway, method, path, body string) string {
	t.Helper()
	req, _ := http.NewRequest(method, g.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+g.SCIMToken)
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		ID string `json:"id"`
	}
	if resp.StatusCode != 201 || json.Unmarshal(raw, &out) != nil || out.ID == "" {
		t.Fatalf("SCIM %s %s: HTTP %d %s", method, path, resp.StatusCode, raw)
	}
	return out.ID
}
