// Package cli owns headless command policy, not provider/process implementations.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
)

const usage = "Usage: taletty-engine {doctor|login|sync|download <ASIN>} [--python /path/to/venv/bin/python] [--marketplace us]\nDoctor, sync and download accept --json; login requires an owned foreground terminal.\n"

type account interface {
	Login(context.Context, *os.File) error
	Library(context.Context, string, func(model.OperationEvent)) (model.LibrarySnapshot, error)
	Download(context.Context, string, model.BookID, func(model.OperationEvent)) (model.DownloadResult, error)
	Close(context.Context) error
}
type dependencies struct {
	resolvePython func(string) (string, error)
	doctor        func(context.Context, string) (audible.Diagnostics, error)
	open          func(context.Context, string, string) (account, error)
	terminal      func() (*os.File, error)
}

// Run is synchronous: return only after the connector has joined its child users
// and closed its session. Caller owns context/shutdown signals and output streams.
func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	return run(ctx, args, out, errOut, dependencies{
		resolvePython: audible.DiscoverPython,
		doctor:        audible.Doctor,
		open: func(ctx context.Context, root, python string) (account, error) {
			return audible.Open(ctx, root, python)
		},
		terminal: func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) },
	})
}

func run(ctx context.Context, args []string, out, errOut io.Writer, deps dependencies) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		if _, err := io.WriteString(out, usage); err != nil {
			return outputFailure(errOut)
		}
		return 0
	}
	if len(args) == 0 || (args[0] != "doctor" && args[0] != "login" && args[0] != "sync" && args[0] != "download") {
		io.WriteString(errOut, usage)
		return 2
	}
	command := args[0]
	flagArgs := args[1:]
	var asin string
	if command == "download" && len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		asin, flagArgs = flagArgs[0], flagArgs[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	python := flags.String("python", "", "Python executable override")
	marketplace := flags.String("marketplace", "us", "Supported marketplace")
	machine := flags.Bool("json", false, "Machine-readable diagnostics, library or download result")
	err := flags.Parse(flagArgs)
	if errors.Is(err, flag.ErrHelp) {
		if _, err := io.WriteString(out, usage); err != nil {
			return outputFailure(errOut)
		}
		return 0
	}
	pythonSet := false
	if err == nil {
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "python" {
				pythonSet = true
			}
		})
	}
	if err == nil && command == "download" && asin == "" && flags.NArg() == 1 {
		asin = flags.Arg(0)
	} else if err == nil && flags.NArg() != 0 {
		err = flag.ErrHelp
	}
	if err != nil || (pythonSet && !validExplicitPython(*python)) || *marketplace != "us" || (command == "login" && *machine) || (command == "download" && !validASIN(asin)) {
		io.WriteString(errOut, usage)
		return 2 // Never echo arguments or flag parser text.
	}
	if err := ctx.Err(); err != nil {
		return reportFailure(out, errOut, *machine, err)
	}
	resolvedPython, err := deps.resolvePython(*python)
	if err != nil {
		return reportFailure(out, errOut, *machine, err)
	}
	if command == "doctor" {
		result, err := deps.doctor(ctx, resolvedPython)
		if err != nil {
			return reportFailure(out, errOut, *machine, err)
		}
		if *machine {
			err = json.NewEncoder(out).Encode(struct {
				Ready       bool                `json:"ready"`
				Diagnostics audible.Diagnostics `json:"diagnostics"`
			}{result.Ready(), result})
		} else {
			_, err = fmt.Fprintf(out, "Python: %s\nAudible: %s\nffmpeg: %s\nffprobe: %s\n", result.Python, result.Audible, result.FFmpeg, result.FFprobe)
		}
		if err != nil {
			return outputFailure(errOut)
		}
		if !result.Ready() {
			return 1
		}
		return 0
	}
	var terminal *os.File
	if command == "login" {
		var err error
		terminal, err = deps.terminal()
		if err != nil {
			return reportFailure(out, errOut, false, audible.ErrTerminal)
		}
		defer terminal.Close()
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return reportFailure(out, errOut, *machine, audible.ErrAuthStorage)
	}
	// Only this Taletty root; no XDG/legacy discovery or chmod repair. Open verifies
	// existing root/lock ownership and exact private modes before any provider work.
	root := filepath.Join(home, ".config", "taletty")
	if err := os.MkdirAll(root, 0700); err != nil {
		return reportFailure(out, errOut, *machine, audible.ErrAuthStorage)
	}
	client, err := deps.open(ctx, root, resolvedPython)
	if err != nil {
		return reportFailure(out, errOut, *machine, err)
	}
	var snapshot model.LibrarySnapshot
	var download model.DownloadResult
	switch command {
	case "login":
		err = client.Login(ctx, terminal)
	case "sync":
		snapshot, err = client.Library(ctx, "cli-sync", nil)
	default:
		download, err = client.Download(ctx, "cli-download", model.BookID{Provider: "audible", Namespace: *marketplace, ID: asin}, nil)
	}
	closeErr := client.Close(context.Background()) // Operation joined; canceled ctx must not skip release.
	if closeErr != nil {
		io.WriteString(errOut, "Authentication session cleanup failed.\n")
		if err == nil {
			err = audible.ErrAuthStorage
		}
	}
	if err != nil {
		if command == "download" {
			return reportDownloadFailure(out, errOut, *machine, download, err)
		}
		return reportFailure(out, errOut, *machine, err)
	}
	switch command {
	case "sync":
		if *machine {
			err = json.NewEncoder(out).Encode(struct {
				Snapshot model.LibrarySnapshot `json:"snapshot"`
			}{snapshot})
		} else {
			_, err = fmt.Fprintf(out, "Library synced: %d books.\n", len(snapshot.Books))
		}
	case "download":
		if *machine {
			err = json.NewEncoder(out).Encode(struct {
				Download model.DownloadResult `json:"download"`
			}{download})
		} else if download.Reused {
			_, err = fmt.Fprintf(out, "Media ready (reused): %s\n", download.Media.Path)
		} else {
			_, err = fmt.Fprintf(out, "Media downloaded: %s\n", download.Media.Path)
		}
	default:
		_, err = io.WriteString(out, "Signed in. Private credentials saved.\n")
	}
	if err != nil {
		return outputFailure(errOut)
	}
	return 0
}

func validExplicitPython(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0)
}

func validASIN(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Retry   string `json:"retry"`
}

func classify(err error) failure {
	result := failure{"internal", "Command failed.", "none"}
	switch {
	case errors.Is(err, audible.ErrTerminal):
		return failure{"terminal_required", "Login terminal ownership or restoration failed.", "none"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure{"canceled", "Operation canceled.", "none"}
	case errors.Is(err, audible.ErrAuthBusy):
		return failure{"busy", "Another session owns the Taletty root.", "user_retry"}
	case errors.Is(err, audible.ErrAuthStorage):
		return failure{"storage_failed", "Authentication storage failed.", "none"}
	case errors.Is(err, audible.ErrRuntimeMissing):
		return failure{"runtime_missing", "Python runtime could not start.", "none"}
	case errors.Is(err, audible.ErrRuntimeIncompatible):
		return failure{"runtime_incompatible", "Python bridge readiness could not be validated.", "none"}
	}
	// Library returns engine-owned safe records, never raw provider exception text.
	var provider *model.Failure
	if errors.As(err, &provider) {
		return failure{provider.Code, provider.Message, provider.Retry}
	}
	var login *audible.LoginError
	if errors.As(err, &login) {
		switch login.Code {
		case "canceled", "terminal_required", "invalid_input", "runtime_missing", "runtime_incompatible", "login_failed", "storage_failed", "storage_uncertain", "internal":
			result.Code, result.Message = login.Code, login.Error()
		}
	}
	return result
}
func reportFailure(out, errOut io.Writer, machine bool, err error) int {
	return reportDownloadFailure(out, errOut, machine, model.DownloadResult{}, err)
}

func reportDownloadFailure(out, errOut io.Writer, machine bool, download model.DownloadResult, err error) int {
	safe := classify(err)
	if machine {
		var attached *model.DownloadResult
		if download.OperationID != "" {
			attached = &download
		}
		if json.NewEncoder(out).Encode(struct {
			Download *model.DownloadResult `json:"download,omitempty"`
			Failure  failure               `json:"failure"`
		}{attached, safe}) != nil {
			return outputFailure(errOut)
		}
	} else {
		if download.Media != nil {
			fmt.Fprintf(errOut, "Media ready: %s\n", download.Media.Path)
		}
		fmt.Fprintln(errOut, safe.Message)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || safe.Code == "canceled" {
		return 130
	}
	return 1
}
func outputFailure(errOut io.Writer) int {
	io.WriteString(errOut, "Output could not be written.\n")
	return 1
}
