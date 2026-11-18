package testkit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/testkit"
)

type model struct {
	init   tea.Cmd
	update func(tea.Msg) (tea.Model, tea.Cmd)
}

func (m *model) Init() tea.Cmd                           { return m.init }
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return m.update(msg) }
func (m *model) View() tea.View                          { return tea.NewView("fixture") }

func newRunner(t *testing.T, ctx context.Context, m tea.Model, limit int) *testkit.Runner {
	t.Helper()
	r, err := testkit.New(ctx, m, limit)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func message(value string) tea.Cmd { return func() tea.Msg { return value } }

func TestInitBatchSchedulingReplacementAndNilCommand(t *testing.T) {
	var got []string
	next := &model{}
	next.update = func(msg tea.Msg) (tea.Model, tea.Cmd) {
		got = append(got, msg.(string))
		return next, nil
	}
	first := &model{init: tea.Batch(message("first"), message("second"))}
	first.update = func(msg tea.Msg) (tea.Model, tea.Cmd) {
		got = append(got, msg.(string))
		return next, func() tea.Msg { return nil }
	}
	r := newRunner(t, context.Background(), first, 20)
	if r.Pending() != 1 || r.Step(0) != nil || r.Pending() != 2 {
		t.Fatal("Init must queue a batch, then expose its commands")
	}
	if err := r.Step(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if r.Model != next || len(got) != 2 || got[0] != "second" || got[1] != "first" {
		t.Fatalf("replacement model or selected completion order lost: %v", got)
	}
	if r.Model.View().Content != "fixture" || r.Pending() != 0 {
		t.Fatal("inspectable model/view and drained queue required")
	}
	if err := r.Send("input"); err != nil || got[2] != "input" {
		t.Fatal("input did not reach real Update")
	}
}

func TestFakeServiceClockAndOutOfOrderReplyJourney(t *testing.T) {
	type reply struct {
		generation int
		at         time.Time
	}
	now := time.Unix(100, 0)
	generation, calls := 0, 0
	var displayed time.Time
	m := &model{}
	m.update = func(msg tea.Msg) (tea.Model, tea.Cmd) {
		switch msg := msg.(type) {
		case string:
			generation++
			request := generation
			return m, func() tea.Msg { calls++; return reply{request, now} }
		case reply:
			if msg.generation == generation {
				displayed = msg.at
			}
		}
		return m, nil
	}
	r := newRunner(t, context.Background(), m, 20)
	if err := r.Drain(); err != nil { // nil Init is valid
		t.Fatal(err)
	}
	for range 2 {
		if err := r.Send("refresh"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour) // no wall-clock wait or provider call
	if err := r.Step(1); err != nil {
		t.Fatal(err)
	}
	wanted := now
	now = now.Add(time.Hour)
	if err := r.Step(0); err != nil || displayed != wanted || calls != 2 {
		t.Fatal("late stale reply replaced the latest service result")
	}
}

func TestInvalidArgumentsAndSchedulingLeaveNoSideEffects(t *testing.T) {
	for _, tc := range []struct {
		ctx context.Context
		m   tea.Model
		max int
	}{{nil, &model{}, 1}, {context.Background(), nil, 1}, {context.Background(), &model{}, 0}} {
		if _, err := testkit.New(tc.ctx, tc.m, tc.max); err == nil {
			t.Fatal("invalid constructor arguments accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testkit.New(ctx, &model{}, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r := newRunner(t, context.Background(), &model{init: message("unused")}, 10)
	for _, index := range []int{-1, 1} {
		if err := r.Step(index); err == nil || r.Pending() != 1 {
			t.Fatal("invalid scheduling index mutated the queue")
		}
	}
}

func TestCancellationBeforeAndDuringFakeCommand(t *testing.T) {
	for _, cancelInside := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		called := false
		m := &model{init: func() tea.Msg { cancel(); return "discard me" }}
		m.update = func(tea.Msg) (tea.Model, tea.Cmd) { called = true; return m, nil }
		r := newRunner(t, ctx, m, 10)
		if !cancelInside {
			cancel()
		}
		if err := r.Step(0); !errors.Is(err, context.Canceled) || called || r.Pending() != 0 {
			t.Fatal("canceled work reached Update or remained pending")
		}
		if err := r.Drain(); !errors.Is(err, context.Canceled) {
			t.Fatal("terminal error not preserved", err)
		}
	}
}

func TestStepAndQueueLimitsStopRecurringWork(t *testing.T) {
	m := &model{init: message("again")}
	m.update = func(tea.Msg) (tea.Model, tea.Cmd) { return m, m.init }
	r := newRunner(t, context.Background(), m, 3)
	if err := r.Drain(); !errors.Is(err, testkit.ErrLimit) || r.Pending() != 0 {
		t.Fatal("recurring work was not bounded", err)
	}
	batch := &model{init: func() tea.Msg { return tea.BatchMsg{nil, message("a"), message("b")} }}
	r = newRunner(t, context.Background(), batch, 1)
	if err := r.Step(0); !errors.Is(err, testkit.ErrLimit) || r.Pending() != 0 {
		t.Fatal("oversized pending queue accepted", err)
	}
	batch.init = func() tea.Msg { return tea.BatchMsg{nil} }
	r = newRunner(t, context.Background(), batch, 1)
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetExhaustedBeforeCommandExecution(t *testing.T) {
	called := false
	m := &model{init: func() tea.Msg { called = true; return nil }}
	m.update = func(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
	r := newRunner(t, context.Background(), m, 1)
	if err := r.Send("uses budget"); err != nil {
		t.Fatal(err)
	}
	if err := r.Step(0); !errors.Is(err, testkit.ErrLimit) || called {
		t.Fatal("command ran after the journey budget was exhausted", err)
	}
}

func TestQuitNilModelAndUnsupportedRuntimeControls(t *testing.T) {
	r := newRunner(t, context.Background(), &model{init: tea.Quit}, 10)
	if err := r.Drain(); err != nil || r.Pending() != 0 {
		t.Fatal(err)
	}
	if err := r.Send("after quit"); !errors.Is(err, testkit.ErrStopped) {
		t.Fatal(err)
	}
	r = newRunner(t, context.Background(), &model{update: func(tea.Msg) (tea.Model, tea.Cmd) {
		return nil, nil
	}}, 10)
	if err := r.Send("broken"); err == nil {
		t.Fatal("nil replacement model accepted")
	}
	r = newRunner(t, context.Background(), &model{init: tea.Sequence(message("a"), message("b"))}, 10)
	if err := r.Drain(); !errors.Is(err, testkit.ErrUnsupported) {
		t.Fatal("private runtime control silently ignored", err)
	}
}
