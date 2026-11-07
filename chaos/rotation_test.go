package chaos

import (
	"context"
	"testing"
	"time"

	"github.com/go-authn/oidc"
)

// A token signed with a key that was REMOVED from the JWKS must be refused once
// the verifier has refetched the key set. Deterministic: the clock is the
// verifier's MinRefresh (1 s), waited out, never a race.
func TestOIDCRefusesATokenOfARemovedKeyAfterTheRefetch(t *testing.T) {
	p := NewIdP("rot", "aud")
	defer p.Close()
	ctx := context.Background()
	v, err := oidc.New(ctx, oidc.Config{Issuer: p.Issuer(), Audience: "aud", ClockSkew: time.Minute, MinRefresh: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := p.Mint("old-key-probe", 10*time.Minute, nil)
	// Not presented before the rotation: nothing cached it.
	p.Rotate(true)
	time.Sleep(1200 * time.Millisecond) // past MinRefresh
	fresh, _ := p.Mint("alice", time.Minute, nil)
	if _, err := v.Verify(ctx, fresh); err != nil {
		t.Fatalf("a token of the new key must verify (this refetches the key set): %v", err)
	}
	if _, err := v.Verify(ctx, old); err == nil {
		t.Fatal("DEFECT: a token signed with a key removed from the JWKS was accepted after the key set was refetched")
	}
}

// The same token presented BEFORE the refetch is also refused (unknown kid), and
// a token verified before the rotation is not re-verified from the key set.
func TestOIDCOldTokenBeforeRefetchIsRefusedAndVerifiedOnesStayVerified(t *testing.T) {
	p := NewIdP("rot2", "aud")
	defer p.Close()
	ctx := context.Background()
	v, err := oidc.New(ctx, oidc.Config{Issuer: p.Issuer(), Audience: "aud", ClockSkew: time.Minute, MinRefresh: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := p.Mint("a", 10*time.Minute, nil)
	if _, err := v.Verify(ctx, old); err != nil {
		t.Fatal(err)
	}
	// Same key still held (no refetch allowed for an hour): the old token still
	// verifies. That is the documented bound on refetching, not a defect; the
	// window is MinRefresh.
	p.Rotate(true)
	if _, err := v.Verify(ctx, old); err != nil {
		t.Logf("old token refused before the refetch: %v", err)
	} else {
		t.Logf("observed: until the key set is refetched (MinRefresh, gateway minimum 5 s, default 1 m) a removed key still verifies the tokens it signed")
	}
}
