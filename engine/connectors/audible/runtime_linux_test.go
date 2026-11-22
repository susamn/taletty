package audible

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
)

func TestContractAndRuntimePins(t *testing.T) {
	if ContractVersion != "v0.1.0" {
		t.Fatalf("unexpected ContractVersion: %q", ContractVersion)
	}
	if BridgeProtocolVersion != 1 {
		t.Fatalf("unexpected BridgeProtocolVersion: %d", BridgeProtocolVersion)
	}
	if bridge.Version != 1 {
		t.Fatalf("unexpected bridge.Version: %d", bridge.Version)
	}
	if PinnedPythonVersion != "3.12" {
		t.Fatalf("unexpected PinnedPythonVersion: %q", PinnedPythonVersion)
	}
	if PinnedAudibleVersion != "0.13.1" {
		t.Fatalf("unexpected PinnedAudibleVersion: %q", PinnedAudibleVersion)
	}
	reqs, err := os.ReadFile(filepath.Join("internal", "bridge", "adapter", "requirements.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reqs), "audible=="+PinnedAudibleVersion) {
		t.Fatalf("requirements.txt does not match PinnedAudibleVersion %q: %s", PinnedAudibleVersion, reqs)
	}
	if !strings.Contains(doctorSource, `AUDIBLE_VERSION = "`+PinnedAudibleVersion+`"`) {
		t.Fatalf("doctor.py does not match PinnedAudibleVersion %q", PinnedAudibleVersion)
	}
}

func TestResolvePythonPrecedenceAndValidation(t *testing.T) {
	noEnv := func(string) string { return "" }
	noPath := func(string) (string, error) { return "", exec.ErrNotFound }

	t.Run("explicit-wins-over-env-and-path", func(t *testing.T) {
		got, err := ResolvePython("/explicit/python",
			func(string) string { t.Fatal("getenv called despite explicit override"); return "" },
			func(string) (string, error) { t.Fatal("lookPath called despite explicit override"); return "", nil },
		)
		if err != nil || got != "/explicit/python" {
			t.Fatalf("got (%q, %v), want (/explicit/python, nil)", got, err)
		}
	})

	t.Run("env-wins-over-path", func(t *testing.T) {
		got, err := ResolvePython("",
			func(key string) string {
				if key != "TALETTY_PYTHON" {
					t.Fatalf("unexpected env key %q", key)
				}
				return "/env/python"
			},
			func(string) (string, error) { t.Fatal("lookPath called despite TALETTY_PYTHON"); return "", nil },
		)
		if err != nil || got != "/env/python" {
			t.Fatalf("got (%q, %v), want (/env/python, nil)", got, err)
		}
	})

	t.Run("python3.12-wins-over-python3", func(t *testing.T) {
		var looked []string
		got, err := ResolvePython("", noEnv, func(name string) (string, error) {
			looked = append(looked, name)
			if name == "python3.12" {
				return "/usr/bin/python3.12", nil
			}
			return "/usr/bin/python3", nil
		})
		if err != nil || got != "/usr/bin/python3.12" || len(looked) != 1 || looked[0] != "python3.12" {
			t.Fatalf("got (%q, %v, %v)", got, err, looked)
		}
	})

	t.Run("python3-fallback", func(t *testing.T) {
		var looked []string
		got, err := ResolvePython("", noEnv, func(name string) (string, error) {
			looked = append(looked, name)
			if name == "python3" {
				return "/usr/bin/python3", nil
			}
			return "", exec.ErrNotFound
		})
		if err != nil || got != "/usr/bin/python3" || len(looked) != 2 || looked[0] != "python3.12" || looked[1] != "python3" {
			t.Fatalf("got (%q, %v, %v)", got, err, looked)
		}
	})

	t.Run("invalid-lookpath-candidate-skipped", func(t *testing.T) {
		got, err := ResolvePython("", noEnv, func(name string) (string, error) {
			if name == "python3.12" {
				return "   ", nil
			}
			return "/usr/bin/python3", nil
		})
		if err != nil || got != "/usr/bin/python3" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})

	t.Run("missing-runtime", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			getenv   func(string) string
			lookPath func(string) (string, error)
		}{
			{"nil-hooks", nil, nil},
			{"not-found", noEnv, noPath},
			{"invalid-path-results", noEnv, func(string) (string, error) { return "bad\x00path", nil }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := ResolvePython("", tc.getenv, tc.lookPath)
				if !errors.Is(err, ErrRuntimeMissing) || got != "" {
					t.Fatalf("got (%q, %v), want ErrRuntimeMissing", got, err)
				}
			})
		}
	})

	t.Run("reject-invalid-explicit-and-env-without-fallback", func(t *testing.T) {
		for _, bad := range []string{" ", "\t\n", "bad\x00python"} {
			got, err := ResolvePython(bad,
				func(string) string { t.Fatal("fell back to env on invalid explicit"); return "/env/python" },
				func(string) (string, error) {
					t.Fatal("fell back to PATH on invalid explicit")
					return "/usr/bin/python3.12", nil
				},
			)
			if !errors.Is(err, ErrRuntimeMissing) || got != "" {
				t.Fatalf("explicit %q: got (%q, %v), want ErrRuntimeMissing", bad, got, err)
			}

			got, err = ResolvePython("",
				func(string) string { return bad },
				func(string) (string, error) {
					t.Fatal("fell back to PATH on invalid TALETTY_PYTHON")
					return "/usr/bin/python3.12", nil
				},
			)
			if !errors.Is(err, ErrRuntimeMissing) || got != "" {
				t.Fatalf("env %q: got (%q, %v), want ErrRuntimeMissing", bad, got, err)
			}
		}
	})
}

func TestDiscoverPythonNeverScansLegacyPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	legacyBin := filepath.Join(home, ".config", "audible-tui", "venv", "bin")
	if err := os.MkdirAll(legacyBin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"python", "python3", "python3.12"} {
		p := filepath.Join(legacyBin, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}

	emptyPath := t.TempDir()
	t.Setenv("TALETTY_PYTHON", "")
	t.Setenv("PATH", emptyPath)
	if got, err := DiscoverPython(""); !errors.Is(err, ErrRuntimeMissing) || got != "" {
		t.Fatalf("legacy path discovered or unexpected runtime: (%q, %v)", got, err)
	}

	// Verify PATH discovery picks python3.12 over python3, TALETTY_PYTHON overrides PATH,
	// and explicit overrides TALETTY_PYTHON.
	pathDir := t.TempDir()
	py3 := filepath.Join(pathDir, "python3")
	py312 := filepath.Join(pathDir, "python3.12")
	for _, p := range []string{py3, py312} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)
	if got, err := DiscoverPython(""); err != nil || got != py312 {
		t.Fatalf("PATH discovery got (%q, %v), want %q", got, err, py312)
	}
	if err := os.Remove(py312); err != nil {
		t.Fatal(err)
	}
	if got, err := DiscoverPython(""); err != nil || got != py3 {
		t.Fatalf("PATH fallback got (%q, %v), want %q", got, err, py3)
	}

	t.Setenv("TALETTY_PYTHON", "/custom/env/python")
	if got, err := DiscoverPython(""); err != nil || got != "/custom/env/python" {
		t.Fatalf("TALETTY_PYTHON got (%q, %v)", got, err)
	}
	if got, err := DiscoverPython("/custom/explicit/python"); err != nil || got != "/custom/explicit/python" {
		t.Fatalf("explicit got (%q, %v)", got, err)
	}
}
