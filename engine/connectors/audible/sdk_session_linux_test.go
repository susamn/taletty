package audible

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
)

// A fixture consumer, NOT a public provider action. Exercise the actual bundled
// SDK lifecycle and HTTP auth flow; #59 supplies production library semantics.
const sdkOperationFixture = `
import audible, audible.auth, httpx, socket
package = load_sources(SOURCES)
request = package.doctor.read_request()
package.doctor.require_action(request, "fixture")
control = os.fdopen(os.dup(1), "w")
null_stdio((0, 1, 2))
real_client = audible.Client
def refresh(**kwargs):
    assert request["mode"] != "recover", "recovery must load refreshed snapshot"
    return dict(access_token="Atna|synthetic-refreshed-not-issued", expires=4102444800)
audible.auth.refresh_access_token = refresh
def response(req):
    assert req.headers["x-amz-access-token"] == "Atna|synthetic-refreshed-not-issued"
    assert not os.get_inheritable(3)
    if request["mode"] == "blocked":
        with socket.socket(socket.AF_UNIX) as barrier:
            barrier.settimeout(5)
            barrier.connect(request["barrier"])
            barrier.sendall(b"R")
            assert barrier.recv(1) == b"G"
        return httpx.Response(429, json={"message":"fixture-private-provider-body"})
    return httpx.Response(200, json={"fixture":True})
audible.Client = lambda **kwargs: real_client(**kwargs, transport=httpx.MockTransport(response))
def forbidden(*args, **kwargs):
    raise AssertionError("provider network forbidden")
httpx.HTTPTransport.handle_request = forbidden
frame = dict(protocol=1, operation_id=request["operation_id"], seq=1)
try:
    with package.sdk_session.authenticated(3) as client:
        client.get("library")
    frame.update(type="result", fixture="ready")
except package.sdk_session.ProviderError as error:
    frame.update(type="error", failure=dict(code=error.code, retry=error.retry))
control.write(json.dumps(frame)+"\n")
control.close()
`

func TestSDKPersistenceUnderOwnedGateAndCancellation(t *testing.T) {
	if _, err := bridge.RunRoot(context.Background(), "unused", nil, nil, "", nil, nil); !errors.Is(err, bridge.ErrProtocol) {
		t.Fatal(err)
	}
	for _, cancelActive := range []bool{false, true} {
		name := "provider-failure"
		if cancelActive {
			name = "active-cancel"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			initial := []byte(`{"locale_code":"us","access_token":"Atna|synthetic-not-issued","refresh_token":"Atnr|synthetic-not-issued","expires":1}`)
			if err := os.WriteFile(filepath.Join(root, "auth.json"), initial, 0600); err != nil {
				t.Fatal(err)
			}
			client, err := Open(ctx, root, pythonFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			socketPath := filepath.Join(t.TempDir(), "barrier")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			source := strings.TrimSuffix(client.script, "\nmain(SOURCES)\n") + sdkOperationFixture
			run := func(ctx context.Context, mode string) (frame bridge.Frame, err error) {
				err = client.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
					request, _ := json.Marshal(map[string]any{"protocol": 1, "operation_id": "sdk-test", "action": "fixture", "mode": mode, "barrier": socketPath})
					frame, err = bridge.RunRoot(ctx, client.python, []string{"-I", "-B", "-c", source}, request, "sdk-test", root, nil)
					return err
				})
				return
			}
			active, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			var frame bridge.Frame
			var activeErr error
			go func() { defer close(done); frame, activeErr = run(active, "blocked") }()
			defer func() { stop(); <-done; client.Close(context.Background()) }()
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal("no provider barrier", err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var ready [1]byte
			if _, err := io.ReadFull(connection, ready[:]); err != nil || ready[0] != 'R' {
				t.Fatal("invalid provider barrier", err)
			}
			// The first operation owns the gate. Canceling a queued operation must never
			// enter its callback/start another process or overwrite refreshed state.
			queued, cancelQueued := context.WithCancel(ctx)
			entered := atomic.Bool{}
			waiting := make(chan error, 1)
			go func() {
				waiting <- client.session.WithRoot(queued, func(context.Context, *os.File) error { entered.Store(true); return nil })
			}()
			cancelQueued()
			if err := <-waiting; !errors.Is(err, context.Canceled) || entered.Load() {
				t.Fatal("canceled queued operation entered", err)
			}
			if cancelActive {
				stop()
			} else {
				if _, err := connection.Write([]byte("G")); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			if cancelActive {
				if !errors.Is(activeErr, context.Canceled) {
					t.Fatal(activeErr)
				}
				data, err := os.ReadFile(filepath.Join(root, "auth.json"))
				if err != nil || string(data) != string(initial) {
					t.Fatal("pre-persistence kill changed snapshot", err)
				}
			} else {
				if activeErr != nil || frame.Type != bridge.Failure || strings.Contains(string(frame.Raw), "fixture-private") {
					t.Fatal("unsafe provider outcome", activeErr)
				}
				var payload struct{ Failure struct{ Code, Retry string } }
				if json.Unmarshal(frame.Raw, &payload) != nil || payload.Failure.Code != "throttled" || payload.Failure.Retry != "wait" {
					t.Fatal("provider failure lost")
				}
				recovered, err := run(ctx, "recover")
				if err != nil || recovered.Type != bridge.Result {
					t.Fatal("refreshed snapshot not reusable", err)
				}
			}
		})
	}
}
