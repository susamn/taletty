# Coverage and module-inventory tooling

Bootstrap slices #36/#37 implement real tooling and per-module gates. Parent #4
remains open for #38's command harness/check policy. There is no runnable Taletty
CLI, Audible adapter or UI yet; do not mistake foundational coverage for features.

## Run locally

From repository root, install test-only tooling in an isolated environment:

```sh
python3 -m venv /tmp/taletty-quality
/tmp/taletty-quality/bin/python -m pip install -r tools/requirements.txt
python3 tools/check_inventory.py --root .
/tmp/taletty-quality/bin/python -m coverage run --branch --source=tools -m unittest discover -s tests/tooling -v
/tmp/taletty-quality/bin/python -m coverage report -m
mkdir -p coverage
/tmp/taletty-quality/bin/python -m coverage json -o coverage/tooling.json
```

Then build and test **each module independently**:

```sh
for module in app engine; do
  (
    set -eu
    cd "$module"
    export GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=1
    test -z "$(gofmt -l .)"
    go mod tidy -diff
    go vet ./...
    go build ./...
    go test -count=1 -race -covermode=atomic -coverpkg=./... -coverprofile="../coverage/$module.out" ./...
  ) || exit 1
done
python3 tools/coverage_gate.py \
  --require tooling --require engine --require app \
  --python tooling=coverage/tooling.json \
  --go engine=coverage/engine.out --go app=coverage/app.out
```

Go modules currently declare Go 1.26.0 and CI verifies the 1.26/1.27 toolchains.
This is a bootstrap language requirement, not a completed distribution support
matrix. No external Go dependencies means no fabricated go.sum or local replace.
Python adapter dependencies/coverage will be registered when adapter code lands.

## Validator semantics

The validator needs only Python's standard library. It reads coverage.py statement
counts (not its branch-combined or rounded percentage) and standard Go coverprofile
statement weights. Valid repeated Go blocks from multi-package `-coverpkg` runs
are merged once, covered if any record was executed; conflicting weights fail.
It rejects exclusions, inconsistent Python file totals, zero executable statements,
malformed/missing reports, reused paths, duplicate target names and missing/extra
targets. Each independent ratio must be >=95%; no pooling. Exit codes: 0 pass,
1 failing/invalid report, 2 invalid CLI usage. Synthetic fixtures test policy, not
application behavior. Fresh report generation remains the trusted CI responsibility.

## Inventory and real CI checks

`check_inventory.py` discovers all go.mod files and validates them against the
registered module roots/identities. Adding/removing a module without registration
fails; the validated list feeds the workflow matrix, so new registered modules
cannot silently miss the race/vet/coverage jobs. Go source outside module roots
fails. Tooling/tests own existing Python files; new adapter/other production Python
fails until its real all-source coverage job is registered in the same PR.
Generated `.git`, `.venv`, `__pycache__`, `coverage`, and vendored dependencies are
not production target discovery; `tests/` holds test-only Python/Go fixtures.

Actual checks are `Coverage tooling` and `Go app/engine (1.26.x/1.27.x)` (four Go
matrix checks). All run on every PR with pinned actions, without live credentials,
providers or protected audio. Each matrix run fails on test/vet/build/format/tidy
errors before accepting its fresh report; no stale report may substitute for a
failed command. Coverage artifacts are uploaded as evidence, not as a replacement
for test status. #38 verifies required-check policy after actual check names exist.

No no-op green Python-adapter, UI, PTY or mpv jobs exist. App currently has only
read-only default-root resolution, engine only validated provider-neutral BookID.
No filesystem initialization, SQLite schema, auth or playback is implied. J14's
path-resolution portion is tested, but the full journey remains pending. Layout/PTY
journeys and generated-media integration accompany #12/#11.
