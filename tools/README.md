# Coverage gate tooling

This is the first implementation slice of #4, tracked by #36. It measures **the
validator tooling itself**, not nonexistent Taletty engine/app/adapter code.

## Run locally

From repository root, use an isolated test environment:

```sh
python3 -m venv /tmp/taletty-quality
/tmp/taletty-quality/bin/python -m pip install -r tools/requirements.txt
/tmp/taletty-quality/bin/python -m coverage run --branch --source=tools -m unittest discover -s tests/tooling -v
/tmp/taletty-quality/bin/python -m coverage report -m
mkdir -p coverage
/tmp/taletty-quality/bin/python -m coverage json -o coverage/tooling.json
python3 tools/coverage_gate.py --require tooling --python tooling=coverage/tooling.json
```

The validator itself needs only Python's standard library. It reads coverage.py
statement counts (not its branch-combined or rounded percentage) and Go standard
coverprofile statement weights. It validates file-summary totals, rejects excluded
statements, and fails on zero executable statements, malformed/missing reports,
reused report paths, duplicate targets/Go blocks, missing or unexpected targets,
and any independent ratio below 95%. Every requested report must pass; pooling is
not allowed. Exit 0 means pass, 1 invalid/failing coverage and 2 invalid CLI usage.

Synthetic report fixtures verify policy; they are **not** application coverage.
Reporting branch coverage remains useful but does not replace the statement gate.
The validator checks supplied report structure/counts; it cannot prove a report
came from the current source or discover omitted source files by itself. Trusted CI
commands must generate fresh reports from all production packages/files.

## Staged integration

`Quality / Coverage tooling` is a real CI job. It runs on every PR, including docs,
without live credentials, providers or audio. Actions are pinned by commit.
Until #37 integrates real module/adapter target discovery, CI fails if app/engine
roots or a `go.mod` appear without updating this guard and the coverage jobs.
There are no no-op green Go, adapter, PTY or mpv jobs.

Next slice #37 adds separate production-code-bearing Go modules and fresh race/vet/
formatting/coverage jobs with `GOWORK=off` and `-coverpkg=./...`. Each production
module/adapter must appear in the required target inventory. Add the Python adapter
report when code lands, never an empty adapter placeholder. #38 adds the bounded
command/message harness and final required-check policy. Parent #4 remains open.
Actual layout/PTY journeys and generated-media integration accompany #12/#11.

Future multi-target invocation (illustrative paths, **not runnable targets yet**):

```sh
python3 tools/coverage_gate.py \
  --require engine --require app --require bridge \
  --go engine=coverage/engine.out --go app=coverage/app.out \
  --python bridge=coverage/bridge.json
```

Always invoke test commands with fail-fast shell semantics: a test/subprocess error
must fail CI before the gate can accept stale coverage. Do not reuse a report from
an earlier successful run. Keep generated reports under ignored `coverage/`.
