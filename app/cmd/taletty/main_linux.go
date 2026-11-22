package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/susamn/taletty/app/internal/app"
	"github.com/susamn/taletty/app/internal/terminal"
)

const usage = "Usage: taletty [--python /path/to/venv/bin/python]\nInteractive audiobook companion using ~/.config/taletty/.\n"

type dependencies struct {
	userHomeDir   func() (string, error)
	openTTY       func() (*os.File, error)
	openWorkspace func(context.Context, string, string) (*app.Coordinator, error)
	runSession    func(context.Context, *app.Coordinator, *os.File, io.Writer) error
}

func defaultDeps() dependencies {
	return dependencies{
		userHomeDir: os.UserHomeDir,
		openTTY:     func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) },
		openWorkspace: func(ctx context.Context, home, python string) (*app.Coordinator, error) {
			return app.OpenEngineWorkspace(ctx, home, python, app.Options{SuspendForLogin: true})
		},
		runSession: func(ctx context.Context, coord *app.Coordinator, tty *os.File, out io.Writer) error {
			return app.RunSession(ctx, coord, tty, out, terminal.Run)
		},
	}
}

func main() {
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDeps())
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, errOut io.Writer, deps dependencies) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		if _, err := io.WriteString(out, usage); err != nil {
			_, _ = io.WriteString(errOut, "Output could not be written.\n")
			return 1
		}
		return 0
	}

	flags := flag.NewFlagSet("taletty", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	python := flags.String("python", "", "Python executable override")
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		if _, writeErr := io.WriteString(out, usage); writeErr != nil {
			_, _ = io.WriteString(errOut, "Output could not be written.\n")
			return 1
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
	if err != nil || flags.NArg() != 0 || (pythonSet && !validExplicitPython(*python)) {
		_, _ = io.WriteString(errOut, usage)
		return 2
	}
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintln(errOut, app.SanitizeError(err))
		return 130
	}

	home, err := deps.userHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		_, _ = io.WriteString(errOut, "Home directory is invalid.\n")
		return 1
	}

	tty, err := deps.openTTY()
	if err != nil {
		_, _ = io.WriteString(errOut, "Taletty requires an interactive terminal.\n")
		return 1
	}
	defer tty.Close()

	coord, err := deps.openWorkspace(ctx, home, *python)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, app.SanitizeError(err))
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 130
		}
		return 1
	}

	runErr := deps.runSession(ctx, coord, tty, tty)
	closeErr := coord.Close(context.Background())
	if runErr != nil {
		_, _ = fmt.Fprintln(errOut, app.SanitizeError(runErr))
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return 130
		}
		return 1
	}
	if closeErr != nil {
		_, _ = fmt.Fprintln(errOut, app.SanitizeError(closeErr))
		return 1
	}
	return 0
}

func validExplicitPython(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0)
}
