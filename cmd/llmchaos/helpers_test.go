package main

import "os"

func writeFile(p string) error { return os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755) }
