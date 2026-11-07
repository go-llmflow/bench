package main

import (
	"bytes"
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

func TestExitCodes(t *testing.T) {
	if exitCode(failedError{2}) != 1 || exitCode(errTest{}) != 2 {
		t.Fatal("an invariant failure exits 1, a setup error 2")
	}
}

type errTest struct{}

func (errTest) Error() string { return "x" }
