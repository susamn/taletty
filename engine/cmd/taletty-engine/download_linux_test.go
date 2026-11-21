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

const downloadSDKInjection = `
import audible, audible.auth, httpx, json, os, socket, subprocess, sys
original_load, original_sdk, original_http = load_sources, audible.Client, httpx.Client
mode = os.environ.get("TALETTY_DOWNLOAD_MODE", "success")
def fixture_load(sources, names):
    package = original_load(sources, names)
    def barrier():
        with socket.socket(socket.AF_UNIX) as connection:
            connection.settimeout(8)
            connection.connect(os.environ["TALETTY_CLI_BARRIER"])
            connection.sendall((str(os.getpid()) + "\n").encode())
            assert connection.recv(1) == b"G"
    def license_response(request):
        if mode == "precommit":
            barrier()
        if mode == "throttled":
            return httpx.Response(429, json={"message": "fixture-private"})
        return httpx.Response(200, json={"content_license": dict(
            status_code="Denied" if mode == "denied" else "Granted",
            asin="B000000001",
            content_metadata=dict(
                content_url=dict(offline_url="https://media.example.invalid/fixture-private"),
                chapter_info=dict(chapters=[dict(title="Chapter 1", start_offset_ms=0, length_ms=1000)])
            )
        )})
    def sdk(**kwargs):
        os.write(1, b"fixture-private")
        os.write(2, b"fixture-private")
        return original_sdk(**kwargs, transport=httpx.MockTransport(license_response))
    audible.Client = sdk
    audible.auth.refresh_access_token = lambda **kwargs: dict(access_token="Atna|fixture-refreshed-not-issued", expires=4102444800)
    def media_response(request):
        assert not any(h.startswith("x-amz") for h in request.headers)
        assert "authorization" not in request.headers and "cookie" not in request.headers
        return httpx.Response(200, content=b"synthetic-media")
    def http(**kwargs):
        if "transport" not in kwargs:
            kwargs["transport"] = httpx.MockTransport(media_response)
        return original_http(**kwargs)
    httpx.Client = http
    def forbidden(*args, **kwargs):
        raise AssertionError("external HTTP forbidden")
    httpx.HTTPTransport.handle_request = forbidden
    def copy(source, output, store, info=None, plan=None, cancel_fd=None):
        output.write(os.pread(source.fd, 1024, 0))
        return package.chapters.prepare(info)[0]
    package.remux.copy = copy
    def probe(argv, fds, *args):
        if os.pread(fds[0], 1024, 0) != b"synthetic-media":
            return b"{}"
        return json.dumps(dict(
            format=dict(format_name="mov,mp4", duration="1"),
            streams=[dict(codec_type="audio", codec_name="aac", nb_read_packets="1")],
            chapters=[dict(start_time="0", end_time="1", tags=dict(title="Chapter 1"))]
        )).encode()
    package.remux._run = probe
    if mode in ("postcommit", "auth_failed"):
        original_save = package.auth_store.save
        def save(fd, payload):
            if mode == "postcommit":
                barrier()
            else:
                raise package.auth_store.StoreError()
            return original_save(fd, payload)
        package.auth_store.save = save
    if mode == "media_uncertain":
        original_rename = package.media_store._rename
        def rename(directory, source, target):
            original_rename(directory, source, target)
            raise OSError("fixture-private-postcommit")
        package.media_store._rename = rename
    return package
main(SOURCES, fixture_load, null_stdio, action=ACTION)
`

func downloadRuntime(t *testing.T) string {
	t.Helper()
	python := testPython(t)
	wrapper := filepath.Join(t.TempDir(), "download-runtime")
	source := "#!" + python + " -I\nimport sys\nsys.dont_write_bytecode=True\ncode=sys.argv[-1]\nfor act in ('download','reconcile'):\n entry='main(SOURCES,load_sources,null_stdio,action=\"'+act+'\")'\n if entry in code:\n  code='ACTION=\"'+act+'\"\\n'+code.replace(entry," + strconv.Quote(downloadSDKInjection) + ")\n  break\nelse:\n raise SystemExit(3)\nexec(compile(code,'<actual-embedded-download>','exec'))\n"
	if err := os.WriteFile(wrapper, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func TestEntrypointDownloadActualSDKAndReadyReuse(t *testing.T) {
	runtime := downloadRuntime(t)
	for _, mode := range []string{"success", "denied", "throttled", "auth_failed", "media_uncertain"} {
		t.Run(mode, func(t *testing.T) {
			home, root := syncHome(t)
			cmd := command(t, home, "download", "B000000001", "--python", runtime, "--json")
			cmd.Env = append(cmd.Env, "TALETTY_DOWNLOAD_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var payload struct {
				Download *model.DownloadResult `json:"download"`
				Failure  *model.Failure        `json:"failure"`
			}
			if json.Unmarshal(output, &payload) != nil || payload.Download == nil || strings.Contains(string(output), "fixture-private") || strings.Contains(string(output), "Atna|") {
				t.Fatal("unsafe or missing download output", string(output))
			}
			ready := mode == "success" || mode == "auth_failed" || mode == "media_uncertain"
			if (payload.Download.State == model.MediaReady) != ready || (payload.Download.Media != nil) != ready {
				t.Fatal("unexpected ready state", mode, string(output))
			}
			if mode == "success" {
				if exitCode(err) != 0 || payload.Failure != nil || len(payload.Download.Media.Chapters) != 1 {
					t.Fatal(exitCode(err), string(output))
				}
			} else {
				want := map[string]string{
					"denied":          "entitlement_denied",
					"throttled":       "throttled",
					"auth_failed":     "storage_failed",
					"media_uncertain": "storage_uncertain",
				}[mode]
				if exitCode(err) != 1 || payload.Failure == nil || payload.Failure.Code != want {
					t.Fatal(exitCode(err), string(output))
				}
			}
			rootReleased(t, root)
			if ready {
				target := filepath.Join(root, "downloads", "B000000001.m4b")
				if payload.Download.Media.Path != target {
					t.Fatal("wrong media path", payload.Download.Media.Path, target)
				}
				// Remove auth.json and verify CLI ready reuse succeeds without re-authenticating.
				if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
					t.Fatal(err)
				}
				reuse := command(t, home, "download", "B000000001", "--python", runtime)
				reuseOut, reuseErr := reuse.CombinedOutput()
				if exitCode(reuseErr) != 0 || string(reuseOut) != "Media ready (reused): "+target+"\n" {
					t.Fatal("ready reuse failed", exitCode(reuseErr), string(reuseOut))
				}
				rootReleased(t, root)
			}
		})
	}
}

func TestEntrypointDownloadSignalsPreAndPostCommit(t *testing.T) {
	runtime := downloadRuntime(t)
	for _, mode := range []string{"precommit", "postcommit"} {
		for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
			t.Run(mode+"/"+sig.String(), func(t *testing.T) {
				home, root := syncHome(t)
				socketPath := filepath.Join(t.TempDir(), "barrier")
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				listener.SetDeadline(time.Now().Add(8 * time.Second))
				cmd := command(t, home, "download", "B000000001", "--python", runtime, "--json")
				cmd.Env = append(cmd.Env, "TALETTY_DOWNLOAD_MODE="+mode, "TALETTY_CLI_BARRIER="+socketPath)
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
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				err = cmd.Wait()
				waited = true
				if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
					t.Fatal("download child not reaped", err)
				}
				var payload struct {
					Download *model.DownloadResult `json:"download"`
					Failure  *model.Failure        `json:"failure"`
				}
				if json.Unmarshal(output.Bytes(), &payload) != nil || payload.Download == nil {
					t.Fatal("invalid JSON output", output.String())
				}
				if mode == "precommit" {
					if exitCode(err) != 130 || payload.Failure == nil || payload.Failure.Code != "canceled" || payload.Download.State != model.MediaAbsent {
						t.Fatal("precommit signal outcome wrong", exitCode(err), output.String())
					}
				} else {
					if exitCode(err) != 0 || payload.Failure != nil || payload.Download.State != model.MediaReady || payload.Download.Media == nil {
						t.Fatal("postcommit signal lost ready media", exitCode(err), output.String())
					}
				}
				rootReleased(t, root)
			})
		}
	}
}

func TestEntrypointDownloadUsageFreshBusyAndBrokenOutput(t *testing.T) {
	runtime := downloadRuntime(t)
	home := t.TempDir()
	out, err := command(t, home, "download", "../secret1", "--python", runtime).CombinedOutput()
	if exitCode(err) != 2 || strings.Contains(string(out), "secret1") {
		t.Fatal(exitCode(err), string(out))
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("usage error created root state")
	}
	root := filepath.Join(home, ".config", "taletty")
	out, err = command(t, home, "download", "B000000001", "--python", runtime, "--json").CombinedOutput()
	if exitCode(err) != 1 || !bytes.Contains(out, []byte(`"code":"not_authenticated"`)) {
		t.Fatal(exitCode(err), string(out))
	}
	owner, err := audible.Open(context.Background(), root, runtime)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := command(t, home, "download", "B000000001", "--python", runtime, "--json").CombinedOutput()
	owner.Close(context.Background())
	if exitCode(runErr) != 1 || !bytes.Contains(out, []byte(`"code":"busy"`)) {
		t.Fatal(exitCode(runErr), string(out))
	}
	home, root = syncHome(t)
	cmd := command(t, home, "download", "B000000001", "--python", runtime, "--json")
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
