"""Targeted wiring/command regressions; no compiler or live gateway required."""
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]


class AcceptanceFixtureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "core"
        self.state = self.root / "state"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.target = self.root / "target"
        (self.target / "release/examples").mkdir(parents=True)
        (self.source / "dist/default").mkdir(parents=True)
        (self.source / "dist/default/Cargo.toml").write_text("")
        self.manifest = self.source / "crates/busbar-contract/Cargo.toml"
        self.manifest.parent.mkdir(parents=True)
        self.manifest.write_text('[package]\nname = "busbar-contract"\nversion = "1.6.0"\n')
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}",
                        CARGO_TARGET_DIR=str(self.target), RUNNER_TEMP=str(self.root),
                        GITHUB_ENV=str(self.root / "env"), CALLS=str(self.root / "calls"),
                        TF_ACC="1", BUSBAR_TEST_HOOK_MODULE="tfacc-gate")
        self.executable(self.bin / "cargo", '''#!/usr/bin/env python3
import json, os, pathlib, sys
with open(os.environ["CALLS"], "a") as stream:
    stream.write(json.dumps({"argv": sys.argv[1:], "public": os.environ.get("BUSBAR_RELEASE_PUBKEY")}) + "\\n")
if "tfacc_gate" in sys.argv and not os.environ.get("OMIT_LIBRARY"):
    (pathlib.Path(os.environ["CARGO_TARGET_DIR"]) / "release/examples/libtfacc_gate.so").write_text("fixture")
''')
        self.executable(self.target / "release/busbar-plugin-pack", '''#!/usr/bin/env python3
import os, pathlib, sys
if sys.argv[1] == "keygen":
    print("private key: " + ("bad" if os.environ.get("BAD_KEYS") else "a" * 64))
    print("public key: " + "b" * 64)
elif sys.argv[1] == "verify":
    assert sys.argv[sys.argv.index("--keys") + 1] == "b" * 64
    assert sys.argv[sys.argv.index("--name") + 1] == "tfacc-gate"
    if os.environ.get("BAD_VERIFY"):
        sys.exit(1)
    print("VERIFIED tfacc-gate")
else:
    assert os.environ["BUSBAR_SIGN_KEY"] == "a" * 64
    assert sys.argv[sys.argv.index("--kind") + 1] == "hook"
    assert "--allow-unsigned" not in sys.argv
    pathlib.Path(sys.argv[sys.argv.index("--out") + 1]).write_text("signed fixture")
''')
        self.executable(self.target / "release/busbar", '''#!/usr/bin/env bash
if [ "${1:-}" = --generate-signing-key ]; then echo test-key; exit; fi
exec sleep 120
''')

    @staticmethod
    def executable(path, content):
        path.write_text(content)
        path.chmod(0o755)

    def gateway(self, mode):
        return subprocess.run(["bash", str(HERE / "gateway.sh"), mode,
                               str(self.source), str(self.state)], env=self.env,
                              capture_output=True, text=True)

    def test_build_signs_fixture_for_same_keyed_default_distribution(self):
        result = self.gateway("build")
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        self.assertEqual(len(calls), 3)
        self.assertIn("busbar-plugin-loader", calls[0]["argv"])
        self.assertIn("pack", calls[0]["argv"])
        self.assertIn("dist/default/Cargo.toml", calls[1]["argv"])
        self.assertEqual(calls[1]["public"], "b" * 64)
        self.assertIn("busbar-contract", calls[2]["argv"])
        self.assertTrue(all("--locked" in call["argv"] for call in calls))
        self.assertTrue((self.state / "plugins/tfacc-gate.tar.gz").is_file())
        self.assertEqual((self.state / "release-public-key.txt").read_text(), "b" * 64 + "\n")
        self.assertEqual((self.state / "plugin-verification.txt").read_text(), "VERIFIED tfacc-gate\n")
        self.assertIn(str(HERE / "gate.rs"), self.manifest.read_text())
        self.assertEqual(self.gateway("build").returncode, 0)
        self.assertEqual(self.manifest.read_text().count('name = "tfacc_gate"'), 1)

    def test_build_rejects_missing_library_and_malformed_keys(self):
        for variable in ("OMIT_LIBRARY", "BAD_KEYS", "BAD_VERIFY"):
            with self.subTest(variable=variable):
                self.env[variable] = "1"
                self.assertNotEqual(self.gateway("build").returncode, 0)
                del self.env[variable]

    def test_unsupported_candidate_fails_without_fallback(self):
        self.manifest.unlink()
        self.assertNotEqual(self.gateway("build").returncode, 0)
        self.assertFalse((self.root / "calls").exists())

    def test_boot_requires_archive_and_checks_live_module(self):
        self.assertNotEqual(self.gateway("boot").returncode, 0)
        self.assertEqual(self.gateway("build").returncode, 0)
        for module, kind, health, expected in (
            ("different", "gate", {"reachable": True, "transport": {"kind": "plugin", "target": "tfacc-gate"}}, 1),
            ("tfacc-gate", "tap", {"reachable": True, "transport": {"kind": "plugin", "target": "tfacc-gate"}}, 1),
            ("tfacc-gate", "gate", {"reachable": False, "transport": {"kind": "plugin", "target": "tfacc-gate"}}, 1),
            ("tfacc-gate", "gate", {"transport": {"kind": "plugin", "target": "tfacc-gate"}}, 1),
            ("tfacc-gate", "gate", {"reachable": True, "transport": {"kind": "none", "target": "tfacc-gate"}}, 1),
            ("tfacc-gate", "gate", {"reachable": True, "transport": {"kind": "plugin", "target": "different"}}, 1),
            ("tfacc-gate", "gate", {"reachable": True, "transport": {"kind": "plugin", "target": "tfacc-gate"}}, 0),
        ):
            definition = {"kind": kind, "transport": {"target": module}}
            self.executable(self.bin / "curl", f'''#!/usr/bin/env python3
import sys
print({json.dumps(health)!r} if any(arg.endswith("/health") for arg in sys.argv) else {json.dumps(definition)!r})
''')
            try:
                result = self.gateway("boot")
                self.assertEqual(result.returncode, expected, result.stderr)
            finally:
                pid_file = self.state / "pid"
                if pid_file.exists():
                    try:
                        os.kill(int(pid_file.read_text()), signal.SIGTERM)
                    except ProcessLookupError:
                        pass
        env_lines = (self.root / "env").read_text().splitlines()
        self.assertEqual(len(env_lines[0].split("=", 1)[1]), 64)
        self.assertEqual(env_lines[0].split("=", 1)[0], "BUSBAR_ADMIN_TOKEN")
        self.assertEqual("\n".join(env_lines[1:]) + "\n", f"BUSBAR_TEST_CONFIG_FILE={self.state}/config.yaml\nBUSBAR_TEST_HOOK_MODULE=tfacc-gate\n")
        config = (self.state / "config.yaml").read_text()
        self.assertNotIn("allow_unsigned", config)
        self.assertFalse(json.loads(config)["admin_require_mtls"])
        self.assertEqual(json.loads(config)["auth"]["admin_auth"], ["admin-tokens"])
        self.assertEqual(json.loads(config)["providers_file"], str(self.state / "providers.json"))
        self.assertEqual(json.loads((self.state / "providers.json").read_text()),
                         {"anthropic": {"protocol": "anthropic", "base_url": "https://api.anthropic.com"}})

    def test_runner_rejects_skips_missing_module_and_failed_tests(self):
        for output, code, expected in (
            ("--- PASS: TestAccHookResource (0.1s)", 0, 0),
            ("--- SKIP: TestAccHookResource (0.1s)", 0, 1),
            ("--- PASS: TestAccHookResource (0.1s)\n--- SKIP: TestAccOther (0.1s)", 0, 1),
            ("--- PASS: TestAccOther (0.1s)", 0, 1),
            ("--- PASS: TestAccHookResource (0.1s)", 1, 1),
        ):
            self.executable(self.bin / "go", f"#!/bin/sh\ncat <<'EOF'\n{output}\nEOF\nexit {code}\n")
            result = subprocess.run(["bash", str(HERE / "test.sh")], env=self.env, capture_output=True)
            self.assertEqual(result.returncode, expected, output)
        self.env.pop("BUSBAR_TEST_HOOK_MODULE")
        result = subprocess.run(["bash", str(HERE / "test.sh")], env=self.env, capture_output=True)
        self.assertNotEqual(result.returncode, 0)

    def test_acceptance_hook_refuses_missing_required_configuration(self):
        hook = (REPO / "internal/provider/hook_resource_test.go").read_text()
        self.assertIn('if os.Getenv("TF_ACC") != "" {\n\t\t\tt.Fatal(', hook)


if __name__ == "__main__":
    unittest.main()
