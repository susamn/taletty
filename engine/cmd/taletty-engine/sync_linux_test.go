package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
	"golang.org/x/sys/unix"
)

// Trusted explicit test executable consumes the actual embedded -c source, injecting
// only transport/refresh/filesystem faults. No production env-based test hooks.
const syncSDKInjection = `
import audible, audible.auth, httpx, socket, stat
original_load,original_client=load_sources,audible.Client
mode=os.environ.get("TALETTY_SYNC_MODE","success")
def fixture_load(sources,names):
    package=original_load(sources,names)
    def refresh(**kwargs):
        assert mode != "recover", "persisted refresh missing"
        return dict(access_token="Atna|fixture-refreshed-not-issued",expires=4102444800)
    audible.auth.refresh_access_token=refresh
    def item(i):return dict(asin=f"T{i:09d}",title="Fixture",authors=[],narrators=[],runtime_length_min=0)
    def response(request):
        assert request.headers["x-amz-access-token"]=="Atna|fixture-refreshed-not-issued"
        if mode in ("empty","recover"):return httpx.Response(200,json={"items":[]})
        if request.url.params["page"]=="1":return httpx.Response(200,json={"items":[item(i) for i in range(100)]})
        if mode=="blocked":
            with socket.socket(socket.AF_UNIX) as barrier:
                barrier.settimeout(8);barrier.connect(os.environ["TALETTY_CLI_BARRIER"])
                barrier.sendall((str(os.getpid())+"\n").encode());assert barrier.recv(1)==b"G"
        return httpx.Response(429 if mode=="throttled" else 200,json={"items":[item(0),item(100)],"message":"fixture-private-provider"})
    def client(**kwargs):
        os.write(1,b"fixture-private-chatter");os.write(2,b"fixture-private-chatter")
        return original_client(**kwargs,transport=httpx.MockTransport(response))
    audible.Client=client
    def forbidden(*args,**kwargs):raise AssertionError("provider HTTP forbidden")
    httpx.HTTPTransport.handle_request=forbidden
    if mode=="storage":
        def replace(*args,**kwargs):raise OSError("fixture-private-replace")
        os.replace=replace
    if mode=="uncertain":
        original_fsync=os.fsync
        def fsync(fd):
            if stat.S_ISDIR(os.fstat(fd).st_mode):raise OSError("fixture-private-fsync")
            original_fsync(fd)
        os.fsync=fsync
    return package
main(SOURCES,fixture_load,null_stdio)
`
const syncInitialAuth = `{"locale_code":"us","access_token":"Atna|fixture-not-issued","refresh_token":"Atnr|fixture-not-issued","expires":1}`

func syncRuntime(t *testing.T) string {
	t.Helper()
	python := testPython(t) // Same explicit override/prepared PATH convention as doctor fixtures.
	wrapper := filepath.Join(t.TempDir(), "runtime")
	source := "#!" + python + " -I\nimport sys\nsys.dont_write_bytecode=True\ncode=sys.argv[-1]\nentry='main(SOURCES,load_sources,null_stdio)'\nassert code.count(entry)==1\nexec(compile(code.replace(entry," + strconv.Quote(syncSDKInjection) + "),'<actual-embedded-library>','exec'))\n"
	if err := os.WriteFile(wrapper, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}
func syncHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".config", "taletty")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(syncInitialAuth), 0600); err != nil {
		t.Fatal(err)
	}
	return home, root
}
func rootReleased(t *testing.T, root string) {
	t.Helper()
	client, err := audible.Open(context.Background(), root, "unused-runtime")
	if err != nil {
		t.Fatal("command retained root", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestEntrypointSyncActualSDKAndRestart(t *testing.T) {
	runtime := syncRuntime(t)
	for _, mode := range []string{"success", "empty", "throttled", "storage", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			home, root := syncHome(t)
			cmd := command(t, home, "sync", "--python", runtime, "--json")
			cmd.Env = append(cmd.Env, "TALETTY_SYNC_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var payload struct {
				Snapshot *model.LibrarySnapshot
				Failure  *model.Failure
			}
			if json.Unmarshal(output, &payload) != nil || strings.Contains(string(output), "fixture-private") || strings.Contains(string(output), "Atna|") {
				t.Fatal("unsafe command output", string(output))
			}
			success := mode == "success" || mode == "empty"
			if success {
				count := 101
				if mode == "empty" {
					count = 0
				}
				if exitCode(err) != 0 || payload.Failure != nil || payload.Snapshot == nil || payload.Snapshot.Books == nil || len(payload.Snapshot.Books) != count {
					t.Fatal(exitCode(err), string(output))
				}
			} else {
				expected := map[string]string{"throttled": "throttled", "storage": "storage_failed", "uncertain": "storage_uncertain"}[mode]
				if exitCode(err) != 1 || payload.Snapshot != nil || payload.Failure == nil || payload.Failure.Code != expected {
					t.Fatal(exitCode(err), string(output))
				}
			}
			rootReleased(t, root)
			saved, err := os.ReadFile(filepath.Join(root, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "storage" {
				if string(saved) != syncInitialAuth {
					t.Fatal("precommit changed auth")
				}
				return
			}
			if !bytes.Contains(saved, []byte("Atna|fixture-refreshed-not-issued")) {
				t.Fatal("refresh not persisted")
			}
			restart := command(t, home, "sync", "--python", runtime)
			restart.Env = append(restart.Env, "TALETTY_SYNC_MODE=recover")
			if output, err := restart.CombinedOutput(); exitCode(err) != 0 || string(output) != "Library synced: 0 books.\n" {
				t.Fatal(exitCode(err), string(output))
			}
			rootReleased(t, root)
		})
	}
}
func TestEntrypointSyncSignalsJoinSDKAndReleaseRoot(t *testing.T) {
	runtime := syncRuntime(t)
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			home, root := syncHome(t)
			socketPath := filepath.Join(t.TempDir(), "barrier")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(8 * time.Second))
			cmd := command(t, home, "sync", "--python", runtime, "--json")
			cmd.Env = append(cmd.Env, "TALETTY_SYNC_MODE=blocked", "TALETTY_CLI_BARRIER="+socketPath)
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
				t.Fatal(err)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(connection).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid <= 1 {
				t.Fatal("invalid PID barrier")
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if exitCode(err) != 130 || !strings.Contains(output.String(), `"code":"canceled"`) || strings.Contains(output.String(), `"snapshot"`) {
				t.Fatal(exitCode(err), output.String())
			}
			if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
				t.Fatal("SDK child not reaped", err)
			}
			saved, err := os.ReadFile(filepath.Join(root, "auth.json"))
			if err != nil || string(saved) != syncInitialAuth {
				t.Fatal("controlled precommit kill changed auth", err)
			}
			rootReleased(t, root)
		})
	}
}
func TestEntrypointSyncFreshUnsafeBusyAndBrokenOutput(t *testing.T) {
	runtime := syncRuntime(t)
	home := t.TempDir()
	root := filepath.Join(home, ".config", "taletty")
	cmd := command(t, home, "sync", "--python", runtime, "--json")
	if output, err := cmd.CombinedOutput(); exitCode(err) != 1 || !bytes.Contains(output, []byte(`"code":"not_authenticated"`)) {
		t.Fatal(exitCode(err), string(output))
	}
	owner, err := audible.Open(context.Background(), root, runtime)
	if err != nil {
		t.Fatal(err)
	}
	output, runErr := command(t, home, "sync", "--python", runtime, "--json").CombinedOutput()
	owner.Close(context.Background())
	if exitCode(runErr) != 1 || !bytes.Contains(output, []byte(`"code":"busy"`)) {
		t.Fatal(exitCode(runErr), string(output))
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	output, err = command(t, home, "sync", "--python", runtime, "--json").CombinedOutput()
	info, statErr := os.Stat(root)
	if exitCode(err) != 1 || !bytes.Contains(output, []byte(`"code":"storage_failed"`)) || statErr != nil || info.Mode().Perm() != 0755 {
		t.Fatal("unsafe root repaired", err)
	}
	home, root = syncHome(t)
	cmd = command(t, home, "sync", "--python", runtime, "--json")
	cmd.Env = append(cmd.Env, "TALETTY_SYNC_MODE=empty")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	defer writer.Close()
	var diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = writer, &diagnostics
	if err := cmd.Run(); exitCode(err) != 1 || !strings.Contains(diagnostics.String(), "Output could not be written") {
		t.Fatal(exitCode(err), diagnostics.String())
	}
	rootReleased(t, root)
}
