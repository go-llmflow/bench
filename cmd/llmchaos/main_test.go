package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := RootCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func TestListNamesScenariosFaultsAndInvariants(t *testing.T) {
	out, _, err := run(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"baseline", "upstream-cut-sse", "ilaas-saturates-while-owners-ramp", "ha-replica-kill", "[pg, CI]",
		"bmc-counter-reset", "idp-rotate", "1. no hard budget", "9. federation metrics"} {
		if !strings.Contains(out, want) {
			t.Errorf("list does not mention %q:\n%s", want, out)
		}
	}
}

func TestRunRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no scenario", []string{"run"}, "name a scenario"},
		{"unknown", []string{"run", "nope", "--gateway", "x"}, `no scenario "nope"`},
		{"no gateway", []string{"run", "baseline"}, "--gateway"},
		{"gateway missing", []string{"run", "baseline", "--gateway", "/nonexistent/gw"}, "not a file"},
	} {
		_, _, err := run(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func TestRunRefusesAVolatileWorkdir(t *testing.T) {
	self := t.TempDir() + "/gw"
	if err := writeFile(self); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/tmp/x", "/private/tmp/x", t.TempDir()} {
		_, _, err := run(t, "run", "baseline", "--gateway", self, "--workdir", dir, "--max-load", "0")
		if err == nil || !strings.Contains(err.Error(), "volatile") && !strings.Contains(err.Error(), "TMPDIR") {
			t.Errorf("workdir %s: %v", dir, err)
		}
	}
}

func TestMergeFailsOnAnIncompleteCampaign(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/r.json"
	if err := os.WriteFile(f, []byte(`[{"Scenario":"baseline","Seed":1,"Requests":5,"Verdicts":[{"N":2,"Status":"FAIL","Detail":"planted"}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "merge", f)
	if _, ok := err.(failedError); !ok {
		t.Fatalf("err = %v, want failedError", err)
	}
	for _, want := range []string{"PROBLEM baseline/1: invariant 2 FAIL: planted", "missing from the campaign", "| baseline | 1 | 5 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("merge output lacks %q:\n%s", want, out)
		}
	}
	if _, _, err := run(t, "merge", dir+"/absent.json"); err == nil {
		t.Error("an absent shard must be an error")
	}
}

func TestDiffNamesTheVerdictsThatChangedBetweenTwoReleases(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, status string) string {
		f := dir + "/" + name
		body := `[{"Scenario":"idp-jwks-down","Seed":1,"Requests":5,"Verdicts":[{"N":6,"Status":"` + status + `","Detail":"d"}]}]`
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	out, _, err := run(t, "diff", mk("a.json", "PASS"), mk("b.json", "FAIL"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "idp-jwks-down/1 invariant 6: PASS -> FAIL") || !strings.Contains(out, "1 difference(s)") {
		t.Fatalf("diff output:\n%s", out)
	}
	merged := dir + "/m.json"
	if _, _, err := run(t, "merge", "--results", merged, mk("c.json", "PASS")); err == nil {
		t.Fatal("an incomplete campaign must fail the merge")
	}
	if _, err := os.Stat(merged); err != nil {
		t.Fatalf("merge --results wrote nothing: %v", err)
	}
}

func TestExitCodes(t *testing.T) {
	if exitCode(failedError{2}) != 1 || exitCode(errTest{}) != 2 {
		t.Fatal("an invariant failure exits 1, a setup error 2")
	}
}

type errTest struct{}

func (errTest) Error() string { return "x" }
