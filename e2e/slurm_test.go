package e2e

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

// Slurm discovery against a REAL slurmctld + slurmd + slurmrestd (gateway
// #75). Runs on a Slurm node as root (it submits jobs as other users with
// sbatch --uid); see the README's "Slurm E2E" for the container.
const (
	envSlurmRestd = "BENCH_SLURM_RESTD"    // e.g. http://127.0.0.1:6820
	envSlurmAPI   = "BENCH_SLURM_API"      // data_parser version, e.g. v0.0.42
	envSlurmJWT   = "BENCH_SLURM_JWT_FILE" // output of `scontrol token username=svc_llm`
	envSlurmModel = "BENCH_SLURM_MODEL"    // model id the job serves
	// envSlurmJob is the job's command; {PORT} and {KEYFILE} are replaced.
	envSlurmJob = "BENCH_SLURM_JOB"
	// envSlurmWait bounds the wait for an instance (vLLM loads slowly);
	// envSlurmMem is the memory of the service job (sbatch --mem).
	envSlurmWait = "BENCH_SLURM_WAIT"
	envSlurmMem  = "BENCH_SLURM_MEM"
)

func slurmEnv(t *testing.T) (restd, api, jwtFile, model, job string, wait time.Duration) {
	t.Helper()
	restd, jwtFile, job = os.Getenv(envSlurmRestd), os.Getenv(envSlurmJWT), os.Getenv(envSlurmJob)
	if restd == "" || jwtFile == "" || job == "" {
		t.Skipf("%s, %s and %s not set: no Slurm to test against", envSlurmRestd, envSlurmJWT, envSlurmJob)
	}
	api, model = os.Getenv(envSlurmAPI), os.Getenv(envSlurmModel)
	if api == "" {
		api = "v0.0.42"
	}
	if model == "" {
		model = "fake"
	}
	wait = 2 * time.Minute
	if d, err := time.ParseDuration(os.Getenv(envSlurmWait)); err == nil {
		wait = d
	}
	return
}

// sbatch submits a job as user in account and returns its id.
func sbatch(t *testing.T, user, account, partition, name, mem, cmd string) string {
	t.Helper()
	out, err := exec.Command("sbatch", "--parsable", "--uid="+user, "--account="+account, "--partition="+partition,
		"--mem="+mem, "--job-name="+name, "--output=/tmp/"+strings.ReplaceAll(name, ":", "_")+".log", "--wrap="+cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("sbatch %s: %v %s", name, err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("scancel", id).Run() })
	return id
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}

func TestSlurmDiscoveryE2E(t *testing.T) {
	restd, api, jwtFile, model, job, wait := slurmEnv(t)
	raw, err := os.ReadFile(jwtFile)
	if err != nil {
		t.Fatal(err)
	}
	// `scontrol token` prints SLURM_JWT=<token>.
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "SLURM_JWT="))
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "slurm.jwt")
	if err := os.WriteFile(tokFile, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The instances' --api-key: the job runs as svc_llm and must read it.
	shared, err := os.MkdirTemp("/tmp", "llmflow-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shared) })
	_ = os.Chmod(shared, 0o755)
	keyFile := filepath.Join(shared, "vllm.key")
	if err := os.WriteFile(keyFile, []byte("e2e-"+randomHex(16)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	static := httptest.NewServer(fakeupstream.NewServer(fakeupstream.Options{Model: "static", Reply: "ok"}, nil))
	defer static.Close()
	tail := fmt.Sprintf(`pool "slurm" { capacity = 0 }
model "llama" {
  upstream_model = %q
  pool           = "slurm"
  key_file       = %q
  slurm {
    url               = %q
    api_version       = %q
    token_file        = %q
    user_name         = "svc_llm"
    accounts          = ["svc_llm"]
    partitions        = ["gpu"]
    users             = ["svc_llm"]
    interval          = "2s"
    stale_after       = "20s"
    instance_capacity = 2
  }
}
`, model, keyFile, restd, api, tokFile)
	g := StartGatewayWith(t, upstream{URL: static.URL, Model: "static"}, nil, nil, "", tail)
	capacity := func() float64 {
		return sumMatching(Metrics(t, g.URL+"/metrics"), "llmflow_pool_capacity", `pool="slurm"`)
	}
	chat := func() int {
		code, _ := postJSON(g.URL+"/v1/chat/completions", g.Keys["a1"], chatBody("llama", "Say hi.", 4, ""), nil, nil)
		return code
	}
	if c := capacity(); c != 0 {
		t.Fatalf("no job yet: C = %v", c)
	}
	cmdFor := func(port int) string {
		return strings.NewReplacer("{PORT}", fmt.Sprint(port), "{KEYFILE}", keyFile).Replace(job)
	}

	// A user's job claiming the model, in another account: never trusted.
	// (A placeholder process: it is never asked anything.)
	sbatch(t, "alice", "physics", "gpu", "llmflow:llama:18101", "100M", "sleep 600")
	// The service account's job, capacity 3.
	mem := os.Getenv(envSlurmMem)
	if mem == "" {
		mem = "200M"
	}
	id := sbatch(t, "svc_llm", "svc_llm", "gpu", "llmflow:llama:18100:3", mem, cmdFor(18100))
	t0 := time.Now()
	waitFor(t, "the instance to be admitted", wait, func() bool { return capacity() == 3 })
	t.Logf("job %s admitted after %s", id, time.Since(t0).Round(time.Second))
	if code := chat(); code != 200 {
		t.Fatalf("chat through the Slurm instance: HTTP %d", code)
	}
	m := Metrics(t, g.URL+"/metrics")
	if sumMatching(m, "llmflow_slurm_rejected_jobs_total", `reason="account"`) != 1 {
		t.Errorf("the foreign job was not rejected once: %v", sumMatching(m, "llmflow_slurm_rejected_jobs_total"))
	}

	// The job ends (the night): drained and removed, C back to 0.
	if out, err := exec.Command("scancel", id).CombinedOutput(); err != nil {
		t.Fatalf("scancel: %v %s", err, out)
	}
	t1 := time.Now()
	waitFor(t, "the instance to be removed", time.Minute, func() bool { return capacity() == 0 })
	t.Logf("removed %s after scancel", time.Since(t1).Round(time.Second))
	if code := chat(); code == 200 {
		t.Error("chat still served after the job ended")
	}
}
