package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

func TestRootCmdServesAndRecords(t *testing.T) {
	out := filepath.Join(t.TempDir(), "req.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrc <- a }
	defer func() { onListen = nil }()
	cmd := RootCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0", "--out", out})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not listening")
	}
	resp, err := http.Post("http://"+addr.String()+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("recording mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(out)
	var r fakeupstream.Record
	if err := json.Unmarshal(bytes.TrimSpace(b), &r); err != nil || r.Path != "/v1/chat/completions" {
		t.Fatalf("recording %s: %v", b, err)
	}
}

func TestRootCmdRejectsArgsAndBadAddr(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"--addr", "256.0.0.1:x", "--out", ""}} {
		cmd := RootCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// A recording file that already exists, with Authorization headers in it and a
// wider mode, is tightened to 0600 when fakevllm opens it (security audit
// 2026-11-05 (2): os.OpenFile's mode applies to a new file only).
func TestRootCmdTightensAnExistingRecording(t *testing.T) {
	out := filepath.Join(t.TempDir(), "req.jsonl")
	if err := os.WriteFile(out, []byte("{\"headers\":{\"Authorization\":[\"Bearer secret\"]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o644); err != nil { // the umask may have narrowed it
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrc <- a }
	defer func() { onListen = nil }()
	cmd := RootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0", "--out", out})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	select {
	case <-addrc:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not listening")
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("an existing recording keeps mode %v", st.Mode().Perm())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A recording that is not a regular file (a device, a pipe) is refused.
func TestRootCmdRefusesAnIrregularRecording(t *testing.T) {
	cmd := RootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0", "--out", os.DevNull})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err == nil {
		t.Error("a device accepted as a recording")
	}
}
