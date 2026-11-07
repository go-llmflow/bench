package chaos

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// goroutines is the number of goroutines of this process.
func goroutines() int { return runtime.NumGoroutine() }

// fdCount is the number of open file descriptors of pid, -1 when unknown.
// Linux reads /proc/<pid>/fd; elsewhere lsof lists them (one line per fd).
func fdCount(pid int) int {
	if pid <= 0 {
		return -1
	}
	if ents, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd"); err == nil {
		return len(ents)
	}
	if pid == os.Getpid() {
		if ents, err := os.ReadDir("/dev/fd"); err == nil {
			return len(ents)
		}
	}
	out, err := exec.Command("lsof", "-n", "-P", "-p", strconv.Itoa(pid), "-Ff").Output()
	if err != nil && len(out) == 0 {
		return -1
	}
	n := 0
	for _, l := range bytes.Split(out, []byte("\n")) {
		if bytes.HasPrefix(l, []byte("f")) {
			f := string(l[1:])
			if f == "cwd" || f == "txt" || f == "rtd" || strings.HasPrefix(f, "mem") {
				continue
			}
			n++
		}
	}
	return n
}

// Load1 is the 1-minute load average, read through `uptime`-free means: the
// kernel's value on Linux, sysctl on macOS.
func Load1() (float64, error) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			return strconv.ParseFloat(f[0], 64)
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, err
	}
	// "{ 4.49 3.80 3.12 }"
	f := strings.Fields(strings.Trim(string(out), "{} \n"))
	if len(f) == 0 {
		return 0, errors.New("sysctl vm.loadavg: empty")
	}
	return strconv.ParseFloat(f[0], 64)
}

// WaitLoad blocks until the 1-minute load is at most max, or gives up after
// wait with an error that names the load seen. wait 0: one reading.
func WaitLoad(max float64, wait time.Duration, logf func(string, ...any)) (float64, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := Load1()
		if err != nil {
			return 0, fmt.Errorf("load gate: %w", err)
		}
		if l <= max {
			return l, nil
		}
		if time.Now().After(deadline) {
			return l, fmt.Errorf("1-minute load %.2f stays above %.2f: deferred (a measurement under load is not a measurement)", l, max)
		}
		if logf != nil {
			logf("load %.2f > %.2f, waiting", l, max)
		}
		time.Sleep(15 * time.Second)
	}
}
