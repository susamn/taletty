// Package testkit drives model journeys without a terminal or command goroutines.
package testkit

import (
	"context"
	"errors"
	"reflect"

	tea "charm.land/bubbletea/v2"
)

var (
	ErrLimit       = errors.New("journey step or pending-command limit exceeded")
	ErrStopped     = errors.New("journey has quit")
	ErrUnsupported = errors.New("runtime control requires a terminal test, not the model runner")
)

// Runner executes fake, nonblocking commands in a test-chosen completion order.
// It is not a concurrent Bubble Tea runtime. Inject fake clocks/services into
// the model; real I/O, waits and terminal controls belong in integration tests.
// Context cancellation is checked between transitions, not inside arbitrary Cmds.
// A runner is owned by one test goroutine.
type Runner struct {
	Model   tea.Model
	ctx     context.Context
	pending []tea.Cmd
	limit   int
	steps   int
	quit    bool
	err     error
}

// New queues Init without executing its returned commands. Limit bounds both
// pending commands and total command executions/model updates across the journey.
func New(ctx context.Context, model tea.Model, limit int) (*Runner, error) {
	if ctx == nil || model == nil || limit < 1 {
		return nil, errors.New("context, model and positive journey limit are required")
	}
	r := &Runner{Model: model, ctx: ctx, limit: limit}
	if err := r.ready(); err != nil {
		return nil, err
	}
	if command := model.Init(); command != nil {
		r.pending = []tea.Cmd{command}
	}
	return r, nil
}

func (r *Runner) stop(err error) error {
	r.err, r.pending = err, nil
	return err
}

func (r *Runner) ready() error {
	if r.err != nil {
		return r.err
	}
	if err := r.ctx.Err(); err != nil {
		return r.stop(err)
	}
	if r.quit {
		return ErrStopped
	}
	return nil
}

func (r *Runner) step() error {
	if err := r.ready(); err != nil {
		return err
	}
	if r.steps >= r.limit {
		return r.stop(ErrLimit)
	}
	r.steps++
	return nil
}

func (r *Runner) enqueue(commands ...tea.Cmd) error {
	for _, command := range commands {
		if command != nil {
			if len(r.pending) >= r.limit {
				return r.stop(ErrLimit)
			}
			r.pending = append(r.pending, command)
		}
	}
	return nil
}

// Pending is the number of commands available for explicit scheduling.
func (r *Runner) Pending() int { return len(r.pending) }

// Send delivers user input or a fake service/clock message through real Update.
func (r *Runner) Send(message tea.Msg) error {
	if err := r.step(); err != nil {
		return err
	}
	model, command := r.Model.Update(message)
	if model == nil {
		return r.stop(errors.New("Update returned a nil model"))
	}
	r.Model = model
	return r.enqueue(command)
}

// Step completes one pending command. Selecting a nonzero index lets tests
// exercise late/stale replies. Public batches expand without promising runtime
// order; private Sequence/terminal controls fail instead of silently disappearing.
func (r *Runner) Step(index int) error {
	if err := r.ready(); err != nil {
		return err
	}
	if index < 0 || index >= len(r.pending) {
		return errors.New("pending command index out of range")
	}
	if err := r.step(); err != nil {
		return err
	}
	command := r.pending[index]
	r.pending = append(r.pending[:index], r.pending[index+1:]...)
	message := command()
	if err := r.ready(); err != nil {
		return err
	}
	switch message := message.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		return r.enqueue(message...)
	case tea.QuitMsg:
		r.quit, r.pending = true, nil
		return nil
	default:
		if reflect.TypeOf(message).PkgPath() == reflect.TypeFor[tea.BatchMsg]().PkgPath() {
			return r.stop(ErrUnsupported)
		}
		return r.Send(message)
	}
}

// Drain uses FIFO for a finite scripted journey. Streaming waits must be fake;
// recurring commands terminate with ErrLimit instead of running indefinitely.
func (r *Runner) Drain() error {
	if err := r.ready(); err != nil {
		return err
	}
	for r.Pending() > 0 {
		if err := r.Step(0); err != nil {
			return err
		}
	}
	return nil
}
