package chaos

import (
	"io"
	"net"
	"sync"
	"time"
)

// Proxy is a TCP relay with faults: between the gateway and PostgreSQL (a
// restart, slow queries) or between two replicas of an HA group (a partition).
// There is no Docker on the workstation, so a PostgreSQL restart is what the
// gateway sees of one: every connection reset, then refused for a while.
type Proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	mode   string
	delay  time.Duration
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// Proxy modes.
const (
	ProxyPass      = "pass"
	ProxyBlackhole = "blackhole" // accepts, relays nothing: a partition
	ProxyRefuse    = "refuse"    // accepts and closes at once: a server that is down
	ProxySlow      = "slow"      // every chunk is delayed: slow queries
)

// NewProxy listens on loopback and relays to target.
func NewProxy(target string) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln, target: target, mode: ProxyPass, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Addr is the proxy's address.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Set changes the mode; a change away from pass resets the open connections
// (a restart drops them), delay applies to ProxySlow.
func (p *Proxy) Set(mode string, delay time.Duration) {
	p.mu.Lock()
	p.mode, p.delay = mode, delay
	var drop []net.Conn
	if mode == ProxyRefuse || mode == ProxyBlackhole {
		for c := range p.conns {
			drop = append(drop, c)
		}
	}
	p.mu.Unlock()
	for _, c := range drop {
		_ = c.Close()
	}
}

// Close stops the proxy and every relay.
func (p *Proxy) Close() {
	_ = p.ln.Close()
	p.mu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.pipe(c) }()
	}
}

func (p *Proxy) state() (string, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode, p.delay
}

func (p *Proxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
	p.mu.Unlock()
}

func (p *Proxy) pipe(c net.Conn) {
	p.track(c, true)
	defer func() { p.track(c, false); _ = c.Close() }()
	switch m, _ := p.state(); m {
	case ProxyRefuse:
		return
	case ProxyBlackhole:
		buf := make([]byte, 4096)
		for { // read and discard until the connection is dropped
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}
	up, err := net.DialTimeout("tcp", p.target, 3*time.Second)
	if err != nil {
		return
	}
	p.track(up, true)
	defer func() { p.track(up, false); _ = up.Close() }()
	done := make(chan struct{}, 2)
	relay := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				m, d := p.state()
				if m == ProxySlow {
					time.Sleep(d)
				}
				if m == ProxyBlackhole {
					continue // swallowed
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go relay(up, c)
	go relay(c, up)
	<-done
	_ = c.Close()
	_ = up.Close()
	<-done
	_ = io.EOF
}
