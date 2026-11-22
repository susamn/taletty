package audible

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
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
	for _, mode := range []string{"success", "borrowed", "failure", "cancel", "interrupt", "stale", "bad", "missing", "unsafe"} {
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
			if mode == "success" || mode == "borrowed" || mode == "cancel" || mode == "interrupt" {
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
				case "success", "borrowed":
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
			if mode == "success" || mode == "borrowed" {
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
	var client *Client
	var err error
	var callerRoot, lockFile *os.File
	if mode == "borrowed" {
		lockFile, err = os.OpenFile(filepath.Join(root, ".auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			os.Exit(1)
		}
		defer lockFile.Close()
		if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			os.Exit(1)
		}
		callerRoot, err = os.Open(root)
		if err != nil {
			os.Exit(1)
		}
		defer callerRoot.Close()
		client, err = OpenBorrowed(ctx, callerRoot, os.Getenv("TALETTY_TEST_PYTHON"))
	} else {
		client, err = Open(ctx, root, os.Getenv("TALETTY_TEST_PYTHON"))
	}
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
	case "success", "borrowed":
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
	if mode == "borrowed" {
		if _, err := callerRoot.Stat(); err != nil {
			os.Exit(1)
		}
		if _, err := Open(context.Background(), root, client.python); !errors.Is(err, ErrAuthBusy) {
			os.Exit(1)
		}
		if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN); err != nil {
			os.Exit(1)
		}
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

func TestOpenBorrowedClientLifecycleAndOperations(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(initialLibraryAuth), 0600); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(filepath.Join(root, ".auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	callerRoot, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer callerRoot.Close()

	python := pythonFixture(t)
	if _, err := Open(context.Background(), root, python); !errors.Is(err, ErrAuthBusy) {
		t.Fatalf("expected ErrAuthBusy while caller holds lock, got %v", err)
	}
	client, err := OpenBorrowed(context.Background(), callerRoot, python)
	if err != nil {
		t.Fatal("OpenBorrowed failed on externally locked root:", err)
	}
	closed := false
	defer func() {
		if !closed {
			client.Close(context.Background())
		}
	}()

	// Doctor remains independent and read-only alongside a borrowed client.
	diag, err := Doctor(doctorContext(t), python)
	if err != nil || diag.Python != Ready || diag.Audible != Ready {
		t.Fatal("Doctor failed alongside borrowed client:", diag, err)
	}

	// Real Library and Download fixtures work through the borrowed root.
	baseLibrarySource := strings.TrimSuffix(client.librarySource, libraryEntry)
	setSDKMode(client, baseLibrarySource, "success", "")
	publicDownloadFixture(client, "success", "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snap, err := client.Library(ctx, "borrowed-lib", nil)
	if err != nil || len(snap.Books) != 101 {
		t.Fatalf("borrowed Library failed: len=%d err=%v", len(snap.Books), err)
	}
	dl, err := client.Download(ctx, "borrowed-dl", fixtureDownloadBook, nil)
	if err != nil || dl.State != model.MediaReady || dl.Media == nil || dl.Media.Path != filepath.Join(root, "downloads", "B000000001.m4b") {
		t.Fatalf("borrowed Download failed: %+v err=%v", dl, err)
	}

	// Operations serialize via Session.token: a queued Download waits until a blocked Library finishes.
	socketPath := filepath.Join(t.TempDir(), "barrier")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(8 * time.Second))
	setSDKMode(client, baseLibrarySource, "blocked", socketPath)

	libDone := make(chan error, 1)
	go func() {
		_, err := client.Library(ctx, "borrowed-blocked", nil)
		libDone <- err
	}()
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal("blocked Library did not reach barrier:", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	dlStarted := make(chan struct{})
	dlDone := make(chan error, 1)
	go func() {
		_, err := client.Download(ctx, "borrowed-queued", fixtureDownloadBook, func(e model.OperationEvent) {
			if e.Type == model.Started {
				close(dlStarted)
			}
		})
		dlDone <- err
	}()
	<-dlStarted
	select {
	case err := <-dlDone:
		t.Fatalf("queued Download finished while Library held Session.token: %v", err)
	default:
	}
	if _, err := conn.Write([]byte("G")); err != nil {
		t.Fatal(err)
	}
	if err := <-libDone; err == nil {
		t.Fatal("expected blocked Library mode to return throttled failure")
	}
	if err := <-dlDone; err != nil {
		t.Fatalf("queued Download failed after Library released gate: %v", err)
	}

	// Close closes only the duplicated root fd and leaves caller's .auth.lock locked.
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, err := callerRoot.Stat(); err != nil {
		t.Fatal("Close closed caller's root fd:", err)
	}
	if _, err := Open(context.Background(), root, python); !errors.Is(err, ErrAuthBusy) {
		t.Fatalf("Close unlocked caller's .auth.lock: %v", err)
	}
	if _, err := client.Library(context.Background(), "after-close", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Library after Close: %v", err)
	}
	if _, err := client.Download(context.Background(), "after-close-dl", fixtureDownloadBook, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Download after Close: %v", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), root, python)
	if err != nil {
		t.Fatalf("Open after caller unlocked failed: %v", err)
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBorrowedRejectsUnsafeRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	callerRoot, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer callerRoot.Close()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenBorrowed(canceled, callerRoot, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := OpenBorrowed(context.Background(), nil, "unused"); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}

	for _, mode := range []os.FileMode{0755, 0700 | os.ModeSticky} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenBorrowed(context.Background(), callerRoot, "unused"); !errors.Is(err, ErrAuthStorage) {
			t.Fatalf("mode %v accepted: %v", mode, err)
		}
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}

	regPath := filepath.Join(root, "file")
	if err := os.WriteFile(regPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	regFile, err := os.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}
	defer regFile.Close()
	if _, err := OpenBorrowed(context.Background(), regFile, "unused"); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}

	linkPath := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, linkPath); err != nil {
		t.Fatal(err)
	}
	linkFD, err := unix.Open(linkPath, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	linkFile := os.NewFile(uintptr(linkFD), "symlink")
	defer linkFile.Close()
	if _, err := OpenBorrowed(context.Background(), linkFile, "unused"); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}

	closedFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	closedFile.Close()
	if _, err := OpenBorrowed(context.Background(), closedFile, "unused"); !errors.Is(err, ErrAuthStorage) {
		t.Fatal(err)
	}
}
