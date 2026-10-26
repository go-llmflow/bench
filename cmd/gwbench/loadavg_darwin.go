//go:build darwin

package main

import "syscall"

// darwinLoadavg reads sysctl vm.loadavg.
func darwinLoadavg() (float64, error) {
	raw, err := syscall.Sysctl("vm.loadavg")
	if err != nil {
		return 0, err
	}
	// syscall.Sysctl drops a trailing NUL byte; pad back to the struct size.
	b := []byte(raw)
	for len(b) < 24 {
		b = append(b, 0)
	}
	return decodeDarwinLoadavg(b)
}
