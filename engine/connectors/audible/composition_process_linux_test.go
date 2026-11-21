package audible

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"golang.org/x/sys/unix"
)

// A trusted test consumer of the private callable, NOT a public download action.
// All provider HTTP is replaced; actual SDK authentication/persistence, store,
// remux, validator and RunRoot are retained. No issued credentials/protected media.
const compositionProcessFixture = `
import audible, audible.auth, base64, dataclasses, httpx, socket, stat, subprocess
package=load_sources(SOURCES, ("doctor","auth_store","login","sdk_session","library","chapters","licensing","media_store","remux","composition"))
request=package.doctor.read_request()
package.doctor.require_action(request,"fixture")
control=os.fdopen(os.dup(1),"w")
os.set_inheritable(3,False)
os.set_inheritable(control.fileno(),False)
null_stdio((0,1,2))
real_sdk,real_http,real_popen=audible.Client,httpx.Client,subprocess.Popen
mode=request["mode"]
def marker(name):
    fd=os.open(name,os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o600,dir_fd=3)
    os.write(fd,b"S");os.close(fd)
def refresh(**kwargs):
    assert mode != "reuse", "ready media must not construct an SDK session"
    return dict(access_token="Atna|synthetic-refreshed-not-issued",expires=4102444800)
audible.auth.refresh_access_token=refresh

def license_response(req):
    assert req.headers["x-amz-access-token"]=="Atna|synthetic-refreshed-not-issued"
    marker("fixture-license")
    if mode=="blocked":
        with socket.socket(socket.AF_UNIX) as barrier:
            barrier.settimeout(8);barrier.connect(request["barrier"])
            barrier.sendall((str(os.getpid())+"\n").encode())
            assert barrier.recv(1)==b"G"
    if mode=="throttle":return httpx.Response(429,json={"message":"fixture-private-provider"})
    content=dict(status_code="Denied" if mode=="denied" else "Granted",asin="B000000001",
        content_metadata=dict(content_url=dict(offline_url="https://media.example.invalid/synthetic?fixture-private"),
            chapter_info=dict(chapters=[dict(start_offset_ms=0,length_ms=1000,title="First"),
                                      dict(start_offset_ms=1000,length_ms=1000,title="Second")])))
    return httpx.Response(200,json=dict(content_license=content))
def sdk(**kwargs):
    assert mode!="reuse", "verified ready reuse must not license"
    marker("fixture-client")
    return real_sdk(**kwargs,transport=httpx.MockTransport(license_response))
audible.Client=sdk

def signed_response(req):
    assert req.url.host=="media.example.invalid"
    assert not any(name.startswith("x-amz") for name in req.headers)
    assert "authorization" not in req.headers and "cookie" not in req.headers
    marker("fixture-transfer")
    return httpx.Response(200,content=base64.b64decode(CLEAR_SOURCE))
def http(**kwargs):
    if "transport" not in kwargs:
        assert kwargs["auth"] is None and kwargs["trust_env"] is False and kwargs["follow_redirects"] is False
        kwargs["transport"]=httpx.MockTransport(signed_response)
    return real_http(**kwargs)
httpx.Client=http
def forbidden(*args,**kwargs):raise AssertionError("external provider HTTP forbidden")
httpx.HTTPTransport.handle_request=forbidden

# Inspect a real further exec with EXACTLY the intended tool FD capability set,
# then launch the real media tool through the original Popen. No new group/session.
def popen(argv,**kwargs):
    intended=kwargs.get("pass_fds",())
    assert 3 not in intended and control.fileno() not in intended
    assert kwargs["close_fds"] is True
    assert "start_new_session" not in kwargs and "process_group" not in kwargs and "preexec_fn" not in kwargs
    for fd in intended:
        info=os.fstat(fd)
        assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode)==0o600
    identities=[(os.fstat(fd).st_dev,os.fstat(fd).st_ino) for fd in (3,control.fileno())]
    inspect="import os,json,stat\nassert os.getpgrp()=="+str(os.getpgrp())+"\nfor value in os.listdir('/proc/self/fd'):\n try: fd=int(value); info=os.fstat(fd)\n except OSError: continue\n if fd>2:\n  assert fd in "+repr(intended)+"\n  assert not stat.S_ISDIR(info.st_mode)\n  assert (info.st_dev,info.st_ino) not in "+repr(identities)+"\n"
    check=real_popen([sys.executable,"-I","-B","-c",inspect],pass_fds=intended,close_fds=True,
                     stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    check.communicate(timeout=5)
    assert check.returncode==0,"unexpected inherited descriptor/group"
    marker("fixture-tools")
    return real_popen(argv,**kwargs)
subprocess.Popen=popen
outcome=package.composition.run(3,"B000000001")
frame=dict(protocol=1,operation_id=request["operation_id"],seq=1,type="result",outcome=dataclasses.asdict(outcome))
control.write(json.dumps(frame)+"\n");control.close()
`

type compositionOutcome struct {
	Ready, Reused bool
	Published     *bool
	MediaCode     *string `json:"media_code"`
	AuthCode      *string `json:"auth_code"`
}

func compositionSource(t *testing.T, clear []byte) string {
	t.Helper()
	sources := map[string]string{}
	for _, name := range []string{"doctor", "auth_store", "login", "sdk_session", "library", "chapters", "licensing", "media_store", "remux", "composition"} {
		data, err := os.ReadFile(filepath.Join("internal", "bridge", "adapter", name+".py"))
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(data)
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	return "import json\nSOURCES=json.loads(" + strconv.Quote(string(encoded)) + ")\nCLEAR_SOURCE=" + strconv.Quote(base64.StdEncoding.EncodeToString(clear)) + "\n" + interactiveSource + compositionProcessFixture
}

func runComposition(ctx context.Context, client *Client, source, mode, barrier string) (frame bridge.Frame, err error) {
	err = client.session.WithRoot(ctx, func(work context.Context, root *os.File) error {
		request, _ := json.Marshal(map[string]any{"protocol": 1, "operation_id": "composition-test", "action": "fixture", "mode": mode, "barrier": barrier})
		frame, err = bridge.RunRoot(work, client.python, []string{"-I", "-B", "-c", source}, request, "composition-test", root, nil)
		return err
	})
	return
}

func decodeComposition(t *testing.T, frame bridge.Frame, err error) compositionOutcome {
	t.Helper()
	if err != nil || frame.Type != bridge.Result {
		t.Fatal("private composition did not complete", err)
	}
	if strings.Contains(string(frame.Raw), "fixture-private") || strings.Contains(string(frame.Raw), "Atna|") || strings.Contains(string(frame.Raw), "https:") {
		t.Fatal("private process exposed provider material")
	}
	var payload struct{ Outcome compositionOutcome }
	if err := json.Unmarshal(frame.Raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Outcome
}

func TestCompositionRunRootRefusalRecoveryAndQueuedCancellation(t *testing.T) {
	for _, mode := range []string{"denied", "throttle", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			client, root, _ := librarySDK(t)
			source := compositionSource(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if mode != "blocked" {
				frame, err := runComposition(ctx, client, source, mode, "")
				outcome := decodeComposition(t, frame, err)
				want := map[string]string{"denied": "entitlement_denied", "throttle": "throttled"}[mode]
				if outcome.Ready || outcome.Published == nil || *outcome.Published || outcome.MediaCode == nil || *outcome.MediaCode != want || outcome.AuthCode != nil {
					t.Fatal("refusal/auth facts lost", outcome)
				}
				data, err := os.ReadFile(filepath.Join(root, "auth.json"))
				if err != nil || !strings.Contains(string(data), "synthetic-refreshed-not-issued") {
					t.Fatal("ordinary refusal did not persist refresh", err)
				}
				frame, err = runComposition(ctx, client, source, "denied", "")
				decodeComposition(t, frame, err) // A new isolated operation reuses refreshed auth/gate.
				return
			}
			barrier := filepath.Join(t.TempDir(), "barrier")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: barrier, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(8 * time.Second))
			active, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { _, err := runComposition(active, client, source, mode, barrier); done <- err }()
			defer stop()
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal("composition never reached actual SDK barrier", err)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(8 * time.Second))
			line, err := bufio.NewReader(connection).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid <= 1 {
				t.Fatal("invalid process barrier")
			}
			queued, cancelQueued := context.WithCancel(ctx)
			entered := atomic.Bool{}
			waiting := make(chan error, 1)
			go func() {
				waiting <- client.session.WithRoot(queued, func(context.Context, *os.File) error { entered.Store(true); return nil })
			}()
			cancelQueued()
			if err := <-waiting; !errors.Is(err, context.Canceled) || entered.Load() {
				t.Fatal("queued canceled operation started", err)
			}
			stop()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("active operation did not cancel/join", err)
			}
			if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
				t.Fatal("RunRoot child not reaped", err)
			}
			if _, err := os.Stat(filepath.Join(root, "downloads", "B000000001.m4b")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("precommit cancel manufactured ready media", err)
			}
			data, err := os.ReadFile(filepath.Join(root, "auth.json"))
			if err != nil || string(data) != initialLibraryAuth {
				t.Fatal("controlled pre-persistence kill changed auth", err)
			}
			frame, err := runComposition(ctx, client, source, "denied", "")
			decodeComposition(t, frame, err) // Root/gate is usable only after all child users joined.
		})
	}
}

func TestCompositionRunRootGeneratedMediaAndReadyReuse(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatal("mandatory composition process media tool missing")
			}
			t.Skip("local media tools absent; mandatory real-media CI")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	generated := filepath.Join(t.TempDir(), "clear.m4b")
	command := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "2", "-c:a", "aac", "-f", "mp4", generated)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		t.Fatal("generated silence failed", err)
	}
	clear, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	client, root, _ := librarySDK(t)
	source := compositionSource(t, clear)
	frame, err := runComposition(ctx, client, source, "success", "")
	outcome := decodeComposition(t, frame, err)
	if !outcome.Ready || outcome.Reused || outcome.Published == nil || !*outcome.Published || outcome.MediaCode != nil || outcome.AuthCode != nil {
		t.Fatal("complete private outcome lost", outcome)
	}
	tools, err := os.ReadFile(filepath.Join(root, "fixture-tools"))
	if err != nil || len(tools) < 3 {
		t.Fatal("real further-exec FD/media checks did not execute", err)
	}
	before, err := os.ReadFile(filepath.Join(root, "downloads", "B000000001.m4b"))
	if err != nil || len(before) == 0 {
		t.Fatal("completed artifact absent", err)
	}
	frame, err = runComposition(ctx, client, source, "reuse", "")
	outcome = decodeComposition(t, frame, err)
	if !outcome.Ready || !outcome.Reused || outcome.MediaCode != nil || outcome.AuthCode != nil {
		t.Fatal("ready reuse failed", outcome)
	}
	for _, name := range []string{"fixture-client", "fixture-license", "fixture-transfer"} {
		marker, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(marker) != "S" {
			t.Fatal("ready reuse re-licensed/re-downloaded", name, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(root, "downloads", "B000000001.m4b"))
	if err != nil || string(after) != string(before) {
		t.Fatal("reuse mutated completed artifact", err)
	}
}
