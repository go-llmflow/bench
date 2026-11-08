package chaos

import (
	"context"
	"fmt"
	"time"
)

// Authentication expectations of the identity-provider faults. They are part
// of invariant 6 (a refused request gets the correct status): a token the
// gateway must refuse gets 401, a token it must accept is not refused as one.

const jwtSkew = 5 * time.Minute // jwt { clock_skew = "5m" } in the generated configuration

// tokenTTL is the life of a token the harness mints (see World.Token).
const tokenTTL = time.Minute

// skewAccepted says whether a token minted on a clock that is s ahead (s>0) or
// behind (s<0) must verify: its nbf/iat is s ahead of the gateway's clock, its
// exp is s+ttl from it, and the gateway tolerates jwtSkew either way. A margin
// of 20 s keeps the verdict away from the edge, where the two clocks' own jitter
// decides.
func skewAccepted(s time.Duration) (accepted, decided bool) {
	const margin = 20 * time.Second
	switch {
	case s > 0 && s < jwtSkew-margin:
		return true, true
	case s > 0 && s > jwtSkew+margin:
		return false, true
	case s <= 0 && s+tokenTTL > -jwtSkew+margin:
		return true, true
	case s <= 0 && s+tokenTTL < -jwtSkew-margin:
		return false, true
	}
	return false, false // too near the edge to judge
}

func (w *World) authProblems(j *join) (problems []string, notes []string) {
	defer func() {
		p, n := w.controlProbes()
		problems, notes = append(problems, p...), append(notes, n...)
	}()
	w.mu.Lock()
	spans := append([]faultSpan(nil), w.spans...)
	w.mu.Unlock()
	for _, sp := range spans {
		switch sp.Kind {
		case "idp-skew":
			acc, ok := skewAccepted(time.Duration(sp.Arg * float64(time.Second)))
			if !ok {
				continue
			}
			var n, bad int
			for _, r := range j.Reqs {
				if r.IdP != sp.Target || r.Left || r.Status == 0 || r.At.Before(sp.From.Add(2*time.Second)) || r.At.After(sp.To.Add(-time.Second)) {
					continue
				}
				n++
				if acc && r.Status == 401 || !acc && r.Status != 401 {
					bad++
				}
			}
			word := "accepted"
			if !acc {
				word = "refused with 401"
			}
			if bad > 0 {
				problems = append(problems, fmt.Sprintf("%s ran %+.0fs off: %d of %d requests were not %s (skew allowed: %s)", sp.Target, sp.Arg, bad, n, word, jwtSkew))
			} else if n > 0 {
				notes = append(notes, fmt.Sprintf("%s %+.0fs: %d/%d %s", sp.Target, sp.Arg, n, n, word))
			}
		case "idp-rotate":
			// after min_refresh (5 s) the gateway has refetched the key set: a
			// token signed with the new key must verify. Before it, a token of
			// an unknown kid is refused: the bounded refetch.
			var n, bad, early int
			for _, r := range j.Reqs {
				if r.IdP != sp.Target || r.Left || r.Status == 0 || r.At.Before(sp.From) {
					continue
				}
				if r.At.Before(sp.From.Add(9 * time.Second)) {
					if r.Status == 401 {
						early++
					}
					continue
				}
				n++
				if r.Status == 401 {
					bad++
				}
			}
			if bad > 0 {
				problems = append(problems, fmt.Sprintf("%s rotated its key: %d of %d tokens signed with the new key were still refused 9 s later (min_refresh 5 s)", sp.Target, bad, n))
			} else if n > 0 {
				notes = append(notes, fmt.Sprintf("%s rotated: %d tokens refused (401) while the key set was stale, then %d/%d accepted", sp.Target, early, n, n))
			}
		case "idp-down":
			// the key set is cached: a fresh token signed with a known key
			// verifies with no network, so the IdP's outage must not refuse it.
			var n, bad int
			for _, r := range j.Reqs {
				if r.IdP != sp.Target || r.Left || r.Status == 0 || r.At.Before(sp.From.Add(time.Second)) || r.At.After(sp.To) {
					continue
				}
				n++
				if r.Status == 401 || r.Status >= 500 {
					bad++
				}
			}
			if bad > 0 {
				problems = append(problems, fmt.Sprintf("%s was down for %s: %d of %d tokens signed with its cached key got 401 or 5xx", sp.Target, sp.To.Sub(sp.From).Round(time.Second), bad, n))
			} else if n > 0 {
				notes = append(notes, fmt.Sprintf("%s down: %d tokens verified from the cached keys", sp.Target, n))
			}
		}
	}
	w.mu.Lock()
	old := map[string]string{}
	for k, v := range w.oldToken {
		old[k] = v
	}
	w.mu.Unlock()
	for idp, tok := range old {
		c, ok := w.firstClientOf(idp)
		if !ok {
			continue
		}
		// the gateway has seen the new key (the load went on after the
		// rotation); the old token's key is no longer in the key set.
		o := Do(context.Background(), NewClient(), w.Primary().URL, Req{ID: 99_999_999, Org: c.Org, Team: c.Team, Who: c.Who, Prefix: c.Prefix, Cred: Cred{Bearer: tok}, MaxTokens: 16, Words: 4, Marker: "MKprobe-s0-1"})
		if o.Status != 401 {
			problems = append(problems, fmt.Sprintf("a token signed by %s's removed key was answered %d after the key set was refetched (want 401)", idp, o.Status))
		} else {
			notes = append(notes, fmt.Sprintf("%s: a token of the removed key got 401", idp))
		}
	}
	return
}

func (w *World) firstClientOf(idp string) (Client, bool) {
	for _, c := range w.Clients {
		if c.IdP == idp {
			return c, true
		}
	}
	return Client{}, false
}

// schacSpoofProbe is the positive control of gateway v0.38.0's HIGH finding
// "a token's institution is its name's scope": the federation issuer accepts
// schac_home_organization = univ-c.example, and a token whose NAME is scoped to
// another institution (eve@univ-e.example) but which states that value must be
// refused (401). v0.37.1 accepted it (filter on the claim only). Only scenarios
// that list "schac-spoof" in Expect run it; a control token (the proper scope)
// must still be accepted, or the probe proves nothing.
func (w *World) schacSpoofProbe() (problems, notes []string) {
	on := false
	for _, e := range w.Sc.Expect {
		on = on || e == "schac-spoof"
	}
	if !on || w.IdPs["fed"] == nil {
		return nil, nil
	}
	c, ok := w.firstClientOf("fed")
	if !ok {
		return nil, nil
	}
	ask := func(name string, id int) int {
		tok, err := w.IdPs["fed"].Mint("eve", time.Minute, map[string]any{"schac_home_organization": "univ-c.example", "preferred_username": name})
		if err != nil {
			return -1
		}
		o := Do(context.Background(), NewClient(), w.Primary().URL, Req{ID: id, Org: c.Org, Team: c.Team, Who: c.Who, Prefix: c.Prefix, Cred: Cred{Bearer: tok}, MaxTokens: 16, Words: 4, Marker: fmt.Sprintf("MKprobe-schac-%d", id)})
		return o.Status
	}
	// 200, or 429 when the borrower's budget is spent by now: either way the
	// token was accepted (authentication comes first), which is the control.
	if got := ask("eve@univ-c.example", 99_999_901); got != 200 && got != 429 {
		problems = append(problems, fmt.Sprintf("schac control: a token scoped to its institution was answered %d (want 200, or 429 with a spent budget): the probe proves nothing", got))
		return
	}
	w.fedProbes++
	if got := ask("eve@univ-e.example", 99_999_902); got != 401 {
		problems = append(problems, fmt.Sprintf("schac spoof: a token named eve@univ-e.example stating schac_home_organization=univ-c.example was answered %d (want 401: the name's scope must equal a value of the claim)", got))
	} else {
		notes = append(notes, "schac spoof: eve@univ-e.example stating univ-c.example got 401; the control token got 200")
	}
	return
}
