package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCommandChild(t *testing.T) {
	raw := os.Getenv("TALETTY_CLI_TEST_ARGS")
	if raw == "" {
		return
	}
	var args []string
	if json.Unmarshal([]byte(raw), &args) != nil {
		os.Exit(2)
	}
	os.Args = append([]string{"taletty-engine"}, args...)
	main()
}
func command(t *testing.T, home string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	raw, _ := json.Marshal(args)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandChild$")
	cmd.Env = append(os.Environ(), "TALETTY_CLI_TEST_ARGS="+string(raw), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "ignored"), "GORACE=atexit_sleep_ms=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
func fakePython(t *testing.T) string {
	t.Helper()
	python := os.Getenv("TALETTY_TEST_PYTHON")
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(t.TempDir(), "python-fixture")
	source := "#!" + python + `
import json, os, socket, sys
request=json.load(sys.stdin)
if "TALETTY_CLI_BARRIER" in os.environ:
    with socket.socket(socket.AF_UNIX) as barrier:
        barrier.connect(os.environ["TALETTY_CLI_BARRIER"])
        barrier.sendall((str(os.getpid())+"\n").encode())
        barrier.recv(1)
print(json.dumps(dict(protocol=1, operation_id=request["operation_id"], seq=1, type="result", python="ready", audible="ready", ffmpeg="ready", ffprobe="ready")))
`
	if err := os.WriteFile(script, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return script
}
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func TestActualEntrypointDoctorUsageAndNonTTY(t *testing.T) {
	for _, test := range []struct {
		args     []string
		code     int
		contains string
	}{
		{[]string{"--help"}, 0, "Usage:"},
		{[]string{"doctor", "--fixture-private=secret"}, 2, "Usage:"},
		{[]string{"doctor", "--python", fakePython(t), "--json"}, 0, `"ready":true`},
		{[]string{"doctor", "--python", "/nonexistent-taletty-python", "--json"}, 1, `"code":"runtime_missing"`},
		{[]string{"login", "--python", "/nonexistent-taletty-python"}, 1, "terminal ownership"},
	} {
		home := t.TempDir()
		cmd := command(t, home, test.args...)
		output, err := cmd.CombinedOutput()
		if exitCode(err) != test.code || !strings.Contains(string(output), test.contains) || strings.Contains(string(output), "secret") {
			t.Fatal("wrong command outcome", exitCode(err), string(output))
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 0 {
			t.Fatal("read-only/nonterminal command created state", err)
		}
	}
	// OS-level broken stdout reaches the safe error path (not SIGPIPE termination).
	cmd := command(t, t.TempDir(), "--help")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	defer writer.Close()
	var diagnostics bytes.Buffer
	cmd.Stdout = writer
	cmd.Stderr = &diagnostics
	if err := cmd.Run(); exitCode(err) != 1 || !strings.Contains(diagnostics.String(), "Output could not be written") {
		t.Fatal(exitCode(err), diagnostics.String())
	}
}

func TestEntrypointSignalsJoinDoctorChild(t *testing.T) {
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			socketPath := filepath.Join(t.TempDir(), "barrier")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(8 * time.Second))
			home := t.TempDir()
			cmd := command(t, home, "doctor", "--python", fakePython(t), "--json")
			cmd.Env = append(cmd.Env, "TALETTY_CLI_BARRIER="+socketPath)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal("no child barrier", err)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(connection).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err := fmt.Sscanf(line, "%d", &pid); err != nil || pid <= 1 {
				t.Fatal("bad barrier PID")
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if exitCode(err) != 130 || !strings.Contains(output.String(), `"code":"canceled"`) {
				t.Fatal(exitCode(err), output.String())
			}
			if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
				t.Fatal("doctor child not reaped", err)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatal("doctor signal created root", err)
			}
		})
	}
}

func TestEntrypointOwnedTTYRootPrivacyAndRestart(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	home := t.TempDir()
	root := filepath.Join(home, ".config", "taletty")
	// A fresh controlling session per command, with no SDK/network login attempted.
	for _, mode := range []os.FileMode{0700, 0700, 0755} {
		if _, err := os.Stat(root); err == nil {
			if err := os.Chmod(root, mode); err != nil {
				t.Fatal(err)
			}
		}
		cmd := command(t, home, "login", "--python", "/nonexistent-taletty-python")
		cmd.ExtraFiles = []*os.File{slave}
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Ctty = 3
		output, err := cmd.CombinedOutput()
		expected := "runtime could not start"
		if mode == 0755 {
			expected = "storage failed"
		}
		if exitCode(err) != 1 || !strings.Contains(string(output), expected) {
			t.Fatal(exitCode(err), string(output))
		}
		info, err := os.Stat(root)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("unsafe root repaired", err)
		}
		lock, err := os.Stat(filepath.Join(root, ".auth.lock"))
		if err != nil || lock.Mode().Perm() != 0600 {
			t.Fatal("lock privacy", err)
		}
		if _, err := os.Stat(filepath.Join(root, "auth.json")); !os.IsNotExist(err) {
			t.Fatal("credentials created without SDK")
		}
	}
}
