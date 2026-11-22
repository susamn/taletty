# Coverage and source-inventory gates

The app, engine, Python adapter and gate tooling are measured **independently**.
Coverage demonstrates only implemented behavior: shell/Help, auth/process/framing
and read-only diagnostics are not complete login, library, playback or UI journeys.

## Local verification

From repository root, use a fresh explicit CPython 3.12 test venv (not an old-app
runtime). This installs test dependencies, not Taletty or a managed user runtime:

```sh
ADAPTER_DIR=engine/connectors/audible/internal/bridge/adapter
python3.12 -m venv /tmp/taletty-quality
/tmp/taletty-quality/bin/python -m pip install -r tools/requirements.txt -r "$ADAPTER_DIR/requirements.txt"
PYTHON=/tmp/taletty-quality/bin/python
export TALETTY_TEST_PYTHON="$PYTHON"
python3 tools/check_inventory.py --root .
mkdir -p coverage
"$PYTHON" -m coverage run --branch --source=tools -m unittest discover -s tests/tooling -v
"$PYTHON" -m coverage json -o coverage/tooling.json
"$PYTHON" -B -m coverage run --branch --source="$ADAPTER_DIR" -m unittest discover -s tests/adapter -v
"$PYTHON" -m coverage report -m
"$PYTHON" -m coverage json -o coverage/adapter.json
python3 tools/check_inventory.py --root . --adapter-report coverage/adapter.json
```

Engine and app contract tests execute real Python/media tools where required. CI
explicitly installs `ffmpeg`/`ffprobe`/`mpv` and sets `TALETTY_REQUIRE_MEDIA_TOOLS=1`,
requiring actual tools to be ready for adapter/engine media preparation and app
`mpv --ao=null` playback tests. Locally absent tools produce truthful missing
states or explicit local skips; deterministic fake-tool/fake-IPC success, failure
and timeout cases still run. No provider/account or protected-media fixture is
used. No installer or silent system pip mutation exists.

Build/test **each Go module independently** (Go 1.26/1.27 in CI):

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
    go test -count=1 -race -timeout=60s -covermode=atomic -coverpkg=./... -coverprofile="../coverage/$module.out" ./...
  ) || exit 1
done
python3 tools/coverage_gate.py \
  --require tooling --require adapter --require engine --require app \
  --python tooling=coverage/tooling.json --python adapter=coverage/adapter.json \
  --go engine=coverage/engine.out --go app=coverage/app.out
```

There is no permanent relative module replace. Pinned Go dependencies/go.sum are
real; source cross-builds do not establish release/runtime support. See #26 for
artifact runtime strategy, full dependency locking and minimum platform versions.

## Fail-closed semantics

`coverage_gate.py` reads unrounded statement counts, rejects exclusions, malformed/
empty/missing reports, inconsistent totals, repeated paths/targets and missing/extra
targets. Each required target independently needs >=95%, never a pooled average.
Go repeated instrumented blocks merge once; conflicting weights fail. Python branch
counts are additionally reported, not mixed into the statement threshold.

`check_inventory.py` discovers all go.mod files and validates registered identities;
the result drives the actual Go matrix. Go source outside modules and Python outside
tools/tests/the registered adapter directory fail. The adapter cannot disappear.
`--adapter-report` requires **exact equality** between every discovered production
adapter `.py` and report filenames: adding an unimported file or dropping a file from
a 100%-looking report fails. Fresh all-source report generation remains mandatory.
Generated `.git`, `.venv`, `__pycache__`, `coverage` and vendor directories are ignored;
`tests/` contains test-only fixtures. There are no production source exclusions.

## Real checks

- **Coverage tooling**: inventory, validator tests and independent tooling coverage.
- **Python adapter (3.12)**: actual production source, real SDK/tool readiness plus
  fake-provider/error tests, all-source report equality and independent adapter gate.
- **Go app/engine (1.26.x/1.27.x)**: four format/tidy/vet/build/race/module gates;
  engine includes actual Go↔Python bootstrap/SDK and fake-provider contracts.
- **Quality gate**: always runs and requires tooling, the whole Go matrix **and the
  adapter job** to succeed. Failure, cancellation or skip fails. Tests exercise all
  64 aggregation status combinations; missing adapter dependency/mapping fails too.

All checks run on every PR with pinned actions. Coverage artifacts are evidence,
not substitutes for tests/status. Required stable Quality gate protection remains
unchanged; see [check policy](../docs/quality-checks.md). No no-op UI/PTY/media job
stands in for [actual feature journeys](../docs/testing.md).
