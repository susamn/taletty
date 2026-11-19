package audible

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pythonFixture(t *testing.T) string {
	t.Helper()
	if python := os.Getenv("TALETTY_TEST_PYTHON"); python != "" {
		return python
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("contract tests require CPython 3.12 with adapter requirements installed", err)
	}
	return python
}

func doctorContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestActualPythonSDKReadinessLeavesFreshRootUntouched(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("PYTHONPATH", filepath.Join(root, "must-not-import"))
	sentinel := filepath.Join(root, "old-app-fixture")
	const content = "synthetic fixture, not credentials"
	if err := os.WriteFile(sentinel, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Doctor(doctorContext(t), pythonFixture(t))
	if err != nil || report.Python != Ready || report.Audible != Ready {
		t.Fatal(report, err)
	}
	if os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1" && !report.Ready() {
		t.Fatal("installed CI media tools not ready", report)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatal("doctor created user state", entries, err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != content {
		t.Fatal("old fixture changed", err)
	}
}

const fakeDependencies = `
import importlib, importlib.metadata, shutil, subprocess, types
class NoProvider:
    def __getattr__(self, name):
        raise AssertionError("doctor attempted provider access")
importlib.metadata.version = lambda name: "0.13.1"
importlib.import_module = lambda name: NoProvider()
shutil.which = lambda name: "/fixture/tool"
subprocess.run = lambda *args, **kwargs: types.SimpleNamespace(returncode=0)
`

func TestRealPythonBootstrapWithFakeProviderAndDependencyFailures(t *testing.T) {
	python, ctx := pythonFixture(t), doctorContext(t)
	for _, tc := range []struct {
		name, prelude string
		want          Diagnostics
	}{
		{"ready", fakeDependencies, Diagnostics{Ready, Ready, Ready, Ready}},
		{"sdk-incompatible", fakeDependencies + "\nimportlib.metadata.version = lambda name: 'not-the-pin'\n", Diagnostics{Ready, RuntimeIncompatible, Ready, Ready}},
		{"sdk-missing", fakeDependencies + "\ndef missing(name): raise importlib.metadata.PackageNotFoundError()\nimportlib.metadata.version = missing\n", Diagnostics{Ready, RuntimeMissing, Ready, Ready}},
		{"tools-missing", fakeDependencies + "\nshutil.which = lambda name: None\n", Diagnostics{Ready, Ready, RuntimeMissing, RuntimeMissing}},
		{"tools-incompatible", fakeDependencies + "\nsubprocess.run = lambda *a, **k: types.SimpleNamespace(returncode=7)\n", Diagnostics{Ready, Ready, RuntimeIncompatible, RuntimeIncompatible}},
		{"python-incompatible", fakeDependencies + "\nimport sys\nsys.version_info = (3, 11)\n", Diagnostics{RuntimeIncompatible, NotChecked, NotChecked, NotChecked}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := doctor(ctx, python, tc.prelude+doctorSource)
			if err != nil || report != tc.want {
				t.Fatal(report, err)
			}
			if report.Ready() != (tc.name == "ready") {
				t.Fatal("false readiness", report)
			}
		})
	}
}

func replyScript(payload map[string]any, extra string) string {
	raw, _ := json.Marshal(payload)
	return `import json,sys
request=json.load(sys.stdin)
frame={"protocol":1,"operation_id":request["operation_id"],"seq":1,"type":"result"}
frame.update(json.loads('` + string(raw) + `'))
print(json.dumps(frame),flush=True)
` + extra
}

func TestMalformedOrFailedRuntimeNeverLeaksRawOutput(t *testing.T) {
	python, ctx := pythonFixture(t), doctorContext(t)
	good := map[string]any{"python": "ready", "audible": "ready", "ffmpeg": "ready", "ffprobe": "ready"}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"python", nil}, {"python", "runtime_missing"}, {"python", "runtime_incompatible"},
		{"audible", "not_checked"}, {"ffmpeg", "fixture-private-exception"}, {"ffprobe", 4},
	} {
		payload := make(map[string]any)
		for key, value := range good {
			payload[key] = value
		}
		payload[tc.key] = tc.value
		if report, err := doctor(ctx, python, replyScript(payload, "")); err != ErrRuntimeIncompatible || report != (Diagnostics{}) {
			t.Fatal(report, err)
		}
	}
	for _, payload := range []map[string]any{
		{}, {"PYTHON": "ready", "audible": "ready", "ffmpeg": "ready", "ffprobe": "ready"},
		{"python": "runtime_incompatible", "audible": "not_checked", "ffmpeg": "ready", "ffprobe": "not_checked"},
		{"python": "runtime_incompatible", "audible": "not_checked", "ffmpeg": "not_checked", "ffprobe": "ready"},
		{"type": "error"}, {"seq": 2}, {"protocol": 2},
	} {
		if report, err := doctor(ctx, python, replyScript(payload, "")); err != ErrRuntimeIncompatible || report != (Diagnostics{}) {
			t.Fatal(report, err)
		}
	}
	for _, script := range []string{"print('fixture-private-banner')", replyScript(good, "sys.exit(9)\n")} {
		report, err := doctor(ctx, python, script)
		if err != ErrRuntimeIncompatible || report != (Diagnostics{}) || strings.Contains(err.Error(), "private") {
			t.Fatal(report, err)
		}
	}
	// Optional/case-alias fields cannot override exact required status names or leak.
	good["PYTHON"], good["future"] = "fixture-private", "fixture-private"
	report, err := doctor(ctx, python, replyScript(good, ""))
	if err != nil || !report.Ready() || strings.Contains(fmt.Sprint(report), "private") {
		t.Fatal(report, err)
	}
}

func TestActiveDoctorCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(doctorContext(t))
	defer cancel()
	done := make(chan error, 1)
	python := pythonFixture(t)
	go func() {
		// Fixture-only Unix IPC is an exact running-child barrier, not provider
		// network or a sleep/poll. The production probe never creates this socket.
		script := fmt.Sprintf("import sys,socket,signal\nsys.stdin.read()\ns=socket.socket(socket.AF_UNIX)\ns.connect(%q)\nsignal.pause()\n", path)
		report, err := doctor(ctx, python, script)
		if report != (Diagnostics{}) {
			done <- fmt.Errorf("partial diagnostics after cancel")
			return
		}
		done <- err
	}()
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal("runtime never reached barrier", err)
	}
	peer.Close()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("doctor did not join canceled transport")
	}
}

func TestMissingAndCanceledRuntime(t *testing.T) {
	if report, err := Doctor(doctorContext(t), "/nonexistent/fixture-private-runtime"); err != ErrRuntimeMissing || report != (Diagnostics{}) {
		t.Fatal(report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Doctor(ctx, "/nonexistent"); err != context.Canceled {
		t.Fatal(err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Time{})
	defer stop()
	if _, err := Doctor(expired, "/nonexistent"); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
}
