package fakeupstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDCGM(t *testing.T) {
	d := NewDCGM("GPU-a", "GPU-b")
	srv := httptest.NewServer(d)
	defer srv.Close()
	d.Add("GPU-a", 1500)
	d.Add("GPU-b", 7)
	code, body := scrape(t, srv.URL+"/metrics")
	if code != 200 || !strings.Contains(body, `DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{gpu="0",UUID="GPU-a",device="nvidia0",Hostname="fake"} 1500`) ||
		!strings.Contains(body, `UUID="GPU-b",device="nvidia1",Hostname="fake"} 7`) || !strings.Contains(body, "DCGM_FI_DEV_POWER_USAGE") {
		t.Fatalf("%d\n%s", code, body)
	}
	d.Reset("GPU-a", 40) // a driver reload
	if _, body = scrape(t, srv.URL+"/metrics"); !strings.Contains(body, "GPU-a\",device=\"nvidia0\",Hostname=\"fake\"} 40\n") {
		t.Fatalf("reset: %s", body)
	}
	if d.Added() != 1507 {
		t.Fatalf("added %d: a reset adds nothing", d.Added())
	}
	d.SetDown(true)
	if code, _ = scrape(t, srv.URL+"/metrics"); code != 503 {
		t.Fatalf("down: %d", code)
	}
	if code, _ = scrape(t, srv.URL+"/other"); code != 503 || d.Hits() != 4 {
		t.Fatalf("%d hits %d", code, d.Hits())
	}
}
