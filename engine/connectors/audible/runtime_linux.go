package audible

import (
	"os"
	"os/exec"
	"strings"
)

// ResolvePython resolves the Python executable with strict precedence:
// explicit override -> TALETTY_PYTHON env -> python3.12 on PATH -> python3 on PATH.
// Empty explicit means no explicit override was supplied; whitespace-only or
// NUL-containing explicit or TALETTY_PYTHON overrides are rejected cleanly with
// ErrRuntimeMissing without falling back. It never scans ~/.config/audible-tui
// or legacy paths, and never runs pip or mutates system Python.
func ResolvePython(explicit string, getenv func(string) string, lookPath func(string) (string, error)) (string, error) {
	if explicit != "" {
		if !validPythonCandidate(explicit) {
			return "", ErrRuntimeMissing
		}
		return explicit, nil
	}
	if getenv != nil {
		if env := getenv("TALETTY_PYTHON"); env != "" {
			if !validPythonCandidate(env) {
				return "", ErrRuntimeMissing
			}
			return env, nil
		}
	}
	if lookPath != nil {
		for _, candidate := range []string{"python3.12", "python3"} {
			if path, err := lookPath(candidate); err == nil && validPythonCandidate(path) {
				return path, nil
			}
		}
	}
	return "", ErrRuntimeMissing
}

// DiscoverPython resolves the Python executable using os.Getenv and exec.LookPath.
func DiscoverPython(explicit string) (string, error) {
	return ResolvePython(explicit, os.Getenv, exec.LookPath)
}

func validPythonCandidate(candidate string) bool {
	return strings.TrimSpace(candidate) != "" && !strings.ContainsRune(candidate, 0)
}
