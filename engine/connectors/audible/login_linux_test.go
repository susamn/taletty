package audible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"golang.org/x/sys/unix"
)

const priorAuth = `{"fixture":"prior-private-state"}`
const fixtureRedirect = "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=synthetic-never-issued"
const fakeLoginSDK = `
import audible, httpx, subprocess, sys
webbrowser.open = lambda *args, **kwargs: False
def no_network(*args, **kwargs):
    raise AssertionError("provider network forbidden")
httpx.Client.send = no_network
def fake_login(**kwargs):
    os.write(1, b"fixture-private-sdk-stdout")
    os.write(2, b"fixture-private-sdk-stderr")
    subprocess.run([sys.executable, "-I", "-B", "-c", '''import os
for fd in (3, 4, 5):
    try: os.fstat(fd)
    except OSError: pass
    else: raise AssertionError("borrowed capability leaked")
'''], close_fds=False, check=True)
    if os.environ["TALETTY_LOGIN_HOST"] == "failure":
        raise RuntimeError("fixture-private-provider-failure")
    redirect = kwargs["login_url_callback"]("https://www.amazon.com/ap/signin?fixture=public-challenge")
    assert redirect == "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=synthetic-never-issued"
    return audible.Authenticator.from_dict({"locale_code":"us", "access_token":"Atna|synthetic-not-issued", "expires":4102444800})
audible.Authenticator.from_login_external = fake_login
`

func loginPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
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
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func TestPublicLoginRealEmbeddedPythonPTY(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel", "interrupt", "stale", "bad", "missing", "unsafe"} {
		t.Run(mode, func(t *testing.T) {
			master, slave := loginPTY(t)
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(priorAuth), 0600); err != nil {
				t.Fatal(err)
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPublicLoginHostChild$")
			cmd.Env = append(os.Environ(), "TALETTY_LOGIN_HOST="+mode, "TALETTY_LOGIN_ROOT="+root,
				"TALETTY_TEST_PYTHON="+pythonFixture(t), "GORACE=atexit_sleep_ms=0", "PYTHONPATH="+root, "HOME="+root, "XDG_CONFIG_HOME="+root)
			cmd.ExtraFiles = []*os.File{slave, reader}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Host operation has its own 5-second cancellation/cleanup budget. Wait for
			// that owner rather than killing it and abandoning its foreground child.
			if mode == "success" || mode == "cancel" || mode == "interrupt" {
				text := ""
				deadline := time.Now().Add(6 * time.Second)
				for !strings.Contains(text, "Final redirect URL") && time.Now().Before(deadline) {
					ready := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
					if n, err := unix.Poll(ready, 1000); err != nil {
						t.Fatal(err)
					} else if n == 0 {
						continue
					}
					data := make([]byte, 4096)
					n, err := master.Read(data)
					if err != nil {
						break
					}
					text += string(data[:n])
				}
				if !strings.Contains(text, "Final redirect URL") {
					cmd.Wait()
					t.Fatal("no hidden-input barrier", output.String())
				}
				switch mode {
				case "success":
					_, err = master.WriteString(fixtureRedirect + "\n")
				case "interrupt":
					_, err = master.Write([]byte{3})
				case "cancel":
					_, err = writer.Write([]byte{1})
				}
				if err != nil {
					cmd.Wait()
					t.Fatal(err)
				}
			}
			if err := cmd.Wait(); err != nil || strings.TrimSpace(output.String()) != "ok" {
				t.Fatal(mode, err, output.String())
			}
			// Drain only ready PTY bytes; an echoed redirect would violate the contract.
			ready := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
			if n, _ := unix.Poll(ready, 0); n > 0 {
				data := make([]byte, 4096)
				n, _ := master.Read(data)
				if strings.Contains(string(data[:n]), "synthetic-never-issued") {
					t.Fatal("redirect echoed")
				}
			}
			destination := root
			if mode == "success" {
				destination = root + ".anchored"
				t.Cleanup(func() { os.RemoveAll(destination) })
			}
			data, err := os.ReadFile(filepath.Join(destination, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" {
				var snapshot map[string]any
				if json.Unmarshal(data, &snapshot) != nil || snapshot["locale_code"] != "us" {
					t.Fatal("no SDK snapshot")
				}
			} else if mode != "unsafe" && string(data) != priorAuth {
				t.Fatal("failure changed prior snapshot")
			}
		})
	}
}

func TestPublicLoginHostChild(t *testing.T) {
	mode := os.Getenv("TALETTY_LOGIN_HOST")
	if mode == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := os.Getenv("TALETTY_LOGIN_ROOT")
	client, err := Open(ctx, root, os.Getenv("TALETTY_TEST_PYTHON"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := Open(ctx, root, client.python); !errors.Is(err, ErrAuthBusy) {
		os.Exit(1)
	}
	client.script = loginScript(browserLoginSource + fakeLoginSDK)
	if mode == "success" {
		if os.Rename(root, root+".anchored") != nil || os.Mkdir(root, 0700) != nil {
			os.Exit(1)
		}
	}
	if mode == "stale" || mode == "unsafe" {
		client.script = `import json, os, sys
request=json.load(sys.stdin)
if os.environ["TALETTY_LOGIN_HOST"]=="unsafe":
    os.chmod("auth.json", 0o644, dir_fd=3)
print(json.dumps(dict(protocol=1, operation_id=request["operation_id"], seq=1, type="result", status="signed_in")))`
	}
	if mode == "bad" {
		client.script = `print("fixture-private-invalid-control")`
	}
	if mode == "missing" {
		client.python = "/nonexistent-taletty-python"
	}
	terminal := os.NewFile(3, "fixture-terminal")
	before, err := unix.IoctlGetTermios(3, unix.TCGETS)
	if err != nil {
		os.Exit(1)
	}
	group, err := unix.IoctlGetInt(3, unix.TIOCGPGRP)
	if err != nil {
		os.Exit(1)
	}
	joined := make(chan struct{})
	cancelBarrier := os.NewFile(4, "cancel-barrier")
	if mode == "cancel" {
		go func() {
			var token [1]byte
			io.ReadFull(cancelBarrier, token[:])
			cancel()
			close(joined)
		}()
	} else {
		close(joined)
	}
	err = client.Login(ctx, terminal)
	cancelBarrier.Close()
	<-joined
	after, modeErr := unix.IoctlGetTermios(3, unix.TCGETS)
	actualGroup, groupErr := unix.IoctlGetInt(3, unix.TIOCGPGRP)
	if modeErr != nil || groupErr != nil || *after != *before || actualGroup != group {
		os.Exit(1)
	}
	switch mode {
	case "success":
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "cancel", "interrupt":
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "failure":
		var failure *LoginError
		if !errors.As(err, &failure) || failure.Code != "login_failed" {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "missing":
		if !errors.Is(err, ErrRuntimeMissing) {
			os.Exit(1)
		}
	case "unsafe":
		if !errors.Is(err, ErrAuthStorage) {
			os.Exit(1)
		}
	default:
		if !errors.Is(err, ErrRuntimeIncompatible) {
			os.Exit(1)
		}
	}
	if client.Close(context.Background()) != nil || client.Close(context.Background()) != nil {
		os.Exit(1)
	}
	if !errors.Is(client.Login(context.Background(), terminal), ErrClosed) {
		os.Exit(1)
	}
	os.Stdout.WriteString("ok\n")
	os.Exit(0)
}

func TestClientOwnershipAndPrivateMetadata(t *testing.T) {
	if _, err := Open(context.Background(), "relative", "unused"); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, t.TempDir(), "unused"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	client, err := Open(context.Background(), root, "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	if err := client.Login(context.Background(), nil); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	if err := client.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	path := filepath.Join(root, "auth.json")
	for _, mode := range []os.FileMode{0600, 0644} {
		if err := os.WriteFile(path, []byte("opaque"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		err := client.session.WithRoot(context.Background(), func(_ context.Context, fd *os.File) error { _, err := authIdentity(fd); return err })
		if mode == 0600 && err != nil || mode != 0600 && !errors.Is(err, ErrAuthStorage) {
			t.Fatal(err)
		}
	}
	if err := client.Login(context.Background(), nil); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-fixture", path); err != nil {
		t.Fatal(err)
	}
	if err := client.Login(context.Background(), nil); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}
	closed, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := authIdentity(closed); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}
	if err := authError(errors.New("fixture-private-storage")); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}
}

func TestTypedLoginOutcomeRejectsUntrustedFields(t *testing.T) {
	good := `{"protocol":1,"operation_id":"fixture","seq":1,"type":"result","status":"signed_in"}`
	frame := bridge.Frame{Seq: 1, Type: bridge.Result, Raw: json.RawMessage(good)}
	if err := loginOutcome(frame); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"bad-json", `{}`, strings.Replace(good, `"signed_in"`, `"other"`, 1), strings.Replace(good, `"status"`, `"Status"`, 1), strings.Replace(good, `"signed_in"`, `true`, 1)} {
		frame.Raw = json.RawMessage(raw)
		if !errors.Is(loginOutcome(frame), ErrRuntimeIncompatible) {
			t.Fatal("accepted malformed outcome")
		}
	}
	frame.Raw = json.RawMessage(strings.TrimSuffix(good, "}") + `,"extra":"fixture-private"}`)
	if err := loginOutcome(frame); err != nil {
		t.Fatal("optional field rejected", err)
	}
	frame.Raw = json.RawMessage(good)
	frame.Seq = 2
	if !errors.Is(loginOutcome(frame), ErrRuntimeIncompatible) {
		t.Fatal("accepted extra frames")
	}
	frame.Seq = 1
	frame.Type = bridge.Failure
	for code := range loginMessages {
		frame.Raw = json.RawMessage(`{"protocol":1,"operation_id":"fixture","seq":1,"type":"error","failure":{"code":"` + code + `"}}`)
		err := loginOutcome(frame)
		if code == "canceled" {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		} else {
			var failure *LoginError
			if !errors.As(err, &failure) || failure.Code != code || failure.Error() == "" {
				t.Fatal(err)
			}
		}
	}
	for _, failure := range []string{`null`, `{"code":"unknown"}`, `{"Code":"login_failed"}`, `{"code":true}`} {
		frame.Raw = json.RawMessage(`{"protocol":1,"operation_id":"fixture","seq":1,"type":"error","failure":` + failure + `}`)
		if !errors.Is(loginOutcome(frame), ErrRuntimeIncompatible) {
			t.Fatal("accepted invalid failure")
		}
	}
	if (&LoginError{Code: "private"}).Error() != "Audible sign-in failed." {
		t.Fatal("unsafe unknown code")
	}
}
