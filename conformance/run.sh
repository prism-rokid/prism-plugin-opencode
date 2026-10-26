#!/usr/bin/env bash
# End-to-end conformance run for prism-plugin-opencode.
#
#  1. Validates pluginbridge-plugin.yaml against the strict SDK manifest schema
#     (../prism-plugin-sdk/conformance/manifest.schema.json), after first
#     proving the validator itself on the SDK's valid/invalid fixtures.
#  2. Builds the adapter (cmd/opencode-adapter), the fake opencode server and
#     the stdio protocol driver.
#  3. Starts the fakeserver on a free port and runs the driver:
#     driver --stdio JSON-RPC--> adapter --HTTP/SSE--> fakeserver.
#
# Exit codes: 0 = all green; 1 = conformance failure; 2 = setup problem
# (e.g. adapter not implemented yet).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SDK_ROOT="../prism-plugin-sdk"
SCHEMA="$SDK_ROOT/conformance/manifest.schema.json"
ADAPTER_PKG="./cmd/opencode-adapter"
ADAPTER_BIN="bin/prism-plugin-opencode"
# Optional: set CONFORMANCE_SERVER_PASSWORD to make the fakeserver require
# basic auth (user "opencode") and have the driver forward the password to the
# adapter — exercises the OPENCODE_SERVER_PASSWORD path end to end.
AUTH_ARGS=()
if [ -n "${CONFORMANCE_SERVER_PASSWORD:-}" ]; then
  AUTH_ARGS=(-password "$CONFORMANCE_SERVER_PASSWORD")
fi

fail() { echo "run.sh: FAIL: $*" >&2; exit 1; }
info() { echo "==> $*"; }

TMP="$(mktemp -d)"
FAKESERVER_PID=""
cleanup() {
  if [ -n "$FAKESERVER_PID" ] && kill -0 "$FAKESERVER_PID" 2>/dev/null; then
    kill "$FAKESERVER_PID" 2>/dev/null || true
    wait "$FAKESERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
info "0/4 checking prerequisites"
if [ ! -d cmd/opencode-adapter ]; then
  cat >&2 <<'EOF'
run.sh: 适配器尚未实现 —— cmd/opencode-adapter/ 目录不存在。
run.sh: adapter not implemented yet (cmd/opencode-adapter is missing).

cmd/opencode-adapter 由并行开发的子任务交付（见 docs/CONTRACT.md）。
它落地后本脚本才能跑通 stdio -> HTTP/SSE 全链路。在此之前可以单独验证：
  - manifest:  见下方 validate_manifest 用法
  - fakeserver: go run ./conformance/fakeserver -port 0
  - driver:    go run ./conformance/driver -adapter <binary> -server-url <url>
EOF
  exit 2
fi
command -v go >/dev/null 2>&1 || fail "go toolchain not found in PATH"
[ -f "$SCHEMA" ] || fail "manifest schema not found at $SCHEMA (expected ../prism-plugin-sdk checkout)"

# YAML -> JSON for the validator (Go stdlib has no YAML; use whichever local
# interpreter has a YAML module — PyYAML when installed, macOS ruby otherwise).
yaml_to_json() {
  if python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 -c 'import sys, yaml, json; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)' "$1"
  elif command -v ruby >/dev/null 2>&1; then
    ruby -ryaml -rjson -e 'print JSON.generate(YAML.safe_load(File.read(ARGV[0])))' "$1"
  else
    echo "run.sh: need python3+PyYAML or ruby to convert the YAML manifest" >&2
    return 1
  fi
}

# ---------------------------------------------------------------------------
info "1/4 validating pluginbridge-plugin.yaml against $SCHEMA"
go build -o "$TMP/validate_manifest" ./conformance/validate_manifest

# Sanity-check the validator itself on the SDK fixtures first, so a green
# result below cannot be an accidentally-toothless checker.
"$TMP/validate_manifest" -schema "$SCHEMA" -manifest "$SDK_ROOT/conformance/manifest.valid.json" \
  || fail "validator self-test failed: SDK valid fixture must pass"
if "$TMP/validate_manifest" -schema "$SCHEMA" -manifest "$SDK_ROOT/conformance/manifest.invalid-unknown-field.json" >/dev/null 2>&1; then
  fail "validator self-test failed: SDK invalid fixture must NOT pass"
fi
info "validator self-test OK (SDK valid fixture passes, invalid fixture is rejected)"

yaml_to_json pluginbridge-plugin.yaml > "$TMP/manifest.json"
# managed_launch_specs is Hub-defined (prism-hub docs/13 §2.1, internal/plugin/
# manifest.go) but still missing from the SDK schema; every sibling protocol-
# native plugin carries it too. Tolerated explicitly, loudly, and only here.
"$TMP/validate_manifest" -schema "$SCHEMA" -manifest "$TMP/manifest.json" \
  -allow-extra managed_launch_specs \
  || fail "manifest violates the SDK schema (beyond the tolerated managed_launch_specs note above)"

# ---------------------------------------------------------------------------
info "2/4 building adapter, fakeserver and driver"
if ! go build -o "$ADAPTER_BIN" "$ADAPTER_PKG" 2>"$TMP/adapter-build.log"; then
  cat "$TMP/adapter-build.log" >&2
  cat >&2 <<'EOF'

run.sh: 适配器（或其依赖 internal/）当前编译失败。
run.sh: the adapter failed to compile — cmd/opencode-adapter and internal/ are
delivered by parallel tasks (see docs/CONTRACT.md) and may be mid-flight.
Retry once both land; until then the conformance chain below cannot run.
EOF
  exit 2
fi
go build -o "$TMP/fakeserver" ./conformance/fakeserver
go build -o "$TMP/driver" ./conformance/driver

# ---------------------------------------------------------------------------
info "3/4 starting fake opencode server"
"$TMP/fakeserver" -port 0 -prompt-delay-ms 400 "${AUTH_ARGS[@]+"${AUTH_ARGS[@]}"}" \
  >"$TMP/fakeserver.stdout" 2>"$TMP/fakeserver.stderr" &
FAKESERVER_PID=$!
SERVER_URL=""
for _ in $(seq 1 50); do
  if ! kill -0 "$FAKESERVER_PID" 2>/dev/null; then
    echo "--- fakeserver stderr ---" >&2
    cat "$TMP/fakeserver.stderr" >&2
    fail "fakeserver exited at startup"
  fi
  # grep exits 1 until the LISTEN line appears; neutralize it for set -e.
  SERVER_URL="$(grep -m1 '^LISTEN ' "$TMP/fakeserver.stdout" 2>/dev/null | awk '{print $2}' || true)"
  [ -n "$SERVER_URL" ] && break
  sleep 0.1
done
[ -n "$SERVER_URL" ] || fail "fakeserver did not report its URL in time"
info "fakeserver listening on $SERVER_URL"

# ---------------------------------------------------------------------------
info "4/4 running stdio conformance driver against the adapter"
set +e
OPENCODE_SERVER_URL="$SERVER_URL" "$TMP/driver" \
  -adapter "$ADAPTER_BIN" \
  -server-url "$SERVER_URL" \
  "${AUTH_ARGS[@]+"${AUTH_ARGS[@]}"}"
RC=$?
set -e

if [ "$RC" -eq 0 ]; then
  info "CONFORMANCE RESULT: PASS"
else
  info "CONFORMANCE RESULT: FAIL (driver exit $RC; fakeserver log kept below)"
  echo "--- fakeserver stderr tail ---" >&2
  tail -n 40 "$TMP/fakeserver.stderr" >&2 || true
fi
exit "$RC"
