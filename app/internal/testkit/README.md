# Model journey runner

Drive real Bubble Tea v2 `Init`, `Update`, `Cmd` and `BatchMsg` without a terminal,
real timers, provider calls or command goroutines. `Runner.Model` is the current
replacement model; assert its typed state and `View()` after every important action.
This helper is internal test infrastructure, not Taletty's production event loop.

## Scheduling

1. Inject a fake provider/service and clock into the model.
2. `New(ctx, model, limit)` queues Init's command without executing it.
3. `Send(keyOrMessage)` calls real Update and queues its command.
4. `Step(index)` explicitly completes a chosen pending command. This allows a newer
   request to complete before an older one, testing stale-reply protection.
5. `Drain()` completes a finite script FIFO. Expand a Batch with one Step, then
   choose the child completion order; FIFO is not a concurrency guarantee.

Commands returning nil are consumed without an Update. Quit clears pending work;
further interaction returns ErrStopped. Cancellation drops pending work and rejects
a result canceled during a cooperative fake command. Step budget and queue limits
make recurring streams/expansion fail explicitly. Failed runners retain their error.
Invalid indexes do not execute work. Each command execution and Update costs a step.

The included fixture journey injects a fake service/time source, issues two refreshes,
advances fake time and completes replies out of order. It verifies that the model's
latest request wins. This proves the harness seam, **not a completed library journey**.

## Deliberate limits

- Own the runner from one goroutine. Do not mutate it concurrently.
- Fake commands must return immediately and use injected dependencies. The helper
  checks context between transitions; it cannot interrupt an arbitrary blocked Go
  function. It spawns no abandoned timeout goroutines. Package-level `go test
  -timeout=60s` and CI timeouts bound accidental real blocking.
- `tea.Sequence` and other framework-owned command control messages are rejected,
  not silently passed to models. Sequence is private in Bubble Tea v2; the helper
  does not reflectively emulate its internal runtime. If needed, use a real-program
  integration test or add an explicitly reviewed/tested extension.
- Feed key/resize inputs through Send; command-driven terminal/renderer/exec controls
  belong in actual Bubble Tea/PTY integration tests. Framework controls are detected
  by their package identity, not by parsing internal command names.
- No arbitrary command-order randomness: enumerate meaningful completion orders in
  table tests. This is deterministic scheduling, not a proof of all possible races.

Bubble Tea v2.0.0 is deliberately pinned: its API supports these public contracts
and its Go floor fits the existing 1.26/1.27 matrix. Later upgrades need normal
dependency review and the harness/control-message regressions. Source is newly
written; no prototype UI/runtime was copied.

J01–J16 tests will exercise the actual app coordinator as features land. #12 owns
layout, focus/typing and terminal/PTY journeys; #11 owns generated-media mpv tests.
Do not use helper unit tests as substitutes for those application journeys.
