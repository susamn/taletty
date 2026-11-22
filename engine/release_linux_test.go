package engine_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/susamn/taletty/engine/connectors/audible"
)

func preparedPython(t *testing.T) string {
	t.Helper()
	if python := os.Getenv("TALETTY_TEST_PYTHON"); python != "" {
		return python
	}
	for _, name := range []string{"python3.12", "python3"} {
		if python, err := exec.LookPath(name); err == nil {
			return python
		}
	}
	t.Fatal("release tests require CPython 3.12 with adapter requirements installed")
	return ""
}

func withEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[key]; replaced {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		if value != "" || key == "TALETTY_PYTHON" || key == "PATH" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func TestCleanTempDirInstallAndDoctor(t *testing.T) {
	engineDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engineDir, "go.mod")); err != nil {
		t.Fatalf("expected engine module root, got %q: %v", engineDir, err)
	}

	goBin := t.TempDir()
	installCmd := exec.Command("go", "install", "./cmd/taletty-engine")
	installCmd.Dir = engineDir
	installCmd.Env = withEnv(map[string]string{
		"GOWORK":      "off",
		"GOTOOLCHAIN": "local",
		"GOBIN":       goBin,
	})
	if out, err := installCmd.CombinedOutput(); err != nil {
		t.Fatalf("go install ./cmd/taletty-engine failed: %v\n%s", err, out)
	}
	binPath := filepath.Join(goBin, "taletty-engine")

	python := preparedPython(t)
	pathDir := t.TempDir()
	wrapper := filepath.Join(pathDir, "python3.12")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \""+python+"\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	decoyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(decoyDir, "python3.12"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{
			name: "prepared-via-env",
			env: map[string]string{
				"TALETTY_PYTHON": python,
				"PATH":           decoyDir + ":" + os.Getenv("PATH"),
			},
		},
		{
			name: "prepared-via-path",
			env: map[string]string{
				"TALETTY_PYTHON": "",
				"PATH":           pathDir + ":" + os.Getenv("PATH"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{
				"HOME":            home,
				"XDG_CONFIG_HOME": filepath.Join(home, "ignored-xdg"),
			}
			for k, v := range tc.env {
				env[k] = v
			}
			cmd := exec.Command(binPath, "doctor", "--json")
			cmd.Env = withEnv(env)
			out, err := cmd.CombinedOutput()
			var payload struct {
				Ready       bool                `json:"ready"`
				Diagnostics audible.Diagnostics `json:"diagnostics"`
			}
			if jsonErr := json.Unmarshal(out, &payload); jsonErr != nil {
				t.Fatalf("invalid doctor JSON (%v): %s", jsonErr, out)
			}
			if payload.Diagnostics.Python != audible.Ready || payload.Diagnostics.Audible != audible.Ready {
				t.Fatalf("unexpected python/audible readiness: %+v (out=%s)", payload, out)
			}
			if os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1" && (!payload.Ready || !payload.Diagnostics.Ready()) {
				t.Fatalf("CI media tools not ready: %+v", payload)
			}
			wantCode := 1
			if payload.Ready {
				wantCode = 0
			}
			if exitStatus(err) != wantCode {
				t.Fatalf("got exit %d, want %d (out=%s err=%v)", exitStatus(err), wantCode, out, err)
			}
			if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
				t.Fatalf("doctor created state in HOME: %v %v", entries, err)
			}
		})
	}

	t.Run("missing-runtime", func(t *testing.T) {
		home := t.TempDir()
		legacyBin := filepath.Join(home, ".config", "audible-tui", "venv", "bin")
		if err := os.MkdirAll(legacyBin, 0700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(legacyBin, "python3.12")
		if err := os.WriteFile(sentinel, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(binPath, "doctor", "--json")
		cmd.Env = withEnv(map[string]string{
			"HOME":            home,
			"XDG_CONFIG_HOME": filepath.Join(home, "ignored-xdg"),
			"TALETTY_PYTHON":  "",
			"PATH":            t.TempDir(),
		})
		out, err := cmd.CombinedOutput()
		if exitStatus(err) != 1 {
			t.Fatalf("got exit %d, want 1: %s", exitStatus(err), out)
		}
		var payload struct {
			Failure struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Retry   string `json:"retry"`
			} `json:"failure"`
		}
		if jsonErr := json.Unmarshal(out, &payload); jsonErr != nil || payload.Failure.Code != "runtime_missing" {
			t.Fatalf("unexpected missing-runtime JSON (%v): %s", jsonErr, out)
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "taletty")); !os.IsNotExist(err) {
			t.Fatalf("missing-runtime doctor created taletty root: %v", err)
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "#!/bin/sh\nexit 1\n" {
			t.Fatalf("legacy sentinel modified: %v", err)
		}
	})
}

func TestExternalConsumerModule(t *testing.T) {
	engineDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	goModBytes, err := os.ReadFile(filepath.Join(engineDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goSumBytes, err := os.ReadFile(filepath.Join(engineDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}

	var requires []string
	for _, line := range strings.Split(string(goModBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "require ") {
			requires = append(requires, strings.TrimPrefix(trimmed, "require "))
		}
	}
	if len(requires) == 0 {
		t.Fatalf("expected require lines in engine/go.mod:\n%s", goModBytes)
	}

	consumerDir := t.TempDir()
	var modBuilder strings.Builder
	modBuilder.WriteString("module example.com/taletty-consumer\n\ngo 1.26.0\n\nrequire (\n\tgithub.com/susamn/taletty/engine v0.1.0\n")
	for _, req := range requires {
		modBuilder.WriteString("\t" + req + "\n")
	}
	modBuilder.WriteString(")\n\nreplace github.com/susamn/taletty/engine => " + engineDir + "\n")

	if err := os.WriteFile(filepath.Join(consumerDir, "go.mod"), []byte(modBuilder.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerDir, "go.sum"), goSumBytes, 0600); err != nil {
		t.Fatal(err)
	}

	const consumerMain = `package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
	"golang.org/x/sys/unix"
)

func must(cond bool, msg string) {
	if !cond {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
}

func main() {
	must(audible.ContractVersion == "v0.1.0", "bad ContractVersion")
	must(audible.BridgeProtocolVersion == 1, "bad BridgeProtocolVersion")
	must(audible.PinnedPythonVersion == "3.12", "bad PinnedPythonVersion")
	must(audible.PinnedAudibleVersion == "0.13.1", "bad PinnedAudibleVersion")

	resolved, err := audible.ResolvePython("", func(k string) string {
		if k == "TALETTY_PYTHON" {
			return "/env/python"
		}
		return ""
	}, nil)
	must(err == nil && resolved == "/env/python", "ResolvePython env failed")

	_, err = audible.ResolvePython("   ", func(string) string { return "/env/python" }, nil)
	must(errors.Is(err, audible.ErrRuntimeMissing), "ResolvePython did not reject whitespace")

	python := os.Getenv("TALETTY_CONSUMER_PYTHON")
	must(python != "", "missing TALETTY_CONSUMER_PYTHON")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	diag, err := audible.Doctor(ctx, python)
	must(err == nil && diag.Python == audible.Ready && diag.Audible == audible.Ready, fmt.Sprintf("Doctor failed: %+v %v", diag, err))
	if os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1" {
		must(diag.Ready(), fmt.Sprintf("CI media tools not ready: %+v", diag))
	}

	_, err = audible.Doctor(ctx, "/nonexistent-taletty-python")
	must(errors.Is(err, audible.ErrRuntimeMissing), "missing Doctor runtime did not return ErrRuntimeMissing")

	root, err := os.MkdirTemp("", "taletty-consumer-root-")
	must(err == nil, "MkdirTemp failed")
	defer os.RemoveAll(root)
	must(os.Chmod(root, 0700) == nil, "Chmod failed")

	lockFile, err := os.OpenFile(filepath.Join(root, ".auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
	must(err == nil, "open lock failed")
	defer lockFile.Close()
	must(unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil, "flock failed")

	callerRoot, err := os.Open(root)
	must(err == nil, "open root failed")
	defer callerRoot.Close()

	_, err = audible.Open(ctx, root, python)
	must(errors.Is(err, audible.ErrAuthBusy), "Open did not return ErrAuthBusy")

	client, err := audible.OpenBorrowed(ctx, callerRoot, python)
	must(err == nil, fmt.Sprintf("OpenBorrowed failed: %v", err))

	book := model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"}
	must(book.Validate() == nil, "BookID.Validate failed")
	dur := 5 * time.Minute
	b := model.Book{ID: book, Title: "Consumer Fixture", Duration: &dur}
	must(b.Validate() == nil, "Book.Validate failed")

	var snap model.LibrarySnapshot
	_, err = client.Library(ctx, "bad id!", func(model.OperationEvent) {})
	var failure *model.Failure
	must(errors.As(err, &failure) && failure.Code == "invalid_input" && len(snap.Books) == 0, "Library invalid_input check failed")

	snap, err = client.Library(ctx, "consumer-lib", nil)
	must(errors.As(err, &failure) && failure.Code == "not_authenticated" && len(snap.Books) == 0, fmt.Sprintf("Library not_authenticated check failed: %v", err))

	var dl model.DownloadResult
	dl, err = client.Download(ctx, "bad id!", book, nil)
	must(errors.As(err, &failure) && failure.Code == "invalid_input" && dl.State == "", "Download invalid_input check failed")

	dl, err = client.Download(ctx, "consumer-dl", book, nil)
	must(errors.As(err, &failure) && failure.Code == "not_authenticated" && dl.State == model.MediaAbsent, fmt.Sprintf("Download not_authenticated check failed: %+v %v", dl, err))

	must(client.Close(context.Background()) == nil, "borrowed Close failed")
	_, err = callerRoot.Stat()
	must(err == nil, "Close closed caller root")
	_, err = audible.Open(ctx, root, python)
	must(errors.Is(err, audible.ErrAuthBusy), "Close unlocked caller .auth.lock")

	must(unix.Flock(int(lockFile.Fd()), unix.LOCK_UN) == nil, "unlock failed")
	reopened, err := audible.Open(ctx, root, python)
	must(err == nil, fmt.Sprintf("Open after unlock failed: %v", err))
	must(reopened.Close(context.Background()) == nil, "reopened Close failed")

	fmt.Println("consumer-ok")
}
`
	if err := os.WriteFile(filepath.Join(consumerDir, "main.go"), []byte(consumerMain), 0600); err != nil {
		t.Fatal(err)
	}

	consumerBin := filepath.Join(consumerDir, "consumer")
	buildCmd := exec.Command("go", "build", "-o", consumerBin, ".")
	buildCmd.Dir = consumerDir
	buildCmd.Env = withEnv(map[string]string{
		"GOWORK":      "off",
		"GOTOOLCHAIN": "local",
		"GOPROXY":     "off",
	})
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("external consumer build failed: %v\n%s", err, out)
	}

	runCmd := exec.Command(consumerBin)
	runCmd.Env = withEnv(map[string]string{
		"TALETTY_CONSUMER_PYTHON": preparedPython(t),
	})
	out, err := runCmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "consumer-ok" {
		t.Fatalf("external consumer run failed: %v\n%s", err, out)
	}
}
