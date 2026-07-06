package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Resolved binary paths, initialized once at startup (see main.go init()).
var (
	cccPath    string // path to the ccc binary itself (for hooks/service/self-exec)
	claudePath string // path to the claude binary
)

func initPaths() {
	cccPath = resolveCccPath()
	claudePath = claudeBin()
}

// resolveCccPath returns the absolute path of the running ccc binary, falling
// back to ~/bin/ccc or a bare "ccc" on PATH.
func resolveCccPath() string {
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.EvalSymlinks(exe); err == nil {
			return abs
		}
		return exe
	}
	home, _ := os.UserHomeDir()
	cand := filepath.Join(home, "bin", "ccc")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	if p, err := exec.LookPath("ccc"); err == nil {
		return p
	}
	return "ccc"
}
