package fakeupstream

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// SlurmJob is the part of a Slurm JOB_INFO the gateway's discovery reads
// (Slurm src/plugins/data_parser/v0.0.44/parsers.c, PARSER_ARRAY(JOB_INFO),
// opened 2026-10-26). JobState is a flag array, e.g. ["RUNNING"] or
// ["RUNNING","COMPLETING"].
type SlurmJob struct {
	JobID     uint32   `json:"job_id"`
	Name      string   `json:"name"`
	Comment   string   `json:"comment"`
	Account   string   `json:"account"`
	Partition string   `json:"partition"`
	QOS       string   `json:"qos"`
	UserName  string   `json:"user_name"`
	BatchHost string   `json:"batch_host"`
	Nodes     string   `json:"nodes"`
	JobState  []string `json:"job_state"`
}

// Slurmrestd is a fake slurmrestd: GET /slurm/<version>/jobs/ answers the
// jobs set with SetJobs, in the shape of OPENAPI_JOB_INFO_RESP (Slurm
// src/slurmrestd/plugins/openapi/slurmctld/api.c), to requests carrying
// X-SLURM-USER-TOKEN = Token (rest_auth/jwt). SetDown simulates an outage
// (502, as when slurmctld cannot be reached).
type Slurmrestd struct {
	// Version is the data_parser version served ("v0.0.44" when empty);
	// Token the JWT required.
	Version string
	Token   string

	mu       sync.Mutex
	jobs     []SlurmJob
	down     bool
	requests int
	denied   int
}

// SetJobs replaces the job list.
func (s *Slurmrestd) SetJobs(jobs ...SlurmJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append([]SlurmJob{}, jobs...)
}

// SetDown starts or ends an outage.
func (s *Slurmrestd) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// Requests returns the requests served and those refused for their token.
func (s *Slurmrestd) Requests() (served, denied int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.denied
}

func (s *Slurmrestd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ver := s.Version
	if ver == "" {
		ver = "v0.0.44"
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-SLURM-USER-TOKEN") != s.Token || s.Token == "" {
		s.denied++
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"error": "Authentication failure", "error_number": 1}}})
		return
	}
	s.requests++
	if s.down {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"error": "Unable to contact slurm controller"}}})
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/slurm/"+ver+"/jobs/" {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"error": "Unable to find path"}}})
		return
	}
	jobs := s.jobs
	if jobs == nil {
		jobs = []SlurmJob{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jobs": jobs, "last_backfill": map[string]any{"set": true, "number": time.Now().Unix()},
		"last_update": map[string]any{"set": true, "number": time.Now().Unix()},
		"meta":        map[string]any{"plugin": map[string]any{"type": "openapi/slurmctld", "data_parser": "data_parser/" + ver}},
		"errors":      []any{}, "warnings": []any{},
	})
}
