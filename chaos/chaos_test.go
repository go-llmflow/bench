package chaos

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorstCaseIsTheDocumentedBound(t *testing.T) {
	// docs, Hard budgets: W = max((L-M)·p_in + n·M·p_out, L·p_in), per million.
	// L=512, M=32, n=1: max(480·1000 + 32·5000, 512·1000)/1e6 = 0.64
	if got := WorstCase(32, 1); math.Abs(got-0.64) > 1e-12 {
		t.Fatalf("W(32,1) = %v, want 0.64", got)
	}
	// n=2, M=256: (256·1000 + 2·256·5000)/1e6 = 2.816
	if got := WorstCase(256, 2); math.Abs(got-2.816) > 1e-12 {
		t.Fatalf("W(256,2) = %v, want 2.816", got)
	}
	// M above L is bounded by L.
	if WorstCase(100000, 1) != WorstCase(MaxModelLen, 1) {
		t.Fatal("M is bounded by L")
	}
}

func TestIdPTokensVerifyAgainstItsKeySet(t *testing.T) {
	p := NewIdP("t", "aud")
	defer p.Close()
	tok, err := p.Mint("alice", time.Minute, map[string]any{"x": "y"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", tok)
	}
	verify := func(tok string) bool {
		parts := strings.Split(tok, ".")
		var hdr struct{ Kid string }
		hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
		_ = json.Unmarshal(hb, &hdr)
		resp, err := http.Get(p.Issuer() + "/keys")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ks struct {
			Keys []struct{ Kid, X, Y string }
		}
		_ = json.NewDecoder(resp.Body).Decode(&ks)
		for _, k := range ks.Keys {
			if k.Kid != hdr.Kid {
				continue
			}
			xb, _ := base64.RawURLEncoding.DecodeString(k.X)
			yb, _ := base64.RawURLEncoding.DecodeString(k.Y)
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			pub := &ecdsa.PublicKey{Curve: p.keys[0].key.Curve, X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
			return ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
		}
		return false
	}
	if !verify(tok) {
		t.Fatal("a fresh token does not verify against the JWKS")
	}
	p.Rotate(true)
	if verify(tok) {
		t.Fatal("after a hard rotation the old token's key must be gone from the JWKS")
	}
	p.SetDown(true)
	resp, _ := http.Get(p.Issuer() + "/keys")
	if resp.StatusCode != 503 {
		t.Fatalf("a down IdP answers %d", resp.StatusCode)
	}
	resp.Body.Close()
	p.SetDown(false)
	p.SetSkew(5 * time.Minute)
	tok, _ = p.Mint("a", time.Minute, nil)
	pb, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	var cl struct{ Iat, Exp int64 }
	_ = json.Unmarshal(pb, &cl)
	if d := time.Until(time.Unix(cl.Iat, 0)); d < 4*time.Minute+50*time.Second {
		t.Fatalf("iat is %s ahead, want ~5m", d)
	}
}

func post(t *testing.T, u *Upstream, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(u.URL()+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const chat = `{"model":"fake","max_tokens":16,"messages":[{"role":"user","content":"MKetabA-s1-7 a b c"}]}`
const chatSSE = `{"model":"fake","max_tokens":16,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"MKetabA-s1-8 a b c"}]}`

func TestUpstreamWitnessesAndFaults(t *testing.T) {
	u := NewUpstream(10)
	defer u.Close()
	resp, body := post(t, u, chat)
	if resp.StatusCode != 200 || !strings.Contains(body, `"completion_tokens":10`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	seen := u.Seen()
	if len(seen) != 1 || seen[0].Marker != "MKetabA-s1-7" || seen[0].Org != "etabA" || !seen[0].Completed || seen[0].ComplTok != 10 || seen[0].PromptTok != 4 {
		t.Fatalf("witness %+v", seen)
	}
	// wrong usage: x3 and +2 completion tokens, in JSON and in the stream.
	u.Update(func(f *UpstreamFaults) { f.UsageMul, f.UsageAdd = 3, 2 })
	_, body = post(t, u, chat)
	if !strings.Contains(body, `"completion_tokens":32`) {
		t.Fatalf("json usage not rewritten: %s", body)
	}
	_, body = post(t, u, chatSSE)
	if !strings.Contains(body, `"completion_tokens":32`) {
		t.Fatalf("stream usage not rewritten: %s", body)
	}
	u.Set(UpstreamFaults{})
	// a cut stream: no [DONE], the witness says cut, the client gets an error or a short body.
	u.Update(func(f *UpstreamFaults) { f.CutAfter = 3 })
	resp, err := http.Post(u.URL()+"/v1/chat/completions", "application/json", strings.NewReader(chatSSE))
	if err != nil {
		t.Fatal(err)
	}
	b, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "[DONE]") || rerr == nil && strings.Count(string(b), "data:") > 3 {
		t.Fatalf("stream not cut: err=%v %q", rerr, b)
	}
	time.Sleep(50 * time.Millisecond)
	last := u.Seen()[len(u.Seen())-1]
	if !last.Cut || last.Completed {
		t.Fatalf("witness of a cut stream: %+v", last)
	}
	u.Set(UpstreamFaults{Status: 502})
	if resp, _ := post(t, u, chat); resp.StatusCode != 502 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	u.Set(UpstreamFaults{ModelsDown: true})
	r2, _ := http.Get(u.URL() + "/v1/models")
	if r2.StatusCode != 503 {
		t.Fatalf("models %d", r2.StatusCode)
	}
	r2.Body.Close()
	// the metrics the gateway reads
	u.PreemptionBurst(5, 3)
	m, _ := httpGet(u.URL() + "/metrics")
	if v, _ := metricValue(m, "vllm:num_preemptions_total"); v != 5 {
		t.Fatalf("preemptions %v in %s", v, m)
	}
	if v, _ := metricValue(m, "vllm:num_requests_waiting"); v != 3 {
		t.Fatalf("waiting %v", v)
	}
}

func TestUpstreamHangEndsWhenTheClientLeaves(t *testing.T) {
	u := NewUpstream(4)
	defer u.Close()
	u.Set(UpstreamFaults{Hang: time.Minute})
	c := &http.Client{Timeout: 200 * time.Millisecond}
	if _, err := c.Post(u.URL()+"/v1/chat/completions", "application/json", strings.NewReader(chat)); err == nil {
		t.Fatal("a hung upstream answered")
	}
	time.Sleep(100 * time.Millisecond)
	if n := u.inFl.Load(); n != 0 {
		t.Fatalf("%d requests still in flight after the client left", n)
	}
	if !u.Seen()[0].ClientGone {
		t.Fatalf("witness %+v", u.Seen()[0])
	}
}

func TestBMCServesAControllableCounterOverTLS(t *testing.T) {
	dir := t.TempDir()
	b, ca, err := NewBMC(dir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if ca == "" {
		t.Fatal("no CA file")
	}
	get := func(user string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", b.URL()+"/redfish/v1/Chassis/1/EnvironmentMetrics", nil)
		req.SetBasicAuth(user, "pw")
		resp, err := b.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	b.Add(1.5)
	if code, m := get("admin"); code != 200 || m["EnergykWh"].(map[string]any)["Reading"] != 1.5 {
		t.Fatalf("%d %v", code, m)
	}
	b.Reset(0)
	if _, m := get("admin"); m["EnergykWh"].(map[string]any)["Reading"] != 0.0 {
		t.Fatalf("reset: %v", m)
	}
	if b.Total() != 1.5 {
		t.Fatalf("total %v", b.Total())
	}
	if code, _ := get("root"); code != 401 {
		t.Fatalf("wrong user: %d", code)
	}
	b.SetPowerOnly(true, 900)
	if _, m := get("admin"); m["EnergykWh"] != nil || m["PowerWatts"] == nil {
		t.Fatalf("power only: %v", m)
	}
	b.SetOffline(true)
	if code, _ := get("admin"); code != 503 {
		t.Fatalf("offline: %d", code)
	}
}

// ---- the judges, with planted breakages: a check that cannot fail checks nothing ----

// adminFake serves canned reports as the gateway's admin API.
func adminFake(t *testing.T, spend []SpendRow, budgets []BudgetRow) *World {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/spend":
			_ = json.NewEncoder(w).Encode(map[string]any{"rows": spend})
		case "/admin/budgets":
			_ = json.NewEncoder(w).Encode(budgets)
		case "/metrics":
			fmt.Fprint(w, "llmflow_budget_hold_exceeded_total 0\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	w := &World{Sc: &Scenario{Name: "t"}, Up: NewUpstream(1), GW: []*Gateway{{URL: srv.URL, Master: "m"}}, IdPs: map[string]*IdP{}}
	t.Cleanup(w.Up.Close)
	w.Clients = []Client{{Org: "etabA", Team: "a1", Who: "key:kA", KeyID: "kA"}, {Org: "etabB", Team: "b1", Who: "key:kB", KeyID: "kB"}}
	return w
}

func cleanReq(w *World, who, org string, id int, prompt, compl int) {
	m := fmt.Sprintf("MK%s-s1-%d", org, id)
	w.Up.mu.Lock()
	w.Up.seen[m+"#1"] = &Seen{Marker: m, Org: org, Completed: true, Reported: true, PromptTok: prompt, ComplTok: compl}
	w.Up.order = append(w.Up.order, m+"#1")
	w.Up.mu.Unlock()
	w.Ledger.Add(Outcome{Req: Req{ID: id, Org: org, Who: who, Marker: m, MaxTokens: 32}, Status: 200, BodyLen: 10})
}

func TestInvariant2FlagsAChargeToTheWrongOrg(t *testing.T) {
	good := []SpendRow{{Org: "etabA", Team: "a1", Key: "kA", Requests: 1, Cost: Cost(6, 10)}}
	w := adminFake(t, good, nil)
	cleanReq(w, "key:kA", "etabA", 1, 6, 10)
	if v := inv2(w, newJoin(w)); v.Status != Pass {
		t.Fatalf("a correct bill fails: %+v", v)
	}
	// the same usage billed to etabB's key: the planted defect.
	bad := adminFake(t, []SpendRow{{Org: "etabB", Team: "b1", Key: "kB", Requests: 1, Cost: Cost(6, 10)}}, nil)
	cleanReq(bad, "key:kA", "etabA", 1, 6, 10)
	if v := inv2(bad, newJoin(bad)); v.Status != Fail {
		t.Fatalf("a request billed to another org passed: %+v", v)
	}
	// billed to the right key but under another org's name.
	mis := adminFake(t, []SpendRow{{Org: "etabB", Team: "a1", Key: "kA", Requests: 1, Cost: Cost(6, 10)}}, nil)
	cleanReq(mis, "key:kA", "etabA", 1, 6, 10)
	if v := inv2(mis, newJoin(mis)); v.Status != Fail {
		t.Fatalf("a key billed under another org passed: %+v", v)
	}
	// doubled spend.
	dbl := adminFake(t, []SpendRow{{Org: "etabA", Team: "a1", Key: "kA", Requests: 2, Cost: 2 * Cost(6, 10)}}, nil)
	cleanReq(dbl, "key:kA", "etabA", 1, 6, 10)
	if v := inv2(dbl, newJoin(dbl)); v.Status != Fail {
		t.Fatalf("doubled spend passed: %+v", v)
	}
	// lost spend.
	lost := adminFake(t, nil, nil)
	cleanReq(lost, "key:kA", "etabA", 1, 6, 10)
	if v := inv2(lost, newJoin(lost)); v.Status != Fail {
		t.Fatalf("lost spend passed: %+v", v)
	}
}

func TestInvariant2AllowsTheCountOfAnAbandonedRequestButNeverItsMoney(t *testing.T) {
	left := func(w *World) {
		w.Ledger.Add(Outcome{Req: Req{ID: 2, Org: "etabA", Who: "key:kA", Marker: "MKetabA-s1-2", MaxTokens: 32}, Err: "context canceled", Left: true})
	}
	cost := Cost(6, 10)
	// one clean request, one abandoned before the upstream saw it, counted: allowed.
	ok := adminFake(t, []SpendRow{{Org: "etabA", Team: "a1", Key: "kA", Requests: 2, Cost: cost}}, nil)
	cleanReq(ok, "key:kA", "etabA", 1, 6, 10)
	left(ok)
	if v := inv2(ok, newJoin(ok)); v.Status != Pass {
		t.Fatalf("an abandoned request counted at no cost fails: %+v", v)
	}
	// planted: counted twice more than anyone could have asked.
	more := adminFake(t, []SpendRow{{Org: "etabA", Team: "a1", Key: "kA", Requests: 3, Cost: cost}}, nil)
	cleanReq(more, "key:kA", "etabA", 1, 6, 10)
	left(more)
	if v := inv2(more, newJoin(more)); v.Status != Fail {
		t.Fatalf("a count above clean + abandoned passed: %+v", v)
	}
	// planted: the abandoned request billed money.
	paid := adminFake(t, []SpendRow{{Org: "etabA", Team: "a1", Key: "kA", Requests: 2, Cost: 2 * cost}}, nil)
	cleanReq(paid, "key:kA", "etabA", 1, 6, 10)
	left(paid)
	if v := inv2(paid, newJoin(paid)); v.Status != Fail {
		t.Fatalf("money billed for an abandoned request passed: %+v", v)
	}
}

func TestInvariant1FlagsAnExceededHardBudget(t *testing.T) {
	w := adminFake(t, nil, []BudgetRow{{Kind: "key", Name: "kA", MaxCost: 0.2, Limit: 0.2, Committed: 0.15}})
	w.Budgets = []BudgetSpec{{Kind: "key", Name: "kA", Period: "day", Hard: 0.2}}
	cleanReq(w, "key:kA", "etabA", 1, 6, 10) // cost 0.056
	if v := inv1(w, newJoin(w)); v.Status != Pass {
		t.Fatalf("%+v", v)
	}
	// clients paid for 4 answers: 4 x 0.056 = 0.224 > 0.2 and nothing was above its W.
	for i := 2; i <= 4; i++ {
		cleanReq(w, "key:kA", "etabA", i, 6, 10)
	}
	if v := inv1(w, newJoin(w)); v.Status != Fail {
		t.Fatalf("a hard budget exceeded by 12 %% passed: %+v", v)
	}
	// the gateway's own ledger above its limit also fails.
	w2 := adminFake(t, nil, []BudgetRow{{Kind: "key", Name: "kA", MaxCost: 0.2, Limit: 0.2, Committed: 0.25}})
	w2.Budgets = []BudgetSpec{{Kind: "key", Name: "kA", Period: "day", Hard: 0.2}}
	if v := inv1(w2, newJoin(w2)); v.Status != Fail {
		t.Fatalf("committed above limit passed: %+v", v)
	}
}

func TestInvariant6FlagsMissingRetryAfterAndEmpty200(t *testing.T) {
	mk := func(os ...Outcome) *World {
		w := adminFake(t, nil, nil)
		for _, o := range os {
			w.Ledger.Add(o)
		}
		return w
	}
	ok := mk(Outcome{Status: 429, RetryAfter: "5"}, Outcome{Status: 200, BodyLen: 20}, Outcome{Status: 401})
	if v := inv6(ok, newJoin(ok)); v.Status != Pass {
		t.Fatalf("%+v", v)
	}
	// a stream cut after its 200 is documented behaviour: an observation, not a failure.
	cut := mk(Outcome{Req: Req{Stream: true}, Status: 200, Events: 3, BodyLen: 50})
	if v := inv6(cut, newJoin(cut)); v.Status != Pass || !strings.Contains(v.Detail, "observed: 1 cut streams") {
		t.Fatalf("a cut stream: %+v", v)
	}
	for name, o := range map[string]Outcome{
		"429 without Retry-After": {Status: 429},
		"429 with junk":           {Status: 429, RetryAfter: "soon"},
		"429 with zero":           {Status: 429, RetryAfter: "0"},
		"empty 200":               {Status: 200},
		"empty stream":            {Status: 200, Stream: false, BodyLen: 0},
	} {
		w := mk(o)
		if v := inv6(w, newJoin(w)); v.Status != Fail {
			t.Errorf("%s passed: %+v", name, v)
		}
	}
}

func TestInvariant9FlagsLeaksOfOtherOrgs(t *testing.T) {
	metrics := func(extra string) string {
		return "# TYPE vllm:time_to_first_token_seconds histogram\n" +
			`vllm:time_to_first_token_seconds_bucket{model_name="chat",le="2560.0"} 1.0` + "\n" +
			`vllm:time_to_first_token_seconds_bucket{model_name="chat",le="+Inf"} 2.0` + "\n" +
			`vllm:time_to_first_token_seconds_sum{model_name="chat"} 0.1` + "\n" +
			`vllm:time_to_first_token_seconds_count{model_name="chat"} 2.0` + "\n" + extra
	}
	run := func(text string) Verdict {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, text) }))
		defer srv.Close()
		w := &World{Sc: &Scenario{Name: "t", F: Features{Federation: true}}, Up: NewUpstream(1), GW: []*Gateway{{URL: srv.URL}}}
		defer w.Up.Close()
		w.Ledger.Add(Outcome{Req: Req{Org: "tiers", Prefix: FedPrefix}, Status: 200, BodyLen: 1})
		w.Ledger.Add(Outcome{Req: Req{Org: "tiers", Prefix: FedPrefix}, Status: 429, RetryAfter: "5"})
		return inv9(w, newJoin(w))
	}
	_ = bufio.NewReader
	if v := run(strings.ReplaceAll(metrics(""), "/fed/ilaas", "")); v.Status != Pass {
		t.Fatalf("%+v", v)
	}
	if v := run(metrics(`vllm:x{org="etabA"} 1` + "\n")); v.Status != Fail {
		t.Fatalf("a series naming an owner passed: %+v", v)
	}
	if v := run(strings.Replace(metrics(""), `_count{model_name="chat"} 2.0`, `_count{model_name="chat"} 9.0`, 1)); v.Status != Fail {
		t.Fatalf("a count that no request explains passed: %+v", v)
	}
	if v := run(strings.Replace(metrics(""), `_sum{model_name="chat"} 0.1`, `_sum{model_name="chat"} +Inf`, 1)); v.Status != Fail {
		t.Fatalf("an infinite sum passed: %+v", v)
	}
}

func TestEveryScenarioIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, sc := range Catalogue() {
		if sc.Name == "" || sc.Doc == "" || sc.Duration <= 0 || len(sc.Load) == 0 {
			t.Errorf("scenario %+v is incomplete", sc.Name)
		}
		if seen[sc.Name] {
			t.Errorf("duplicate scenario %s", sc.Name)
		}
		seen[sc.Name] = true
		for _, f := range sc.Faults {
			if _, ok := faultTable[f.Kind]; !ok {
				t.Errorf("%s: unknown fault %q", sc.Name, f.Kind)
			}
			if f.Target != "" && !strings.HasPrefix(f.Target, "idp") && f.Target != "fed" && !strings.HasPrefix(f.Target, "r") {
				t.Errorf("%s: odd target %q", sc.Name, f.Target)
			}
		}
		for n := range sc.Skip {
			if n < 1 || n >= len(InvariantNames) {
				t.Errorf("%s: skips invariant %d", sc.Name, n)
			}
		}
	}
}

func TestRngIsReplayable(t *testing.T) {
	a, b := rngFor(7, "s", "etabA", "0"), rngFor(7, "s", "etabA", "0")
	for i := 0; i < 50; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatal("the same seed and name must replay the same plan")
		}
	}
	if rngFor(7, "s", "etabA", "0").Uint64() == rngFor(8, "s", "etabA", "0").Uint64() {
		t.Fatal("another seed must give another plan")
	}
}

func TestReportFractionProratesByDays(t *testing.T) {
	f := reportFraction()
	if f < 3.0/31 || f > 3.0/28+1e-9 {
		t.Fatalf("three days are between 3/31 and 3/28 of a month, got %v", f)
	}
}
