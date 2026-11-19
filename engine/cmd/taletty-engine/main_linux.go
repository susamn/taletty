package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/susamn/taletty/engine/internal/cli"
)

func main() {
	// Command owns shutdown signals; connector libraries never install handlers.
	// Broken stdout must reach Run's write-error path rather than terminate mid-cleanup.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code) // All synchronous connector users/session cleanup have returned.
}
