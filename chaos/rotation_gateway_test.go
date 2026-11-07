package chaos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Gateway level: set LLMCHAOS_GATEWAY to a llmflow-gateway binary. A token signed
// with a key that left the JWKS must get 401 once the gateway has refetched.
func TestGatewayRefusesATokenOfARemovedKey(t *testing.T) {
	bin := os.Getenv("LLMCHAOS_GATEWAY")
	if bin == "" {
		t.Skip("LLMCHAOS_GATEWAY not set")
	}
	dir := os.Getenv("LLMCHAOS_WORKDIR")
	if dir == "" {
		t.Skip("LLMCHAOS_WORKDIR (a durable directory) not set")
	}
	sc := &Scenario{Name: "rotation-repro", F: Features{IdPs: true}, Duration: time.Second, Load: map[string]OrgLoad{}}
	w, err := Setup(context.Background(), Options{GatewayBin: bin, WorkDir: filepath.Clean(dir), Seed: 1}, sc)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	c, _ := w.firstClientOf("idpA")
	hc := NewClient()
	send := func(tok string, id int) Outcome {
		return Do(context.Background(), hc, w.Primary().URL, Req{ID: id, Org: c.Org, Team: c.Team, Who: c.Who, Cred: Cred{Bearer: tok}, MaxTokens: 16, Words: 4, Marker: "MKetabA-s1-" + string(rune('0'+id%10))})
	}
	tok, _ := w.Token(c)
	if o := send(tok, 1); o.Status != 200 {
		t.Fatalf("a valid token: %d %s", o.Status, o.Body)
	}
	old, _ := w.mintFor("idpA", "old-key-probe", "", 10*time.Minute)
	w.IdPs["idpA"].Rotate(true)
	time.Sleep(6 * time.Second) // past min_refresh = 5 s
	tok, _ = w.Token(c)
	if o := send(tok, 2); o.Status != 200 {
		t.Fatalf("a token of the new key: %d %s", o.Status, o.Body)
	}
	if o := send(old, 3); o.Status != 401 {
		t.Fatalf("DEFECT: a token signed with the removed key got %d (want 401): %s", o.Status, o.Body)
	}
}
