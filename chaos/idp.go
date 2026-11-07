package chaos

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// IdP is an OpenID provider on loopback: discovery, a JWKS that can be taken
// down or rotated, and tokens signed with ES256 on a clock that can be skewed.
type IdP struct {
	Name     string
	Audience string
	srv      *httptest.Server
	mu       sync.Mutex
	keys     []*idpKey // the first one signs
	down     bool      // discovery and JWKS answer 503
	skew     time.Duration
	jwksHits int
	seq      int
}

type idpKey struct {
	kid string
	key *ecdsa.PrivateKey
}

// NewIdP starts a provider.
func NewIdP(name, audience string) *IdP {
	p := &IdP{Name: name, Audience: audience}
	p.rotate()
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	return p
}

func (p *IdP) rotate() {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	p.seq++
	p.keys = append([]*idpKey{{kid: fmt.Sprintf("%s-k%d", p.Name, p.seq), key: k}}, p.keys...)
}

// Rotate adds a new signing key. dropOld removes the older keys from the JWKS
// at once (a hard rotation: tokens signed before it stop verifying once the
// gateway refetches).
func (p *IdP) Rotate(dropOld bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rotate()
	if dropOld {
		p.keys = p.keys[:1]
	}
}

// SetDown makes discovery and the key set answer 503.
func (p *IdP) SetDown(down bool) { p.mu.Lock(); p.down = down; p.mu.Unlock() }

// SetSkew shifts the provider's clock: iat, nbf and exp are computed from
// now+skew.
func (p *IdP) SetSkew(d time.Duration) { p.mu.Lock(); p.skew = d; p.mu.Unlock() }

// JWKSHits is the number of key-set requests received.
func (p *IdP) JWKSHits() int { p.mu.Lock(); defer p.mu.Unlock(); return p.jwksHits }

// Issuer is the provider's issuer URL.
func (p *IdP) Issuer() string { return p.srv.URL }

// Close stops it.
func (p *IdP) Close() { p.srv.CloseClientConnections(); p.srv.Close() }

func (p *IdP) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path == "/keys" {
		p.jwksHits++
	}
	if p.down {
		http.Error(w, "idp down", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"ES256"}})
	case "/keys":
		var ks []map[string]string
		for _, k := range p.keys {
			ks = append(ks, map[string]string{"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": k.kid,
				"x": b64(pad32(k.key.PublicKey.X)), "y": b64(pad32(k.key.PublicKey.Y))})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": ks})
	default:
		http.NotFound(w, r)
	}
}

func pad32(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Mint signs an access token for subject sub, valid ttl on the provider's
// (possibly skewed) clock. extra claims are merged in.
func (p *IdP) Mint(sub string, ttl time.Duration, extra map[string]any) (string, error) {
	p.mu.Lock()
	k, skew := p.keys[0], p.skew
	p.mu.Unlock()
	now := time.Now().Add(skew)
	claims := map[string]any{"iss": p.srv.URL, "aud": p.Audience, "sub": sub, "iat": now.Unix(), "nbf": now.Unix(),
		"exp": now.Add(ttl).Unix(), "groups": []string{"users"}, "preferred_username": sub}
	for kk, v := range extra {
		claims[kk] = v
	}
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": k.kid})
	pl, _ := json.Marshal(claims)
	signing := b64(hdr) + "." + b64(pl)
	h := sha256.Sum256([]byte(signing))
	rr, ss, err := ecdsa.Sign(rand.Reader, k.key, h[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(append(pad32(rr), pad32(ss)...)), nil
}
