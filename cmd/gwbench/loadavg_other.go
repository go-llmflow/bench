//go:build !darwin

package main

import "errors"

func darwinLoadavg() (float64, error) { return 0, errors.New("not darwin") }
