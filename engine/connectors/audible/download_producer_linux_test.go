package audible

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"golang.org/x/sys/unix"
)

// Trusted test transports only. Production fileless entrypoint, real SDK and
// store are retained. Deterministic copy/probe fixtures are NOT real-media proof;
// the separate generated test keeps the actual ffmpeg/ffprobe path unmodified.
const downloadProducerFixture = `
import audible, audible.auth, base64, httpx, socket, subprocess
original_load, original_sdk, original_http = load_sources, audible.Client, httpx.Client
def fixture_load(sources,names):
    package=original_load(sources,names)
    def barrier():
        with socket.socket(socket.AF_UNIX) as connection:
            connection.settimeout(8);connection.connect(BARRIER)
            connection.sendall((str(os.getpid())+"\n").encode())
            assert connection.recv(1)==b"G"
    def license_response(request):
        if MODE=="precommit":barrier()
        if MODE=="throttle":return httpx.Response(429,json={"message":"fixture-private"})
        return httpx.Response(200,json={"content_license":dict(status_code="Denied" if MODE=="denied" else "Granted",
            asin="B000000001",content_metadata=dict(content_url=dict(offline_url="https://media.example.invalid/fixture-private"),
            chapter_info=dict(chapters=[dict(title="Fixture",start_offset_ms=0,length_ms=2000 if MODE=="real" else 100)])))})
    def sdk(**kwargs):
        marker=os.open("fixture-client",os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o600,dir_fd=3)
        os.write(marker,b"S");os.close(marker)
        os.write(1,b"fixture-private");os.write(2,b"fixture-private")
        assert os.read(0,1)==b""
        check="import os\nfor fd in (3,4):\n try: os.fstat(fd)\n except OSError: pass\n else: raise SystemExit(2)"
        subprocess.run([sys.executable,"-I","-B","-c",check],close_fds=False,check=True)
        return original_sdk(**kwargs,transport=httpx.MockTransport(license_response))
    audible.Client=sdk
    audible.auth.refresh_access_token=lambda **kwargs:dict(access_token="Atna|synthetic-refreshed-not-issued",expires=4102444800)
    def media_response(request):
        assert not any(h.startswith("x-amz") for h in request.headers)
        assert "authorization" not in request.headers and "cookie" not in request.headers
        return httpx.Response(200,content=base64.b64decode(CLEAR_SOURCE) if MODE=="real" else b"synthetic-media")
    def http(**kwargs):
        if "transport" not in kwargs:kwargs["transport"]=httpx.MockTransport(media_response)
        return original_http(**kwargs)
    httpx.Client=http
    def forbidden(*args,**kwargs):raise AssertionError("external HTTP forbidden")
    httpx.HTTPTransport.handle_request=forbidden
    if MODE!="real":
        def copy(source,output,store,info=None,plan=None,cancel_fd=None):
            output.write(os.pread(source.fd,1024,0))
            return package.chapters.prepare(info)[0]
        package.remux.copy=copy
        def probe(argv,fds,*args):
            if os.pread(fds[0],1024,0)!=b"synthetic-media":return b"{}"
            return json.dumps(dict(format=dict(format_name="mov,mp4",duration="1"),
                streams=[dict(codec_type="audio",codec_name="aac",nb_read_packets="1")],
                chapters=[dict(start_time="0",end_time="0.1",tags=dict(title="Fixture"))])).encode()
        package.remux._run=probe
    if MODE in ("postcommit","auth_failed","truncated"):
        original_save=package.auth_store.save
        def save(fd,payload):
            if MODE=="truncated":os._exit(0)
            if MODE=="postcommit":barrier()
            else:raise package.auth_store.StoreError()
            original_save(fd,payload)
        package.auth_store.save=save
    return package
main(SOURCES,fixture_load,null_stdio,action=ACTION)
`

func producerSource(reconcile bool, mode, barrier string, clear []byte) string {
	action := "download"
	if reconcile {
		action = "reconcile"
	}
	source := downloadScript(reconcile)
	entry := "\nmain(SOURCES,load_sources,null_stdio,action=" + strconv.Quote(action) + ")\n"
	if !strings.HasSuffix(source, entry) {
		panic("fixture no longer replaces production entrypoint")
	}
	return "MODE=" + strconv.Quote(mode) + "\nBARRIER=" + strconv.Quote(barrier) + "\nCLEAR_SOURCE=" + strconv.Quote(base64.StdEncoding.EncodeToString(clear)) + "\nACTION=" + strconv.Quote(action) + "\n" + strings.TrimSuffix(source, entry) + downloadProducerFixture
}

func producerRun(ctx context.Context, client *Client, root *os.File, source, action string) (bridge.Frame, error) {
	request := []byte(`{"protocol":1,"operation_id":"download-fixture","action":"` + action + `","book_id":{"provider":"audible","namespace":"us","id":"B000000001"}}`)
	return bridge.RunRoot(ctx, client.python, []string{"-I", "-B", "-c", source}, request, "download-fixture", root, nil)
}

func producerPayload(t *testing.T, frame bridge.Frame, err error) map[string]json.RawMessage {
	t.Helper()
	if err != nil || frame.Type != bridge.Result || strings.Contains(string(frame.Raw), "fixture-private") || strings.Contains(string(frame.Raw), "Atna|") {
		t.Fatal("unsafe/incomplete producer result", err)
	}
	fields, err := strictFields(frame.Raw)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestDownloadProducerSDKRefusalPersistenceAndReadyReuse(t *testing.T) {
	for _, mode := range []string{"success", "denied", "throttle", "auth_failed"} {
		t.Run(mode, func(t *testing.T) {
			client, path, _ := librarySDK(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := client.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
				frame, err := producerRun(ctx, client, root, producerSource(false, mode, "", nil), "download")
				fields := producerPayload(t, frame, err)
				var outcome struct {
					compositionOutcome
					Chapters []json.RawMessage
				}
				if json.Unmarshal(fields["outcome"], &outcome) != nil {
					t.Fatal("bad outcome")
				}
				ready := mode == "success" || mode == "auth_failed"
				if outcome.Ready != ready || outcome.Published == nil || *outcome.Published != ready || outcome.Reused || (len(outcome.Chapters) == 1) != ready {
					t.Fatal("media facts lost", outcome)
				}
				if mode == "denied" || mode == "throttle" {
					want := map[string]string{"denied": "entitlement_denied", "throttle": "throttled"}[mode]
					if outcome.MediaCode == nil || *outcome.MediaCode != want {
						t.Fatal("refusal changed", outcome)
					}
				}
				if mode == "auth_failed" && (outcome.AuthCode == nil || *outcome.AuthCode != "storage_failed") {
					t.Fatal("auth failure erased", outcome)
				}
				if ready {
					if err := os.Remove(filepath.Join(path, "auth.json")); err != nil {
						t.Fatal(err)
					}
					frame, err = producerRun(ctx, client, root, producerSource(false, mode, "", nil), "download")
					fields = producerPayload(t, frame, err)
					var reused map[string]any
					json.Unmarshal(fields["outcome"], &reused)
					if reused["ready"] != true || reused["reused"] != true || reused["published"] != false || reused["chapters"] != nil {
						t.Fatal("reuse invented hierarchy/auth", reused)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(path, "auth.json"))
			if mode == "denied" || mode == "throttle" {
				if err != nil || !strings.Contains(string(saved), "synthetic-refreshed-not-issued") {
					t.Fatal("ordinary refusal lost refresh", err)
				}
			}
			marker, err := os.ReadFile(filepath.Join(path, "fixture-client"))
			if err != nil || string(marker) != "S" {
				t.Fatal("reuse restarted SDK", err)
			}
		})
	}
}

func TestDownloadProducerJoinedCancelThenAnchoredProviderFreeRecovery(t *testing.T) {
	for _, mode := range []string{"precommit", "postcommit"} {
		t.Run(mode, func(t *testing.T) {
			client, path, _ := librarySDK(t)
			barrier := filepath.Join(t.TempDir(), "barrier")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: barrier, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(8 * time.Second))
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			done := make(chan error, 1)
			go func() {
				done <- client.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
					_, runErr := producerRun(ctx, client, root, producerSource(false, mode, barrier, nil), "download")
					// RunRoot is joined. Keep this SAME root/gate; do not recurse or
					// reuse the canceled context. Whole recovery child is bounded.
					recoverCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					frame, err := producerRun(recoverCtx, client, root, producerSource(true, mode, "", nil), "reconcile")
					if err != nil || frame.Type != bridge.Result {
						return errors.New("recovery child failed")
					}
					var result struct{ State string }
					json.Unmarshal(frame.Raw, &result)
					want := "absent"
					if mode == "postcommit" {
						want = "ready"
					}
					if result.State != want {
						return errors.New("recovery state incorrect")
					}
					return runErr
				})
			}()
			joined := false
			defer func() {
				stop()
				if !joined {
					<-done
				}
			}()
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
				t.Fatal("invalid barrier pid")
			}
			queued, cancelQueued := context.WithCancel(context.Background())
			cancelQueued()
			if err := client.session.WithRoot(queued, func(context.Context, *os.File) error { t.Error("queued canceled work entered"); return nil }); !errors.Is(err, context.Canceled) {
				t.Fatal("queued work did not cancel", err)
			}
			stop()
			err = <-done
			joined = true
			if !errors.Is(err, context.Canceled) || !errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
				t.Fatal("cancel did not join/reap before recovery", err)
			}
			marker, err := os.ReadFile(filepath.Join(path, "fixture-client"))
			if err != nil || string(marker) != "S" {
				t.Fatal("reconciliation relicensed", err)
			}
		})
	}
}

func TestDownloadProducerSeededRecoveryWithoutAuth(t *testing.T) {
	for _, state := range []string{"ready", "invalid", "unsafe", "absent"} {
		t.Run(state, func(t *testing.T) {
			client, path, _ := librarySDK(t)
			if err := os.Remove(filepath.Join(path, "auth.json")); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(path, "downloads")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if state != "absent" {
				payload, mode := []byte("synthetic-media"), os.FileMode(0600)
				if state == "invalid" {
					payload = []byte("not-media")
				}
				if state == "unsafe" {
					mode = 0644
				}
				if err := os.WriteFile(filepath.Join(directory, "B000000001.m4b"), payload, mode); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
				frame, err := producerRun(ctx, client, root, producerSource(true, "fixture", "", nil), "reconcile")
				fields := producerPayload(t, frame, err)
				if string(fields["state"]) != strconv.Quote(state) {
					t.Fatal("seeded artifact misclassified", string(fields["state"]))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(path, "fixture-client")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("seeded recovery accessed SDK", err)
			}
		})
	}
}

func TestDownloadProducerNonzeroAndTruncatedChildRetainArtifact(t *testing.T) {
	for _, mode := range []string{"nonzero", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			client, _, _ := librarySDK(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
				source := producerSource(false, mode, "", nil)
				if mode == "nonzero" {
					source += "\nraise SystemExit(7)\n"
				}
				if _, err := producerRun(ctx, client, root, source, "download"); err == nil {
					t.Fatal("incomplete/nonzero exit trusted as success")
				}
				recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				frame, err := producerRun(recoveryCtx, client, root, producerSource(true, mode, "", nil), "reconcile")
				if fields := producerPayload(t, frame, err); string(fields["state"]) != `"ready"` {
					t.Fatal("joined failure lost committed artifact")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDownloadProducerActualGeneratedMedia(t *testing.T) {
	clear := compositionClearSource(t) // Only this test requires real media tools.
	client, _, _ := librarySDK(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := client.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
		frame, err := producerRun(ctx, client, root, producerSource(false, "real", "", clear), "download")
		fields := producerPayload(t, frame, err)
		var outcome compositionOutcome
		if json.Unmarshal(fields["outcome"], &outcome) != nil || !outcome.Ready || outcome.MediaCode != nil || outcome.AuthCode != nil {
			t.Fatal("actual media production failed", outcome)
		}
		frame, err = producerRun(ctx, client, root, producerSource(true, "real", "", nil), "reconcile")
		fields = producerPayload(t, frame, err)
		if string(fields["state"]) != `"ready"` {
			t.Fatal("production probe did not reconcile generated media")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
