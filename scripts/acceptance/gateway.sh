#!/usr/bin/env bash
# Only run against a disposable upstream checkout. The fixture is an example of
# that checkout's contract crate so it uses exactly the gateway's SDK and lockfile.
set -euo pipefail
mode=${1:?build or boot}
source_dir=$(cd "${2:?busbar checkout}" && pwd)
state_dir=${3:?absolute output directory}
case "$state_dir" in /*) ;; *) echo 'output directory must be absolute' >&2; exit 1 ;; esac
script_dir=$(cd "$(dirname "$0")" && pwd)
target_dir=${CARGO_TARGET_DIR:-$source_dir/target}
case "$target_dir" in /*) ;; *) target_dir="$source_dir/$target_dir" ;; esac
pack="$target_dir/release/busbar-plugin-pack"
gateway="$target_dir/release/busbar"
module=tfacc-gate
mkdir -p "$state_dir/plugins"

case "$mode" in
  build)
    python3 - "$source_dir" "$script_dir/gate.rs" <<'PY'
import json
import pathlib
import sys
import tomllib

source, fixture = map(pathlib.Path, sys.argv[1:])
manifest = source / "crates/busbar-contract/Cargo.toml"
if not manifest.is_file() or not (source / "dist/default/Cargo.toml").is_file():
    sys.exit("acceptance fixture requires the current busbar-contract SDK and default distribution")
text = manifest.read_text()
existing = [entry for entry in tomllib.loads(text).get("example", []) if entry["name"] == "tfacc_gate"]
if existing:
    if existing != [{"name": "tfacc_gate", "path": str(fixture), "crate-type": ["cdylib"]}]:
        sys.exit("conflicting tfacc_gate example in upstream checkout")
else:
    manifest.write_text(text + '\n[[example]]\nname = "tfacc_gate"\npath = ' + json.dumps(str(fixture)) + '\ncrate-type = ["cdylib"]\n')
PY
    (
      cd "$source_dir"
      cargo build --locked --release --target-dir "$target_dir" \
        -p busbar-plugin-loader --features pack --bin busbar-plugin-pack
      keys=$("$pack" keygen)
      private=$(printf '%s\n' "$keys" | sed -n 's/^private.*: //p')
      public=$(printf '%s\n' "$keys" | sed -n 's/^public.*: //p')
      [[ "$private" =~ ^[0-9a-fA-F]{64}$ && "$public" =~ ^[0-9a-fA-F]{64}$ ]] || {
        echo 'unparseable ephemeral signing keypair' >&2; exit 1;
      }
      printf '%s\n' "$public" > "$state_dir/release-public-key.txt"
      BUSBAR_RELEASE_PUBKEY="$public" cargo build --locked --release \
        --manifest-path dist/default/Cargo.toml --target-dir "$target_dir" --bin busbar
      cargo build --locked --release --target-dir "$target_dir" \
        -p busbar-contract --example tfacc_gate
      library="$target_dir/release/examples/libtfacc_gate.so"
      test -s "$library"
      BUSBAR_SIGN_KEY="$private" "$pack" pack --lib "$library" \
        --name "$module" --alias "$module" --kind hook \
        --publisher busbar --license Apache-2.0 --out "$state_dir/plugins/$module.tar.gz"
      test -s "$state_dir/plugins/$module.tar.gz"
      "$pack" verify "$state_dir/plugins/$module.tar.gz" --keys "$public" \
        --name "$module" > "$state_dir/plugin-verification.txt"
    )
    ;;
  boot)
    test -s "$state_dir/plugins/$module.tar.gz"
    "$gateway" --generate-signing-key > "$state_dir/signing.key"
    # JSON is also YAML; the config acceptance test reuses this exact boot
    # document so applying it cannot erase plugin loading or admin identity.
    python3 - "$state_dir" "$module" <<'PYCONFIG'
import json
import pathlib
import sys
state = pathlib.Path(sys.argv[1])
catalog = {"anthropic": {"protocol": "anthropic", "base_url": "https://api.anthropic.com"}}
(state / "providers.json").write_text(json.dumps(catalog, indent=2) + "\n")
config = {
    "providers_file": str(state / "providers.json"),
    "listen": "127.0.0.1:8080",
    "admin_listen": "127.0.0.1:8081",
    "admin_require_mtls": False,
    "store": {"module": "memory"},
    "plugins": {"enabled": True, "dir": str(state / "plugins")},
    "identity-providers": {
        "admin-tokens": {"module": "admin-tokens", "token": {"env": "BUSBAR_ADMIN_TOKEN"}},
    },
    "auth": {
        "chain": ["keys"],
        "signing_key": {"file": str(state / "signing.key")},
        "admin_auth": ["admin-tokens"],
    },
    "providers": {"anthropic": {"api_key": {"env": "ANTHROPIC_API_KEY"}}},
    "models": {"test-model": {"provider": "anthropic", "max_concurrent": 1}},
    "hooks": {
        "tfacc-fixture-proof": {"kind": "gate", "module": sys.argv[2], "settings": {"threshold": 0.5}},
    },
}
(state / "config.yaml").write_text(json.dumps(config, indent=2) + "\n")
PYCONFIG
    ANTHROPIC_API_KEY=dummy BUSBAR_ADMIN_TOKEN=tfacc-admin-token \
      BUSBAR_CONFIG="$state_dir/config.yaml" \
      "$gateway" > "$state_dir/busbar.log" 2>&1 &
    pid=$!
    echo "$pid" > "$state_dir/pid"
    ready=false
    for _ in $(seq 1 30); do
      if curl -fsS http://localhost:8081/api/v1/admin/hooks/tfacc-fixture-proof \
        -H 'x-admin-token: tfacc-admin-token' > "$state_dir/hook.json"; then
        ready=true; break
      fi
      kill -0 "$pid" 2>/dev/null || break
      sleep 2
    done
    if [ "$ready" != true ]; then
      cat "$state_dir/busbar.log" >&2
      exit 1
    fi
    curl -fsS http://localhost:8081/api/v1/admin/hooks/tfacc-fixture-proof/health \
      -H 'x-admin-token: tfacc-admin-token' > "$state_dir/hook-health.json"
    python3 - "$state_dir/hook.json" "$state_dir/hook-health.json" "$module" <<'PY'
import json
import sys
with open(sys.argv[1]) as stream:
    hook = json.load(stream)
with open(sys.argv[2]) as stream:
    health = json.load(stream)
if hook.get("kind") != "gate" or hook.get("transport", {}).get("target") != sys.argv[3]:
    sys.exit("gateway did not expose the expected gate definition")
if (health.get("reachable") is not True
        or health.get("transport", {}).get("kind") != "plugin"
        or health.get("transport", {}).get("target") != sys.argv[3]):
    sys.exit("gateway did not resolve the signed gate fixture to a loaded plugin")
PY
    echo "BUSBAR_TEST_CONFIG_FILE=$state_dir/config.yaml" >> "${GITHUB_ENV:?GITHUB_ENV required}"
    echo "BUSBAR_TEST_HOOK_MODULE=$module" >> "${GITHUB_ENV:?GITHUB_ENV required}"
    ;;
  *) echo "unknown mode: $mode" >&2; exit 1 ;;
esac
