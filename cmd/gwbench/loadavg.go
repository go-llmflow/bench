package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// LoadAvg1 returns the 1-minute load average.
func LoadAvg1() (float64, error) {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return 0, err
		}
		return parseProcLoadavg(string(b))
	case "darwin":
		return darwinLoadavg()
	}
	return 0, errors.New("load average not supported on " + runtime.GOOS)
}

// WaitForLoad returns the current load once it is at most max. With a
// zero wait it checks once. It polls every `every` until `wait` has
// elapsed, then gives up: measuring under load gives meaningless ratios.
func WaitForLoad(ctx context.Context, read func() (float64, error), max float64, wait, every time.Duration, onWait func(float64)) (float64, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := read()
		if err != nil {
			return 0, fmt.Errorf("cannot read the load average: %w", err)
		}
		if l <= max {
			return l, nil
		}
		if wait <= 0 || time.Now().Add(every).After(deadline) {
			return l, fmt.Errorf("load average %.2f > %.2f: refusing to measure (ratios under load are meaningless)", l, max)
		}
		if onWait != nil {
			onWait(l)
		}
		select {
		case <-ctx.Done():
			return l, ctx.Err()
		case <-time.After(every):
		}
	}
}

func parseProcLoadavg(s string) (float64, error) {
	f := strings.Fields(s)
	if len(f) < 1 {
		return 0, errors.New("empty /proc/loadavg")
	}
	return strconv.ParseFloat(f[0], 64)
}

// decodeDarwinLoadavg decodes sysctl vm.loadavg, a
// struct loadavg { fixpt_t ldavg[3]; long fscale; } (xnu sys/resource.h)
// on a 64-bit little-endian Mac: three uint32, 4 bytes of padding, an int64.
func decodeDarwinLoadavg(b []byte) (float64, error) {
	if len(b) < 24 {
		return 0, errors.New("vm.loadavg too short")
	}
	ld0 := binary.LittleEndian.Uint32(b[0:4])
	fscale := int64(binary.LittleEndian.Uint64(b[16:24]))
	if fscale <= 0 {
		return 0, errors.New("vm.loadavg: bad fscale")
	}
	return float64(ld0) / float64(fscale), nil
}
