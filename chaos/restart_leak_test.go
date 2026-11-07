//go:build !windows

package chaos

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for a gateway: `serve --config F`
// listens on the address F holds and answers /healthz.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "serve" && os.Getenv("CHAOS_FAKE_GATEWAY") == "1" {
		b, _ := os.ReadFile(os.Args[3])
		ln, err := net.Listen("tcp", string(b))
		if err != nil {
			os.Exit(3)
		}
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A gateway that is killed and started again must leave nothing behind in the
// harness: the leak invariant counts the harness's goroutines and descriptors.
func TestRestartingAGatewayLeaksNothing(t *testing.T) {
	t.Setenv("CHAOS_FAKE_GATEWAY", "1")
	addr, err := FreeAddr()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gateway.hcl"), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Bin: os.Args[0], Dir: dir, URL: "http://" + addr}
	settle := func() (int, int) {
		time.Sleep(500 * time.Millisecond)
		return goroutines(), fdCount(osPid())
	}
	if err := g.Start(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	_ = g.Kill()
	g0, f0 := settle()
	for i := 0; i < 3; i++ {
		if err := g.Start(20 * time.Second); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			_ = g.Kill()
		} else {
			_ = g.Stop()
		}
	}
	// a second Start of a running gateway is refused, never a second process
	if err := g.Start(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(time.Second); err == nil {
		t.Fatal("a running gateway was started twice")
	}
	_ = g.Kill()
	g1, f1 := settle()
	if g1 > g0 || f1 > f0 {
		t.Fatalf("three restarts: goroutines %d -> %d, descriptors %d -> %d", g0, g1, f0, f1)
	}
}
