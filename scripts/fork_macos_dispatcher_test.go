package scripts_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict on
// it). It holds the offline controls for scripts/ci/macos-go-test.py, the Go test
// dispatcher that fork-macos.yml runs (the M-rules guard that workflow in
// fork_macos_lane_pin_test.go). They lived in scripts/ci_workflow_test.go, a file
// upstream owns and rewrites on every sync, and moved here verbatim with the same
// function name (be-jvluf7 sections 6.2 and 9.7).
//
// One control did not move: the unittest method that ran the real
// scripts/ci/pr-core.sh with a fake Python dispatcher. The fork's edit of that
// script, which routed its go test through the dispatcher, is gone and the script
// is upstream's again, so no check of this file executes it (ratified drop,
// be-jvluf7 sections 6.4 and 9). The other nine methods read and run only
// macos-go-test.py.

// The same offline program can be run directly during CI-runner development.
// Its fake Go executable exercises dispatch and receipt handling, not storage.
const macOSDispatcherControls = `
import importlib.util, json, os, signal, socket, sys, tempfile, time, unittest
from pathlib import Path
from unittest import mock
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("macos_go", sys.argv.pop(1))
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)

FAKE = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
mode = os.environ.get("DISPATCH_CASE", "good")
graph = "github.com/steveyegge/beads/internal/storage/graphstore"
other = "github.com/steveyegge/beads/scripts"
args = sys.argv[1:]
with Path("calls.jsonl").open("a") as log: log.write(json.dumps(args) + "\n")
if args[0] == "list":
    print(other)
    if mode != "missing-package": print(graph)
    if mode == "duplicate-package": print(graph)
    sys.exit(0)
if "-list" in args:
    if mode == "discovery-failure": sys.exit(7)
    if mode == "empty": sys.exit(0)
    print("TestEmbeddedOld")
    if mode == "excluded-only": sys.exit(0)
    for name in ["TestZulu", "TestAlpha", "TestServer", "TestBeta", "TestNew", "ExampleValue", "FuzzSeeds"]: print(name)
    print("BenchmarkIgnored")
    if mode == "duplicate-name": print("TestNew")
    print("ok  " + graph + " 0.01s")
    sys.exit(0)
pkg = args[-1]
if (mode == "test-failure" and pkg == graph) or (mode == "other-failure" and pkg == other): sys.exit(9)
def event(action, name=None):
    print(json.dumps(dict(Action=action, Package=pkg, **({"Test":name} if name else {}))))
event("start")
if mode == "json-fail": event("fail")
if mode == "build-fail": event("build-fail")
if pkg == graph:
    names = args[args.index("-run")+1][2:-2].split("|")
    if mode == "missing-root": names = names[1:]
    if mode == "extra-root": names += ["TestUnexpected"]
    for name in names:
        event("run", name)
        event("run", name + "/child")
        if mode != "unfinished-child": event("pass", name + "/child")
        event("skip" if name == "TestServer" else "pass", name)
        if mode == "duplicate-terminal": event("pass", name)
    if mode == "bad-json": print("not json")
if mode != "missing-package-terminal" and not (mode == "other-missing-terminal" and pkg == other): event("pass")
'''

class DispatchControls(unittest.TestCase):
    def exercise(self, case):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); fake = root / "go"; fake.write_text(FAKE); fake.chmod(0o700)
            output = root / "evidence"
            with mock.patch.dict(os.environ, {"DISPATCH_CASE":case}):
                if case == "good": m.execute(root, output, str(fake))
                else:
                    with self.assertRaises((RuntimeError, ValueError)): m.execute(root, output, str(fake))
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(summary["passed"], case == "good")
            receipts = json.loads((output / "processes.json").read_text())
            self.assertTrue(all(r["groupGone"] for r in receipts))
            if case != "good": return
            plan = json.loads((output / "plan.json").read_text())
            self.assertEqual(plan["excluded"], ["TestEmbeddedOld"])
            expected = sorted(["TestZulu", "TestAlpha", "TestServer", "TestBeta", "TestNew", "ExampleValue", "FuzzSeeds"])
            self.assertEqual(sorted(sum(plan["groups"], [])), expected)
            self.assertEqual(len(sum(plan["groups"], [])), len(expected))
            self.assertEqual(plan["groups"], m.partition(list(reversed(plan["discovered"]))))
            self.assertEqual(len(summary["groups"]), 4)
            self.assertTrue(any(g["skipped"] for g in summary["groups"]))
            calls = [json.loads(line) for line in (root / "calls.jsonl").read_text().splitlines()]
            self.assertEqual(calls[0], ["list", "-race", "-tags", "gms_pure_go", "./..."])
            executions = [call for call in calls if call[0] == "test" and "-list" not in call]
            self.assertEqual(len(executions), 5)
            for call in executions:
                for flag in ["-race", "-short", "-timeout=30m", "-json"]: self.assertIn(flag, call)
                self.assertNotIn("-v", call)
                self.assertEqual(call[call.index("-tags")+1], "gms_pure_go")
                self.assertEqual(call[call.index("-skip")+1], "^TestEmbedded")
            self.assertEqual(executions[-1][-1], "github.com/steveyegge/beads/scripts")
            self.assertNotIn(m.GRAPHSTORE, executions[-1])
            for index, group in enumerate(plan["groups"], 1):
                census = json.loads((output / ("graphstore-%d-census.json" % index)).read_text())
                self.assertEqual(census["roots"], group)

    def test_pr_core_options_and_raw_json_compatibility(self):
        for case in ("good", "json-fail", "other-failure"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = Path(directory); fake = root / "go"; fake.write_text(FAKE); fake.chmod(0o700)
                output, combined = root / "evidence", root / "go-test.json"
                combined.write_text("stale output must be replaced")
                with mock.patch.dict(os.environ, {"DISPATCH_CASE": case}):
                    if case == "good":
                        m.execute(root, output, str(fake), package_parallel=3, test_parallel=2, json_output=combined)
                    else:
                        with self.assertRaises(RuntimeError):
                            m.execute(root, output, str(fake), package_parallel=3, test_parallel=2, json_output=combined)
                receipts = json.loads((output / "processes.json").read_text())
                executions = [r for r in receipts if "-json" in r["argv"]]
                actual = b"".join((output / (r["label"] + ".stdout")).read_bytes() for r in executions)
                self.assertEqual(combined.read_bytes(), actual)
                for r in executions:
                    args = r["argv"]
                    self.assertEqual(args[args.index("-p") + 1], "3")
                    self.assertEqual(args[args.index("-parallel") + 1], "2")
                    self.assertIn("-race", args); self.assertIn("-short", args)
                    self.assertIn("-timeout=30m", args); self.assertIn("^TestEmbedded", args)
                if case != "good":
                    self.assertFalse(json.loads((output / "summary.json").read_text())["passed"])
                    continue
                # Exercise the actual nightly consumer against the concatenated
                # named terminal events, including the permitted server skip.
                eq_path = Path(m.__file__).resolve().parents[2] / "tools/bazel/equivalence.py"
                eq_spec = importlib.util.spec_from_file_location("equivalence", eq_path)
                eq = importlib.util.module_from_spec(eq_spec); eq_spec.loader.exec_module(eq)
                statuses = eq.go_test_statuses(combined, {m.GRAPHSTORE: "internal/storage/graphstore", "github.com/steveyegge/beads/scripts": "scripts"})
                self.assertEqual(set(statuses["internal/storage/graphstore"]), {"TestZulu", "TestAlpha", "TestServer", "TestBeta", "TestNew", "ExampleValue", "FuzzSeeds"})
                self.assertEqual(statuses["internal/storage/graphstore"]["TestServer"], "skipped")
                self.assertEqual(statuses["internal/storage/graphstore"]["TestNew"], "passed")
                self.assertTrue(all(r["groupGone"] for r in receipts))

    def test_real_go_json_preserves_unterminated_output(self):
        # Go's -json supplies framed verbose output unless explicit -v overrides
        # it. Without framing an application fragment can swallow RUN/PASS lines.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "go.mod").write_text("module example.invalid/dispatch-framing\ngo 1.26\n")
            (root / "framing_test.go").write_text('package framing\nimport ("fmt"; "os"; "testing")\nfunc TestFragment(t *testing.T) { fmt.Fprint(os.Stderr, "before-child"); t.Run("child", func(t *testing.T) { fmt.Print("child-tail") }); fmt.Fprint(os.Stderr, "parent-tail") }\n')
            runner = m.Runner(root, root)
            events = runner.run("framing", ["go", "test", *m.JSON_FLAGS, "-json", "-count=1", "."], 30)
            census = m.verify_events(events, ["example.invalid/dispatch-framing"], ["TestFragment"])
            self.assertEqual(census["tests"], 2)
            self.assertEqual(census["skipped"], [])

    def test_complete_partition_and_existing_skip(self): self.exercise("good")
    def test_negative_receipts(self):
        for case in ["missing-package", "duplicate-package", "discovery-failure", "empty", "excluded-only",
                     "duplicate-name", "test-failure", "other-failure", "missing-root", "extra-root",
                     "unfinished-child", "duplicate-terminal", "bad-json", "json-fail", "build-fail", "missing-package-terminal", "other-missing-terminal"]:
            with self.subTest(case=case): self.exercise(case)
    def test_partition_refuses_empty_or_duplicate(self):
        for names in [[], ["TestA", "TestA"], ["TestEmbeddedOnly"]]:
            with self.assertRaises(RuntimeError): m.partition(names)
    def test_unexpected_package_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events"
            path.write_text(json.dumps(dict(Action="pass", Package="wrong")) + "\n")
            with self.assertRaises(RuntimeError): m.verify_events(path, [m.GRAPHSTORE])
    def test_total_deadline_refuses_spawn(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); runner = m.Runner(root, root); runner.deadline = 0
            with self.assertRaises(RuntimeError): runner.run("expired", [sys.executable, "-c", "raise SystemExit(0)"], 1)
            self.assertEqual(runner.receipts, [])
    def test_interrupt_during_spawn_retains_ownership(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); runner = m.Runner(root, root)
            original_popen = m.subprocess.Popen
            def interrupted_spawn(*args, **kwargs):
                child = original_popen(*args, **kwargs)
                deadline = time.monotonic() + 3
                while not (root / "ready").exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                os.kill(os.getpid(), signal.SIGTERM)
                return child
            previous = signal.signal(signal.SIGTERM, m.interrupted)
            try:
                with mock.patch.object(m.subprocess, "Popen", interrupted_spawn):
                    with self.assertRaisesRegex(RuntimeError, "interrupted by signal"):
                        runner.run("interrupt", [sys.executable, "-c", "import time; from pathlib import Path; Path('ready').touch(); time.sleep(60)"], 5)
            finally:
                signal.signal(signal.SIGTERM, previous)
            receipt = json.loads((root / "processes.json").read_text())[0]
            self.assertTrue(receipt["groupGone"]); self.assertIsNotNone(receipt["exitCode"])
            self.assertFalse(m.group_alive(receipt["pid"]))

    def test_real_timeout_reaps_owner_and_closes_listener(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); ready = root / "ready"
            code = "import json,os,signal,socket,time; from pathlib import Path; s=socket.socket(); s.bind(('127.0.0.1',0)); s.listen(); signal.signal(signal.SIGTERM,signal.SIG_IGN); Path('ready').write_text(json.dumps({'pid':os.getpid(),'port':s.getsockname()[1]})); time.sleep(60)"
            runner = m.Runner(root, root)
            with mock.patch.object(m, "CLEANUP_SECONDS", 0.1):
                with self.assertRaises(m.subprocess.TimeoutExpired): runner.run("timeout", [sys.executable, "-c", code], 0.5)
            child = json.loads(ready.read_text())
            self.assertFalse(m.group_alive(child["pid"]))
            with socket.socket() as probe: self.assertNotEqual(probe.connect_ex(("127.0.0.1", child["port"])), 0)
            receipt = json.loads((root / "processes.json").read_text())[0]
            self.assertTrue(receipt["groupGone"]); self.assertIsNotNone(receipt["exitCode"])
            self.assertIn("timed out", receipt["failure"])

unittest.main()
`

func TestMacOSGoTestDispatcherControls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS dispatcher owns POSIX process groups")
	}
	python := requireHostTool(t, "python3")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", macOSDispatcherControls,
		filepath.Join(sourceRepoRoot(t), "scripts", "ci", "macos-go-test.py"))
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline macOS dispatch controls: %v\n%s", err, output)
	}
}
