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
