package audible

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/model"
	"golang.org/x/sys/unix"
)

const libraryEntry = "\nmain(SOURCES,load_sources,null_stdio)\n"
const librarySDKFixture = `
import audible, audible.auth, httpx, socket, stat, subprocess, sys
original_load, original_client = load_sources, audible.Client

def fixture_load(sources,names):
    package=original_load(sources,names)
    def refresh(**kwargs):
        assert MODE != "recover", "refresh was not restored"
        return dict(access_token="Atna|synthetic-refreshed-not-issued",expires=4102444800)
    audible.auth.refresh_access_token=refresh
    def item(i):
        return dict(asin=f"T{i:09d}",title="Fixture",authors=None,narrators=[],subtitle="",runtime_length_min=0)
    def response(request):
        assert request.headers["x-amz-access-token"] == "Atna|synthetic-refreshed-not-issued"
        page=int(request.url.params["page"])
        if MODE in ("empty","recover"):
            return httpx.Response(200,json={"items":[]})
        if page == 1:
            return httpx.Response(200,json={"items":[item(i) for i in range(100)]})
        if MODE == "blocked":
            with socket.socket(socket.AF_UNIX) as barrier:
                barrier.settimeout(8)
                barrier.connect(BARRIER)
                barrier.sendall((str(os.getpid())+"\n").encode())
                assert barrier.recv(1)==b"G"
        if MODE == "cancel":
            raise KeyboardInterrupt()
        status=429 if MODE in ("denied","blocked") else 200
        items=None if MODE == "malformed" else ([item(i) for i in range(100)] if MODE == "repeat" else [item(0),item(100)])
        return httpx.Response(status,json={"items":items,"message":"fixture-private-provider"})
    def client(**kwargs):
        marker=os.open("fixture-start",os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600,dir_fd=3)
        os.write(marker,b"S");os.close(marker)
        os.write(1,b"fixture-private-stdout");os.write(2,b"fixture-private-stderr")
        assert os.read(0,1)==b""
        check="import os\nfor fd in (3,4):\n try: os.fstat(fd)\n except OSError: pass\n else: raise SystemExit(2)"
        subprocess.run([sys.executable,"-I","-B","-c",check],close_fds=False,check=True)
        result=original_client(**kwargs,transport=httpx.MockTransport(response))
        if MODE == "unsupported":result.switch_marketplace("de")
        if MODE == "close":
            original_close=result.close
            def close():
                original_close()
                raise RuntimeError("fixture-private-close")
            result.close=close
        return result
    audible.Client=client
    def forbidden(*args,**kwargs):raise AssertionError("provider HTTP forbidden")
    httpx.HTTPTransport.handle_request=forbidden
    if MODE == "storage":
        def failed_replace(*args,**kwargs):raise OSError("fixture-private-replace")
        package.auth_store.os.replace=failed_replace
    if MODE == "uncertain":
        original_fsync=os.fsync
        def fsync(fd):
            if stat.S_ISDIR(os.fstat(fd).st_mode):raise OSError("fixture-private-dir-fsync")
            original_fsync(fd)
        os.fsync=fsync
    return package
main(SOURCES,fixture_load,null_stdio)
`
const initialLibraryAuth = `{"locale_code":"us","access_token":"Atna|synthetic-not-issued","refresh_token":"Atnr|synthetic-not-issued","expires":1}`

func librarySDK(t *testing.T) (*Client, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(initialLibraryAuth), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := Open(context.Background(), root, pythonFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(client.librarySource, libraryEntry) {
		t.Fatal("fixture no longer consumes actual embedded launcher")
	}
	source := strings.TrimSuffix(client.librarySource, libraryEntry)
	t.Cleanup(func() { client.Close(context.Background()) })
	return client, root, source
}
func setSDKMode(client *Client, source, mode, barrier string) {
	client.librarySource = "MODE=" + strconv.Quote(mode) + "\nBARRIER=" + strconv.Quote(barrier) + "\n" + source + librarySDKFixture
}

func TestActualLibrarySDKSnapshotAndFailureRecovery(t *testing.T) {
	for _, mode := range []string{"success", "empty", "denied", "repeat", "malformed", "cancel", "storage", "uncertain", "close", "unsupported", "missing"} {
		t.Run(mode, func(t *testing.T) {
			client, root, source := librarySDK(t)
			setSDKMode(client, source, mode, "")
			if mode == "missing" {
				if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var events []model.OperationEvent
			result, err := client.Library(ctx, "sdk-library", func(e model.OperationEvent) { events = append(events, e) })
			expected := map[string]string{"denied": "throttled", "repeat": "protocol_error", "malformed": "protocol_error", "cancel": "canceled", "storage": "storage_failed", "uncertain": "storage_uncertain", "close": "internal", "unsupported": "unsupported", "missing": "not_authenticated"}[mode]
			if expected == "" {
				count := 101
				if mode == "empty" {
					count = 0
				}
				if err != nil || result.Books == nil || len(result.Books) != count || events[len(events)-1].Type != model.Completed {
					t.Fatal("complete snapshot lost", err)
				}
				if count > 0 {
					book := result.Books[0]
					if book.Authors != nil || book.Narrators == nil || book.Subtitle == nil || book.Duration == nil || *book.Duration != 0 {
						t.Fatal("optional conversion")
					}
				}
			} else if err == nil || failureCode(t, err) != expected || result.Books != nil || strings.Contains(err.Error(), "fixture-private") {
				t.Fatal("unsafe/partial SDK result", err)
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), "Atna|") || strings.Contains(string(encoded), "fixture-private") {
				t.Fatal("event leaked SDK data")
			}
			if mode == "missing" {
				return
			}
			saved, readErr := os.ReadFile(filepath.Join(root, "auth.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var auth struct {
				Access string `json:"access_token"`
			}
			if json.Unmarshal(saved, &auth) != nil {
				t.Fatal("bad persisted auth")
			}
			token := "Atna|synthetic-refreshed-not-issued"
			if mode == "storage" || mode == "unsupported" {
				token = "Atna|synthetic-not-issued"
			}
			if auth.Access != token {
				t.Fatal("persistence boundary")
			}
			if token == "Atna|synthetic-refreshed-not-issued" {
				setSDKMode(client, source, "recover", "")
				if recovered, err := client.Library(ctx, "sdk-library", nil); err != nil || recovered.Books == nil || len(recovered.Books) != 0 {
					t.Fatal("refreshed recovery failed", err)
				}
			}
		})
	}
}

func TestLibrarySDKGateQueueAndBlockedCallbackCancellation(t *testing.T) {
	for _, cancelActive := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelActive), func(t *testing.T) {
			client, root, source := librarySDK(t)
			socketPath := filepath.Join(t.TempDir(), "barrier")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(8 * time.Second))
			setSDKMode(client, source, "blocked", socketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			callbackEntered, releaseCallback := make(chan struct{}), make(chan struct{})
			var release sync.Once
			done := make(chan struct{})
			var result model.LibrarySnapshot
			var activeErr error
			go func() {
				defer close(done)
				result, activeErr = client.Library(ctx, "active", func(e model.OperationEvent) {
					if e.Type == model.Stage && cancelActive {
						close(callbackEntered)
						<-releaseCallback
					}
				})
			}()
			defer func() { cancel(); release.Do(func() { close(releaseCallback) }); <-done }()
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(8 * time.Second))
			line, err := bufio.NewReader(connection).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid <= 1 {
				t.Fatal("bad child barrier")
			}
			queued, stop := context.WithCancel(context.Background())
			started := make(chan struct{})
			waiting := make(chan error, 1)
			go func() {
				_, err := client.Library(queued, "queued", func(e model.OperationEvent) {
					if e.Type == model.Started {
						close(started)
					}
				})
				waiting <- err
			}()
			<-started
			stop()
			if err := <-waiting; !errors.Is(err, context.Canceled) {
				t.Fatal("queue cancellation", err)
			}
			marker, err := os.ReadFile(filepath.Join(root, "fixture-start"))
			if err != nil || string(marker) != "S" {
				t.Fatal("queued operation started child", err)
			}
			if _, err := client.Library(context.Background(), "active", nil); failureCode(t, err) != "busy" {
				t.Fatal("duplicate entered")
			}
			stopped, stopClose := context.WithCancel(context.Background())
			stopClose()
			if err := client.Close(stopped); !errors.Is(err, context.Canceled) {
				t.Fatal("close stole active root", err)
			}
			if cancelActive {
				<-callbackEntered
				pidfd, err := unix.PidfdOpen(pid, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Close(pidfd)
				cancel()
				pollDeadline := time.Now().Add(5 * time.Second)
				var polled int
				for {
					remaining := time.Until(pollDeadline).Milliseconds()
					if remaining <= 0 {
						t.Fatal("owned child exit deadline")
					}
					polled, err = unix.Poll([]unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}, int(remaining))
					if !errors.Is(err, unix.EINTR) {
						break
					}
				}
				if err != nil || polled != 1 {
					t.Fatal("owned child did not exit while caller callback blocked", err)
				}
				select {
				case <-done:
					t.Fatal("Library returned through caller-owned callback")
				default:
				}
				release.Do(func() { close(releaseCallback) })
			} else {
				if _, err := connection.Write([]byte("G")); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			if result.Books != nil {
				t.Fatal("partial snapshot escaped")
			}
			if cancelActive {
				if !errors.Is(activeErr, context.Canceled) {
					t.Fatal(activeErr)
				}
				saved, err := os.ReadFile(filepath.Join(root, "auth.json"))
				if err != nil || string(saved) != initialLibraryAuth {
					t.Fatal("controlled pre-persistence kill changed auth", err)
				}
			} else {
				if failureCode(t, activeErr) != "throttled" {
					t.Fatal(activeErr)
				}
				setSDKMode(client, source, "recover", "")
				if _, err := client.Library(context.Background(), "active", nil); err != nil {
					t.Fatal("failure left gate/refresh/ID busy", err)
				}
			}
			if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
				t.Fatal("child not reaped", err)
			}
		})
	}
}
