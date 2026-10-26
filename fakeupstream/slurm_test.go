package fakeupstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getJobs(t *testing.T, url, token string) (*http.Response, map[string]json.RawMessage) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/slurm/v0.0.44/jobs/", nil)
	if token != "" {
		req.Header.Set("X-SLURM-USER-TOKEN", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return resp, m
}

func TestSlurmrestd(t *testing.T) {
	s := &Slurmrestd{Token: "jwt"}
	srv := httptest.NewServer(s)
	defer srv.Close()
	if resp, m := getJobs(t, srv.URL, ""); resp.StatusCode != 401 || m["errors"] == nil {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, m := getJobs(t, srv.URL, "jwt")
	if resp.StatusCode != 200 || string(m["jobs"]) != "[]" {
		t.Fatalf("empty: %d %s", resp.StatusCode, m["jobs"])
	}
	s.SetJobs(SlurmJob{JobID: 7, Name: "llmflow:llama:8000:4", Account: "svc_llm", Partition: "gpu", BatchHost: "gpu01", JobState: []string{"RUNNING"}})
	_, m = getJobs(t, srv.URL, "jwt")
	var jobs []map[string]any
	if err := json.Unmarshal(m["jobs"], &jobs); err != nil || len(jobs) != 1 || jobs[0]["job_id"] != float64(7) || jobs[0]["batch_host"] != "gpu01" {
		t.Fatalf("jobs %s", m["jobs"])
	}
	if st, _ := jobs[0]["job_state"].([]any); len(st) != 1 || st[0] != "RUNNING" {
		t.Fatalf("job_state %v", jobs[0]["job_state"])
	}
	s.SetDown(true)
	if resp, _ := getJobs(t, srv.URL, "jwt"); resp.StatusCode != 502 {
		t.Fatalf("down: %d", resp.StatusCode)
	}
	if served, denied := s.Requests(); served != 3 || denied != 1 {
		t.Fatalf("requests %d denied %d", served, denied)
	}
}
