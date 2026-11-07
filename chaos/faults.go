package chaos

import (
	"fmt"
	"sort"
	"time"
)

// FaultKinds is the catalogue: the name of every fault a scenario may use.
func FaultKinds() []string {
	k := make([]string, 0, len(faultTable))
	for n := range faultTable {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

type faultFn func(w *World, f FaultAt) (undo func(), err error)

var faultTable = map[string]faultFn{
	// upstream
	"upstream-5xx": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.Status, u.StatusEvery = int(f.Arg), int(f.Arg2) })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.Status, u.StatusEvery = 0, 0 }) }, nil
	},
	"upstream-hang": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.Hang = time.Duration(f.Arg * float64(time.Second)) })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.Hang = 0 }) }, nil
	},
	"upstream-slow-tokens": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.TokenDelay = time.Duration(f.Arg) * time.Millisecond })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.TokenDelay = 0 }) }, nil
	},
	"upstream-cut-sse": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.CutAfter = int(f.Arg) })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.CutAfter = 0 }) }, nil
	},
	"upstream-wrong-usage": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.UsageMul, u.UsageAdd = f.Arg, int(f.Arg2) })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.UsageMul, u.UsageAdd = 0, 0 }) }, nil
	},
	"upstream-no-usage": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.NoUsage = true })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.NoUsage = false }) }, nil
	},
	"models-down": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.ModelsDown = true })
		return func() { w.Up.Update(func(u *UpstreamFaults) { u.ModelsDown = false }) }, nil
	},
	"models-up": func(w *World, f FaultAt) (func(), error) {
		w.Up.Update(func(u *UpstreamFaults) { u.ModelsDown = false })
		return nil, nil
	},
	"preemption-burst": func(w *World, f FaultAt) (func(), error) {
		w.Up.PreemptionBurst(int64(f.Arg), int64(f.Arg2))
		return w.Up.Calm, nil
	},
	// identity providers
	"idp-down": func(w *World, f FaultAt) (func(), error) {
		p, err := w.idp(f.Target)
		if err != nil {
			return nil, err
		}
		p.SetDown(true)
		return func() { p.SetDown(false) }, nil
	},
	"idp-rotate": func(w *World, f FaultAt) (func(), error) {
		p, err := w.idp(f.Target)
		if err != nil {
			return nil, err
		}
		if f.Arg != 0 {
			// a token signed with the key that is about to leave the key set,
			// kept to be presented after the gateway has refetched the keys.
			if tok, err := w.mintFor(f.Target, "old-key-probe", "", 10*time.Minute); err == nil {
				w.mu.Lock()
				if w.oldToken == nil {
					w.oldToken = map[string]string{}
				}
				w.oldToken[f.Target] = tok
				w.mu.Unlock()
			}
		}
		p.Rotate(f.Arg != 0)
		return nil, nil
	},
	"idp-skew": func(w *World, f FaultAt) (func(), error) {
		p, err := w.idp(f.Target)
		if err != nil {
			return nil, err
		}
		p.SetSkew(time.Duration(f.Arg * float64(time.Second)))
		return func() { p.SetSkew(0) }, nil
	},
	// BMC and DCGM
	"bmc-offline": func(w *World, f FaultAt) (func(), error) {
		if w.BMC == nil {
			return nil, fmt.Errorf("no BMC in this scenario")
		}
		w.BMC.SetOffline(true)
		return func() { w.BMC.SetOffline(false) }, nil
	},
	"bmc-counter-reset": func(w *World, f FaultAt) (func(), error) {
		if w.BMC == nil {
			return nil, fmt.Errorf("no BMC in this scenario")
		}
		w.BMC.Reset(f.Arg)
		return nil, nil
	},
	"bmc-power-only": func(w *World, f FaultAt) (func(), error) {
		if w.BMC == nil {
			return nil, fmt.Errorf("no BMC in this scenario")
		}
		w.BMC.SetPowerOnly(true, nodeWatts) // what the simulated node draws, so any source gives the same energy
		return func() { w.BMC.SetPowerOnly(false, 0) }, nil
	},
	"dcgm-down": func(w *World, f FaultAt) (func(), error) {
		if w.DCGM == nil {
			return nil, fmt.Errorf("no DCGM in this scenario")
		}
		w.DCGM.SetDown(true)
		return func() { w.DCGM.SetDown(false) }, nil
	},
	// PostgreSQL (CI): through the relay between the gateways and the database.
	"pg-restart": func(w *World, f FaultAt) (func(), error) {
		if w.PGProxy == nil {
			return nil, fmt.Errorf("no PostgreSQL relay in this scenario")
		}
		w.PGProxy.Set(ProxyRefuse, 0)
		return func() { w.PGProxy.Set(ProxyPass, 0) }, nil
	},
	"pg-slow": func(w *World, f FaultAt) (func(), error) {
		if w.PGProxy == nil {
			return nil, fmt.Errorf("no PostgreSQL relay in this scenario")
		}
		w.PGProxy.Set(ProxySlow, time.Duration(f.Arg)*time.Millisecond)
		return func() { w.PGProxy.Set(ProxyPass, 0) }, nil
	},
	// HA: a partition between the replicas (the database stays reachable).
	"ha-partition": func(w *World, f FaultAt) (func(), error) {
		if len(w.HAProxy) == 0 {
			return nil, fmt.Errorf("no HA group in this scenario")
		}
		for _, p := range w.HAProxy {
			p.Set(ProxyBlackhole, 0)
		}
		return func() {
			for _, p := range w.HAProxy {
				p.Set(ProxyPass, 0)
			}
		}, nil
	},
	// a configuration reload: the gateway has no SIGHUP, so it is a graceful
	// stop and a start on the same configuration, replica by replica.
	"config-reload": func(w *World, f FaultAt) (func(), error) {
		g, err := w.replica(f.Target)
		if err != nil {
			return nil, err
		}
		if err := g.Stop(); err != nil {
			return nil, err
		}
		return func() {
			if err := g.Start(40 * time.Second); err != nil {
				w.Note("replica %s did not restart after the reload: %v", f.Target, err)
			}
		}, nil
	},
	// HA and PostgreSQL (CI)
	"replica-kill": func(w *World, f FaultAt) (func(), error) {
		g, err := w.replica(f.Target)
		if err != nil {
			return nil, err
		}
		if err := g.Kill(); err != nil {
			return nil, err
		}
		return func() {
			if err := g.Start(40 * time.Second); err != nil {
				w.Note("replica %s did not restart: %v", f.Target, err)
			}
		}, nil
	},
}

func (w *World) applyFault(f FaultAt) (func(), error) {
	fn, ok := faultTable[f.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown fault %q (known: %v)", f.Kind, FaultKinds())
	}
	return fn(w, f)
}

func (w *World) idp(name string) (*IdP, error) {
	p, ok := w.IdPs[name]
	if !ok {
		return nil, fmt.Errorf("no identity provider %q", name)
	}
	return p, nil
}

func (w *World) replica(name string) (*Gateway, error) {
	for i, g := range w.GW {
		if name == fmt.Sprintf("r%d", i) {
			return g, nil
		}
	}
	return nil, fmt.Errorf("no replica %q", name)
}
