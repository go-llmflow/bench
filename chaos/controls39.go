package chaos

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Positive controls of gateway v0.39.0 (fourth audit): the institution of a
// person, domains with one owner, a counter glitch never billed as a reset, the
// accounting spool degrading fairly. (n and best_of: nAdmissionProbe.)

func init() {
	// one scrape of the exporter reads 0 for a GPU
	faultTable["dcgm-glitch"] = func(w *World, f FaultAt) (func(), error) {
		if w.DCGM == nil {
			return nil, fmt.Errorf("no DCGM exporter in this scenario")
		}
		w.DCGM.GlitchOnce("GPU-aaa")
		return nil, nil
	}
}

// setupControl39 adds the configuration of the v0.39.0 controls' features.
func (w *World) setupControl39(topo *Topology) {
	if w.Sc.F.SharedSCIM {
		// one SCIM client, two organisations' teams: it owns no domain; etabA
		// claims s.example itself (org { domains }, from v0.39.0)
		topo.Extra += fmt.Sprintf("scim {\n  token_file = \"@DIR@/scim.token\"\n  base_url = \"https://llm.example.org/scim/v2\"\n  domain = \"s.example\"\n  group \"Lab Physics\" { team = %q }\n  group \"Lab B\" { team = %q }\n}\n", TeamOf["etabA"], TeamOf["etabB"])
		topo.OrgDomains = map[string][]string{"etabA": {"s.example"}}
	}
	if w.Sc.F.SpoolFair {
		topo.StoreExtra = "  spool_max_bytes = 1048576\n"
	}
}

// ---- control schac-institution ----

// schacInstitutionProbe: three issuers of the borrower's organisation, and the
// three ways v0.38.x let a person of one institution get the team of another:
//
//	instA: a token naming dave@univ-d.example that states [univ-d, univ-c]: the
//	       map on univ-c matched any stated value;
//	instB: accept names schacHomeOrganization (univ-d), the map reads
//	       schac_home_organization (univ-c): two spellings, two answers;
//	instC: domains and no accept: no tie at all, the map on univ-c selects the team.
//
// All three must be refused. The control, a person of univ-c stating univ-c,
// must be accepted on each (200, or 429 when the borrower's budget is spent).
func (w *World) schacInstitutionProbe() (problems, notes []string) {
	if !w.Sc.expects("schac-institution") {
		return nil, nil
	}
	c, ok := w.firstClientOf("fed")
	if !ok {
		return []string{"schac-institution: no federation client"}, nil
	}
	id := 99_990_000
	ask := func(idp, sub, name string, claims map[string]any) int {
		extra := map[string]any{"preferred_username": name}
		for k, v := range claims {
			extra[k] = v
		}
		tok, err := w.IdPs[idp].Mint(sub, time.Minute, extra)
		if err != nil {
			return -1
		}
		id++
		o := Do(context.Background(), NewClient(), w.Primary().URL, Req{ID: id, Org: c.Org, Team: c.Team, Who: c.Who, Prefix: c.Prefix, Cred: Cred{Bearer: tok}, MaxTokens: 16, Words: 4, Marker: fmt.Sprintf("MKprobe-inst-%d", id)})
		return o.Status
	}
	const cu, du = "univ-c.example", "univ-d.example"
	type attack struct {
		idp, what      string
		control, claim map[string]any
	}
	for _, a := range []attack{
		{"instA", "a token stating [univ-d, univ-c] named in univ-d",
			map[string]any{"schac_home_organization": cu}, map[string]any{"schac_home_organization": []string{du, cu}}},
		{"instB", "the tie on schacHomeOrganization=univ-d and the map on schac_home_organization=univ-c",
			map[string]any{"schacHomeOrganization": cu, "schac_home_organization": cu}, map[string]any{"schacHomeOrganization": du, "schac_home_organization": cu}},
		{"instC", "no accept: univ-c stated by a person named in univ-d",
			map[string]any{"schac_home_organization": cu}, map[string]any{"schac_home_organization": cu}},
	} {
		if got := ask(a.idp, "carol", "carol@"+cu, a.control); got != 200 && got != 429 {
			problems = append(problems, fmt.Sprintf("schac-institution control (%s): a person of univ-c stating univ-c was answered %d (want 200 or 429): the probe proves nothing", a.idp, got))
			continue
		}
		if got := ask(a.idp, "dave", "dave@"+du, a.claim); got == 200 || got == 429 {
			problems = append(problems, fmt.Sprintf("schac-institution (%s): %s was accepted (%d), and mapped to univ-c's team (want a refusal: a person has one institution, the scope of their name)", a.idp, a.what, got))
		}
	}
	if len(problems) == 0 {
		notes = append(notes, "schac-institution: the three attacks refused, the controls accepted")
	}
	return
}

// ---- control shared-scim ----

// sharedScimProbe: the SCIM client's groups map to the teams of etabA and etabB,
// so it is shared; etabA claims s.example by org { domains } (v0.39.0). The org
// admin of etabA creates a person of s.example (the control, 201, on both
// releases); the org admin of etabB must be refused 400: a shared source owns
// nothing. v0.38.1 let the admin of every organisation that shares the client
// create its people.
func (w *World) sharedScimProbe() (problems, notes []string) {
	if !w.Sc.expects("shared-scim") {
		return nil, nil
	}
	mk := func(org, admin string) (func(string) int, error) {
		g := w.Primary()
		if code, b, err := g.Admin("POST", "/admin/users", `{"name":"`+admin+`"}`); err != nil || (code != 201 && code != 409) {
			return nil, fmt.Errorf("create %s: %d %v %s", admin, code, err, b)
		}
		if code, b, err := g.Admin("PUT", "/admin/users/"+admin+"/roles", `{"role":"org_admin","org":"`+org+`"}`); err != nil || code != 200 {
			return nil, fmt.Errorf("grant org_admin on %s: %d %v %s", org, code, err, b)
		}
		code, b, err := g.Admin("POST", "/admin/users/"+admin+"/tokens", "")
		if err != nil || code != 201 {
			return nil, fmt.Errorf("token for %s: %d %v %s", admin, code, err, b)
		}
		i := strings.Index(string(b), "llma-")
		if i < 0 {
			return nil, fmt.Errorf("no llma- token for %s", admin)
		}
		tok := string(b)[i:]
		if j := strings.IndexAny(tok, `"`); j > 0 {
			tok = tok[:j]
		}
		return func(name string) int {
			c, _, _ := g.AdminAs(tok, "POST", "/admin/users", `{"name":"`+name+`"}`)
			return c
		}, nil
	}
	createA, err := mk("etabA", "adminsa@a.example")
	if err != nil {
		return []string{"shared-scim: " + err.Error()}, nil
	}
	createB, err := mk("etabB", "adminsb@b.example")
	if err != nil {
		return []string{"shared-scim: " + err.Error()}, nil
	}
	if c := createA("dora@s.example"); c != 201 {
		return []string{fmt.Sprintf("shared-scim control: the org admin of etabA creating dora@s.example got %d (want 201): the probe proves nothing", c)}, nil
	}
	if c := createB("carol@s.example"); c != 400 {
		problems = append(problems, fmt.Sprintf("shared-scim: the org admin of etabB creating carol@s.example, a domain of a SCIM client it only shares, got %d (want 400: a shared source owns nothing)", c))
	} else {
		notes = append(notes, "shared-scim: etabA's admin created dora@s.example, etabB's admin was refused 400")
	}
	return
}

// ---- control spool-fair ----

// spoolFairProbe: PostgreSQL hangs for most of the run, the spool is 1 MiB, and
// etabA floods; etabB sends a little. Every request answered must be billed
// exactly once (invariant 2, no kill): v0.38.1 loses the records that do not fit;
// v0.39.0 refuses the flooder (503 spool_org_share, budgeted requests) and keeps
// serving the others. The probe wants etabB served meanwhile, and the flood
// refused 503 with that reason.
func (w *World) spoolFairProbe() (problems, notes []string) {
	if !w.Sc.expects("spool-fair") {
		return nil, nil
	}
	w.mu.Lock()
	var from, to time.Time
	for _, sp := range w.spans {
		if sp.Kind == "pg-hang" {
			from, to = sp.From, sp.To
		}
	}
	w.mu.Unlock()
	if from.IsZero() {
		return []string{"spool-fair: PostgreSQL never hung"}, nil
	}
	return spoolFairProblems(w.Ledger.All(), from, to)
}

// spoolFairProblems judges the window from 3 s into the hang (the spool has
// filled by then) to 1 s before its end: etabB, which floods nothing, must be
// served at least half of the time, and the flooder must be refused 503 at
// least once. v0.38.1 refuses every budgeted request once the spool is full
// ("requests under a budget are refused until it is emptied"), the innocent
// included; v0.39.0 refuses only the organisation that holds most of it.
func spoolFairProblems(outs []Outcome, from, to time.Time) (problems, notes []string) {
	lo, hi := from.Add(3*time.Second), to.Add(-time.Second)
	var bAttempts, bServed, aRefused int
	for _, o := range outs {
		if o.At.Before(lo) || o.At.After(hi) || o.Left || o.Status == 0 {
			continue
		}
		switch {
		case o.Org == "etabB":
			bAttempts++
			if o.Status == 200 {
				bServed++
			}
		case o.Org == "etabA" && o.Status == 503:
			aRefused++
		}
	}
	switch {
	case bAttempts < 15:
		problems = append(problems, fmt.Sprintf("spool-fair: etabB sent only %d requests in the window: the probe proves nothing", bAttempts))
	case bServed*2 < bAttempts:
		problems = append(problems, fmt.Sprintf("spool-fair: etabB, which floods nothing, was served %d of %d times while the spool was full: one organisation filled it for everyone", bServed, bAttempts))
	case aRefused == 0:
		problems = append(problems, "spool-fair: the flooding organisation was never refused while the spool was full: the probe proves nothing")
	default:
		notes = append(notes, fmt.Sprintf("spool-fair: etabB served %d of %d times in the window, the flooder refused %d times", bServed, bAttempts, aRefused))
	}
	return
}
