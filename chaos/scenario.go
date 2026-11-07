package chaos

import "time"

// Features say which parts of the deployment a scenario builds.
type Features struct {
	Fluids, Federation, IdPs, BMC, Budgets bool
	ModelsDownAtStart                      bool // /v1/models answers 503 until a fault lifts it
}

// OrgLoad is the traffic of one organisation: Workers closed-loop clients, each
// sleeping up to Think between requests.
type OrgLoad struct {
	Workers   int
	Start     time.Duration // the workers begin this long after the run starts (a ramp)
	Think     time.Duration
	StreamPct int   // percent of requests that stream
	NPct      int   // percent that ask n=2 (and best_of=2)
	LeavePct  int   // percent that disconnect, at a random phase
	Tokens    []int // max_tokens to pick from
	Words     []int // prompt sizes to pick from
	RetryMax  int   // a refused request is retried this many times (retry storm)
}

// FaultAt is one fault of the timeline: Kind starts At the offset and is
// lifted For later (For 0: until the end of the load).
type FaultAt struct {
	Kind   string
	At     time.Duration
	For    time.Duration
	Target string  // an IdP, a replica
	Arg    float64 // kind-specific
	Arg2   float64
}

// Burst releases N requests of an organisation at the same instant.
type Burst struct {
	At     time.Duration
	Org    string
	N      int
	Edge   bool // aim at the key whose hard budget is small
	Tokens int
}

// Scenario is data: it is replayed from its name and a seed.
type Scenario struct {
	Name     string
	Doc      string
	Needs    []string // "pg": needs PostgreSQL (CI)
	F        Features
	Replicas int
	Duration time.Duration
	// UpstreamLatency is how long the fake model takes to start an answer.
	UpstreamLatency time.Duration
	// PrioritySlots > 0: the upstream runs that many requests and queues the
	// rest by (X-Vllm-Priority, arrival), as vLLM's priority scheduler does.
	PrioritySlots int
	Load          map[string]OrgLoad
	Faults        []FaultAt
	Bursts        []Burst
	// Skip lists invariants that do not apply here, with the reason, so that a
	// verdict reads NOT APPLICABLE instead of a pass that checked nothing.
	Skip map[int]string
	// Expect adds scenario specific checks by name (see checks.go).
	Expect []string
}

// rampAt is when the last owner's workers start.
func (s *Scenario) rampAt() time.Duration {
	var at time.Duration
	for _, o := range Owners {
		at = max(at, s.Load[o].Start)
	}
	return at
}

func (s *Scenario) needs(x string) bool {
	for _, n := range s.Needs {
		if n == x {
			return true
		}
	}
	return false
}
