#!/usr/bin/env python3
"""Negative controls for the required Go-test receipt gate and the leg aggregator; no engine needed."""
import ast
import contextlib
import hashlib
import importlib.util
import io
import json
import os
import re
import shutil
import signal
import socket
import tempfile
import time
import subprocess
import sys
import threading
from pathlib import Path
import unittest
from unittest import mock

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location("graph_c0", Path(__file__).with_name("graph-c0-qualify.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def event(action, test=None):
    return dict(Action=action, **({"Test": test} if test else {}))


def go_events(names):
    events = []
    for name in names:
        events += [event("run", name), event("pass", name)]
    return ("\n".join(json.dumps(item) for item in events + [event("pass")]) + "\n").encode()


def receipt(label, seconds, roots=None):
    return dict(label=label, elapsedSeconds=seconds, **({} if roots is None else dict(roots=roots)))


def runner(directory):
    # Qualification.run needs only these attributes: no Go, Dolt or network.
    root = Path(directory)
    q = module.Qualification.__new__(module.Qualification)
    q.root, q.output, q.env = root, root, dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    q.children, q.receipts = set(), []
    return q


def without_server(label):
    # Stands in for Qualification.server wherever a test drives tests() without Dolt.
    return contextlib.nullcontext()


SOURCE = Path(__file__).with_name("graph-c0-qualify.py").read_text()


def lane_function(name):
    return next(n for n in ast.walk(ast.parse(SOURCE)) if isinstance(n, ast.FunctionDef) and n.name == name)


def self_calls(function, method):
    """Every self.<method>(...) call inside the named lane function, in source order."""
    return sorted((c for c in ast.walk(lane_function(function)) if isinstance(c, ast.Call)
                   and isinstance(c.func, ast.Attribute) and isinstance(c.func.value, ast.Name)
                   and c.func.value.id == "self" and c.func.attr == method),
                  key=lambda c: c.lineno)


def resolve(node):
    """A lane call's argument: a literal, or a name from the qualifier's constants block."""
    return getattr(module, node.id) if isinstance(node, ast.Name) else ast.literal_eval(node)


WORKFLOW = Path(__file__).resolve().parents[2] / ".github/workflows/fork-qualification.yml"


def workflow_jobs():
    """fork-qualification.yml's jobs as {id: their lines}. Plain text on purpose: the lane that runs this file has no YAML dependency."""
    lines = WORKFLOW.read_text().splitlines()
    first = lines.index("jobs:") + 1
    headers = [i for i in range(first, len(lines)) if re.fullmatch(r"  [\w-]+:\s*", lines[i])]
    return {lines[i].strip()[:-1]: lines[i:j] for i, j in zip(headers, headers[1:] + [len(lines)])}


def job_value(lines, key):
    """The value of a job's one `key:` line at job level, or None."""
    found = [m.group(1) for m in (re.fullmatch(rf"    {key}:\s*(.*?)\s*", line) for line in lines) if m]
    return found[0] if len(found) == 1 else None


def job_steps(lines):
    """A job's steps as {uses, name, if, run, with: {key: value}}; a `|` block value is its lines joined by newlines.
    A one-line `run: command` is that command."""
    starts = [i for i, line in enumerate(lines) if line.startswith("      - ")]
    steps = []
    for first, last in zip(starts, starts[1:] + [len(lines)]):
        step, section, key = {"uses": "", "name": "", "if": None, "run": "", "with": {}}, None, None
        for line in ["        " + lines[first][8:], *lines[first + 1:last]]:
            text, indent = line.strip(), len(line) - len(line.lstrip())
            if not text or text.startswith("#"):
                continue
            if indent == 8:
                section, key = text.split(":")[0], None
                if section in ("uses", "name", "if"):
                    step[section] = re.sub(r"\s+#.*$", "", text.partition(":")[2].strip())
                elif section == "run" and text.partition(":")[2].strip() not in ("", "|"):
                    step["run"] = text.partition(":")[2].strip() + "\n"
            elif section == "with" and indent == 10:
                key, _, value = text.partition(":")
                step["with"][key] = value.strip()
            elif section == "with" and key:
                step["with"][key] = (step["with"][key].lstrip("|") + "\n" + text).strip()
            elif section == "run":
                step["run"] += line + "\n"
        steps.append(step)
    return steps


class RequiredReceipts(unittest.TestCase):
    def setUp(self):
        self.good = [event("run", "TestRequired"), event("run", "TestRequired/child"),
                     event("pass", "TestRequired/child"), event("pass", "TestRequired"), event("pass")]

    def test_complete(self):
        self.assertEqual(module.verify_tests(self.good, {"TestRequired"}), 1)

    def test_no_tests(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests([event("pass")], set())

    def test_skipped_subtest(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good + [event("skip", "TestRequired/optional")], {"TestRequired"})

    def test_missing_subtest_pass(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good + [event("run", "TestRequired/unfinished")], {"TestRequired"})

    def test_extra_subtest_pass(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good + [event("pass", "TestRequired/never-started")], {"TestRequired"})

    def test_extra_subtest_root(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good + [event("run", "TestOther/child"), event("pass", "TestOther/child")], {"TestRequired"})

    def test_duplicate_subtest_pass(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good + [event("pass", "TestRequired/child")], {"TestRequired"})

    def test_missing_new_test(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good, {"TestRequired", "TestNew"})

    def test_truncated_package(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good[:-1], {"TestRequired"})

    def test_failed_package(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests(self.good + [event("fail")], {"TestRequired"})

    def test_cached_without_execution(self):
        with self.assertRaises(RuntimeError):
            module.verify_tests([event("pass", "TestRequired"), event("pass")], {"TestRequired"})


class RequiredEngineControls(unittest.TestCase):
    def exercise(self, engines):
        # Drive the complete discovery/execution gate with a source root and
        # valid complete package receipts. A missing engine never emits a skip,
        # so ordinary run/pass equality alone cannot catch the omission.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "workflow_test.go"
            source.write_text("func TestWorkflow(t *testing.T) {}\n")
            events = [event("run", "TestWorkflow")]
            for engine in engines:
                events += [event("run", "TestWorkflow/" + engine), event("pass", "TestWorkflow/" + engine)]
            events += [event("pass", "TestWorkflow"), event("pass")]
            q = module.Qualification.__new__(module.Qualification)
            q.output, q.counts = root, {}
            q.server = without_server
            listing = b"TestWorkflow\n"
            execution = "\n".join(json.dumps(item) for item in events).encode()
            with mock.patch.object(q, "run", side_effect=[listing, execution]):
                q.tests("./fixture", "^TestWorkflow", [source], "cli",
                        ("TestWorkflow/embedded", "TestWorkflow/server"))
            self.assertEqual(q.counts, {"cli": 1})

    def test_both_installed_engines_complete(self):
        self.exercise(("embedded", "server"))

    def test_missing_installed_engine_refused(self):
        for engines in [("embedded",), ("server",), ()]:
            with self.subTest(engines=engines), self.assertRaisesRegex(RuntimeError, "required engine proof absent"):
                self.exercise(engines)


class RequiredGroupedExecution(unittest.TestCase):
    def exercise(self, mode):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            names = ["Test" + letter for letter in "ABCDEFGH"]
            source = root / "storage_test.go"
            source.write_text("\n".join("func " + name + "(t *testing.T) {}" for name in names))
            q = module.Qualification.__new__(module.Qualification)
            q.output, q.counts = root, {}
            q.server = without_server
            calls = []
            def run(args, label, **kwargs):
                calls.append((args, label, kwargs))
                if "-list" in args:
                    listed = names + ([names[0]] if mode == "duplicate-discovery" else [])
                    return ("\n".join(listed) + "\n").encode()
                selected = args[args.index("-run") + 1][2:-2].split("|")
                if mode == "missing": selected = selected[1:]
                if mode == "duplicate": selected += selected[:1]
                events = []
                for name in selected:
                    events += [event("run", name), event("run", name + "/embedded"), event("pass", name + "/embedded"), event("pass", name)]
                events += [event("fail" if mode == "failure" else "pass")]
                raw = ("\n".join(json.dumps(item) for item in events) + "\n").encode()
                (root / (label + ".stdout")).write_bytes(raw)
                (root / (label + ".stderr")).write_bytes(b"")
                if mode == "timeout": raise subprocess.TimeoutExpired(args, 960)
                return raw
            with mock.patch.object(q, "run", run):
                if mode == "good":
                    q.tests("./fixture", "^Test", [source], "storage", groups=4)
                else:
                    with self.assertRaises((RuntimeError, subprocess.TimeoutExpired)):
                        q.tests("./fixture", "^Test", [source], "storage", groups=4)
            if mode == "duplicate-discovery":
                self.assertEqual(len(calls), 1)
                return
            plan = json.loads((root / "storage-groups.json").read_text())
            self.assertEqual(plan["complete"], mode == "good")
            self.assertEqual(plan["sourceRoots"], sorted(names))
            self.assertEqual(plan["compiledRoots"], sorted(names))
            self.assertEqual(len(plan["groups"]), 4)
            parts = [root / (group["label"] + ".stdout") for group in plan["groups"]]
            self.assertEqual((root / "storage.stdout").read_bytes(), b"".join(path.read_bytes() for path in parts if path.exists()))
            for args, label, kwargs in calls[1:]:
                self.assertIn("-count=1", args)
                self.assertIn("-timeout=15m", args)
                self.assertIn("-p=1", args)
                self.assertIn("-parallel=1", args)
                self.assertEqual(kwargs["timeout"], 960)
            if mode == "good":
                self.assertEqual(q.counts, {"storage": 8})
                self.assertTrue(all(group["passed"] for group in plan["groups"]))
                self.assertEqual(len(calls), 5)
                self.assertEqual(module.verify_tests([json.loads(line) for line in (root / "storage.stdout").read_bytes().splitlines()], set(names)), 8)
                self.assertEqual(plan["stdoutSHA256"], module.digest(root / "storage.stdout"))
            else:
                self.assertEqual(q.counts, {})
                self.assertFalse(any(group["passed"] for group in plan["groups"]))

    def test_exhaustive_groups_and_actual_concatenation(self): self.exercise("good")

    def test_missing_duplicate_failed_or_timed_out_group_refused(self):
        for mode in ["missing", "duplicate", "failure", "timeout", "duplicate-discovery"]:
            with self.subTest(mode=mode): self.exercise(mode)

    def test_zero_duplicate_and_empty_group_selection_refused(self):
        for names, groups in [([], 4), (["TestA", "TestA"], 2), (["TestA"], 4)]:
            with self.subTest(names=names), self.assertRaises(RuntimeError):
                module.partition_required_tests(names, groups)
        names = ["Test" + letter for letter in "ABCDEFGH"]
        self.assertEqual(module.partition_required_tests(names, 4), module.partition_required_tests(list(reversed(names)), 4))


class RequiredDiscovery(unittest.TestCase):
    # Exercise the actual source/compiled/execution boundary with temporary Go
    # source. Only subprocess output is supplied; no engine or Go build is needed.
    def exercise(self, source, compiled, events):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "patch_test.go").write_text(source)
            q = module.Qualification.__new__(module.Qualification)
            q.output, q.counts = root, {}
            q.server = without_server
            calls = []

            def run(argv, label, **kwargs):
                calls.append((argv, label))
                if label == "graphpatch-discovery":
                    return compiled.encode()
                self.assertEqual(label, "graphpatch")
                return "\n".join(json.dumps(e) for e in events).encode()

            q.run = run
            q.tests("./internal/graphpatch", "^Test", root.glob("*_test.go"), "graphpatch")
            self.assertEqual(q.counts, {"graphpatch": 1})
            self.assertEqual([label for _, label in calls], ["graphpatch-discovery", "graphpatch"])
            self.assertTrue(all(argv[-1] == "./internal/graphpatch" for argv, _ in calls))
            self.assertIn("-count=1", calls[1][0])

    def test_required_package_discovery_and_execution(self):
        self.exercise("package graphpatch\nfunc TestPatch(t *testing.T) {}\n", "TestPatch\n",
                      [event("run", "TestPatch"), event("run", "TestPatch/child"),
                       event("pass", "TestPatch/child"), event("pass", "TestPatch"), event("pass")])

    def test_omitted_or_unexecuted_required_source_refuses(self):
        source = "package graphpatch\nfunc TestPatch(t *testing.T) {}\n"
        good = [event("run", "TestPatch"), event("pass", "TestPatch"), event("pass")]
        for name, text, compiled, events in [
            ("empty-source", "package graphpatch\n", "", [event("pass")]),
            ("compiled-excludes-source", source, "", good),
            ("source-glob-omits-compiled-root", "package graphpatch\n", "TestPatch\n", good),
            ("compiled-adds-unregistered-root", source, "TestPatch\nTestOther\n", good),
            ("discovered-but-not-executed", source, "TestPatch\n", [event("pass")]),
            ("discovered-but-skipped", source, "TestPatch\n", [event("skip", "TestPatch"), event("pass")]),
        ]:
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                self.exercise(text, compiled, events)


class HTTPReceiptControls(unittest.TestCase):
    def test_clean_listener(self):
        module.verify_clean_receipt(dict(exit_code=0, failure=None, forced=False,
                                         group_gone=True, port_closed=True), listener=True)

    def test_forced_or_incomplete_cleanup_refused(self):
        good = dict(exit_code=0, failure=None, forced=False, group_gone=True, port_closed=True)
        for change in [dict(forced=True), dict(group_gone=False), dict(port_closed=False),
                       dict(exit_code=1), dict(failure="timeout")]:
            with self.subTest(change=change), self.assertRaises(RuntimeError):
                module.verify_clean_receipt({**good, **change}, listener=True)

    def test_missing_cleanup_field_refused(self):
        with self.assertRaises(RuntimeError):
            module.verify_clean_receipt(dict(exit_code=0, failure=None), listener=True)


class ProcessCleanup(unittest.TestCase):
    def test_live_group_is_not_clean(self):
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], start_new_session=True)
        try:
            self.assertFalse(module.group_exited(child.pid, timeout=0.01))
        finally:
            child.kill()
            child.wait(timeout=5)
        self.assertTrue(module.group_exited(child.pid, timeout=0.01))

    def test_exiting_group_gets_bounded_grace(self):
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(0.15)"], start_new_session=True)
        reaper = threading.Thread(target=child.wait)
        reaper.start()
        try:
            self.assertTrue(module.group_exited(child.pid, timeout=5))
        finally:
            if child.poll() is None:
                child.kill()
            reaper.join(timeout=5)
        self.assertFalse(reaper.is_alive())


class NestedOwnershipControls(unittest.TestCase):
    # Real independent process group + bound socket, using the transferred
    # capture's Process and registry. No database or mocked process receipt.
    OWNER = r'''
import argparse, importlib.util, json, signal, sys, time
from pathlib import Path
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("smoke", sys.argv[1])
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)
registry, ready, output = map(Path, sys.argv[2:5])
args = argparse.Namespace(output_dir=output, bd=Path(sys.executable), server_port=0,
    server_root=None, total_timeout=120, command_timeout=60, owned_groups=registry)
capture = smoke.c0.Capture(args)
def interrupted(*_):
    raise KeyboardInterrupt("test outer interruption")
signal.signal(signal.SIGTERM, interrupted)
child_code = """import json, os, signal, socket, sys
from pathlib import Path
sock=socket.socket(); sock.bind(('127.0.0.1',0)); sock.listen()
def stop(*_):
    sock.close(); sys.exit(0)
signal.signal(signal.SIGINT, stop); signal.signal(signal.SIGTERM, stop)
Path(sys.argv[1]).write_text(json.dumps({'pid':os.getpid(),'port':sock.getsockname()[1]}))
signal.pause()
"""
process = None
try:
    process = smoke.Process(capture, "listener", [sys.executable, "-c", child_code, str(ready)])
    while not ready.exists():
        time.sleep(0.01)
    process.port = json.loads(ready.read_text())["port"]
    ready.with_suffix(".owner").write_text("ready")
    time.sleep(60)
except KeyboardInterrupt:
    pass
finally:
    if process:
        process.close()
    if capture.active:
        capture.stop()
'''

    def exercise(self, interrupt):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            owner, registry, ready = root / "owner.py", root / "groups.jsonl", root / "ready.json"
            owner.write_text(self.OWNER)
            q = module.Qualification.__new__(module.Qualification)
            q.root, q.output, q.env = root, root, dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
            q.children, q.receipts = set(), []
            argv = [sys.executable, str(owner), str(Path(__file__).resolve().parents[1] / "graph-bdp-read-smoke.py"),
                    str(registry), str(ready), str(root / "capture")]
            thread = None
            previous = None
            if interrupt:
                def receive(*_):
                    raise RuntimeError("real signal interruption control")
                previous = signal.signal(signal.SIGUSR1, receive)
                def send_when_listening():
                    deadline = time.monotonic() + 5
                    while not ready.with_suffix(".owner").exists() and time.monotonic() < deadline:
                        time.sleep(0.01)
                    if ready.with_suffix(".owner").exists():
                        os.kill(os.getpid(), signal.SIGUSR1)
                thread = threading.Thread(target=send_when_listening)
                thread.start()
            try:
                with self.assertRaises(RuntimeError if interrupt else subprocess.TimeoutExpired):
                    q.run(argv, "nested", timeout=10 if interrupt else 3, owned_groups=registry)
                self.assertTrue(ready.exists(), "real listener never started")
                info = json.loads(ready.read_text())
                self.assertTrue(module.group_exited(info["pid"], timeout=0.1))
                with self.assertRaises(ConnectionRefusedError):
                    socket.create_connection(("127.0.0.1", info["port"]), timeout=1)
                self.assertEqual(q.children, set())
                self.assertEqual(q.receipts[-1]["failure"], "RuntimeError" if interrupt else "TimeoutExpired")
                self.assertFalse(q.receipts[-1]["forcedOwner"])
                receipt = json.loads((root / "capture/listener/receipt.json").read_text())
                module.verify_clean_receipt(receipt, listener=True)
            finally:
                if thread:
                    thread.join(timeout=6)
                if previous is not None:
                    signal.signal(signal.SIGUSR1, previous)
                # Failures must not leave the negative control itself alive.
                module.cleanup_registered_groups(registry)

    def test_outer_timeout_drains_independent_listener(self):
        self.exercise(interrupt=False)

    def test_outer_interruption_drains_independent_listener(self):
        self.exercise(interrupt=True)

    def test_registry_failure_cannot_leave_unresponsive_owner_alive(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            owner, registry, ready = root / "owner.py", root / "groups.jsonl", root / "ready.json"
            # Ignore outer termination but reap real children when they stop.
            # This isolates an unresponsive owner from orphan-zombie behavior.
            source = self.OWNER.replace("import argparse, importlib.util, json, signal, sys, time",
                                        "import argparse, importlib.util, json, os, signal, sys, time")
            source = source.replace("signal.signal(signal.SIGTERM, interrupted)",
                "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
                "def reap(*_):\n"
                "    try:\n"
                "        while os.waitpid(-1, os.WNOHANG)[0]: pass\n"
                "    except ChildProcessError: pass\n"
                "signal.signal(signal.SIGCHLD, reap)")
            owner.write_text(source)
            q = module.Qualification.__new__(module.Qualification)
            q.root, q.output, q.env = root, root, dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
            q.children, q.receipts = set(), []
            argv = [sys.executable, str(owner), str(Path(__file__).resolve().parents[1] / "graph-bdp-read-smoke.py"),
                    str(registry), str(ready), str(root / "capture")]
            actual_cleanup = module.cleanup_registered_groups
            calls = []
            def fail_first_registry_pass(path):
                result = actual_cleanup(path)
                calls.append(result)
                if len(calls) == 1:
                    raise RuntimeError("injected registry failure after real child cleanup")
                return result
            try:
                # Only the grace duration and injected registry error vary;
                # owner, independent listener, sockets and signals are real.
                with mock.patch.object(module, "OWNER_CLEANUP_GRACE", 0.2), \
                     mock.patch.object(module, "cleanup_registered_groups", fail_first_registry_pass), \
                     self.assertRaises(subprocess.TimeoutExpired):
                    q.run(argv, "forced-owner", timeout=3, owned_groups=registry)
                info = json.loads(ready.read_text())
                receipt = q.receipts[-1]
                self.assertTrue(module.group_exited(receipt["pid"], timeout=0.1))
                self.assertTrue(module.group_exited(info["pid"], timeout=0.1))
                with self.assertRaises(ConnectionRefusedError):
                    socket.create_connection(("127.0.0.1", info["port"]), timeout=1)
                self.assertEqual(receipt["failure"], "TimeoutExpired")
                self.assertTrue(receipt["forcedOwner"])
                self.assertEqual(receipt["exit"], -signal.SIGKILL)
                self.assertTrue(any("injected registry failure" in error for error in receipt["cleanupErrors"]))
                self.assertGreaterEqual(len(calls), 2)
                self.assertTrue(receipt["nestedCleanup"]["groupsGone"])
                self.assertEqual(q.children, set())
            finally:
                actual_cleanup(registry)


class LaneShape(unittest.TestCase):
    # The cmd/bd groups run in the rest leg (be-vm7gou, ruling be-jmdn71 G1), so this pin reads that leg's entry point.
    def test_cmd_bd_graph_tests_run_as_three_groups(self):
        [call] = [c for c in self_calls("execute_rest", "tests") if resolve(c.args[0]) == "./cmd/bd"]
        self.assertEqual(resolve(call.args[1]), "^Test(GraphModeCLI|GraphPreview)")
        self.assertEqual(resolve(call.args[3]), "cli")
        self.assertEqual(resolve(call.args[4]), module.CLI_REQUIRED)
        self.assertEqual({k.arg: resolve(k.value) for k in call.keywords}, {"groups": 3})

    def test_rest_leg_runs_every_label_but_storage_and_in_the_single_lane_order(self):
        labels = [resolve(c.args[3]) for c in self_calls("execute_rest", "tests")]
        self.assertEqual(labels, list(module.REST_LABELS))
        self.assertNotIn("storage", labels)
        self.assertEqual(labels, [label for label in module.PASSED_LABELS if label != "storage"])

    def test_storage_leg_runs_four_groups_of_graphstore_through_the_leg_pairing(self):
        [call] = self_calls("execute_storage", "tests")
        self.assertEqual(resolve(call.args[0]), "./internal/storage/graphstore")
        self.assertEqual(resolve(call.args[1]), "^Test")
        self.assertEqual(resolve(call.args[3]), "storage")
        self.assertEqual(resolve(call.args[4]), module.STORAGE_REQUIRED)
        keywords = {k.arg: k.value for k in call.keywords}
        self.assertEqual(set(keywords), {"groups", "run_groups"})
        self.assertEqual(resolve(keywords["groups"]), 4)
        self.assertEqual(ast.unparse(keywords["run_groups"]), "groups", "the leg must run exactly the groups it was given")

    def test_execute_dispatches_each_leg_to_its_entry_point_and_runs_nothing_itself(self):
        self.assertEqual(self_calls("execute", "tests"), [])
        self.assertEqual([ast.unparse(c) for c in self_calls("execute", "execute_rest")], ["self.execute_rest()"])
        self.assertEqual([ast.unparse(c) for c in self_calls("execute", "execute_storage")], ["self.execute_storage(STORAGE_LEGS[leg])"])


class ReceiptTiming(unittest.TestCase):
    def test_every_path_records_elapsed_seconds(self):
        for name, code, timeout, failure in [
                ("success", "pass", 30, None),
                ("failed-exit", "raise SystemExit(3)", 30, RuntimeError),
                ("timed-out", "import time; time.sleep(60)", 0.5, subprocess.TimeoutExpired)]:
            with self.subTest(path=name), tempfile.TemporaryDirectory() as directory:
                q = runner(directory)
                argv = [sys.executable, "-c", code]
                if failure:
                    with self.assertRaises(failure):
                        q.run(argv, name, timeout=timeout)
                else:
                    q.run(argv, name, timeout=timeout)
                self.assertEqual(q.receipts[-1]["label"], name)
                self.assertIn("elapsedSeconds", q.receipts[-1])
                self.assertGreaterEqual(q.receipts[-1]["elapsedSeconds"], 0.5 if name == "timed-out" else 0)


class PhaseReport(unittest.TestCase):
    def test_go_test_runs_carry_their_root_count_but_discovery_does_not(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            names = ["Test" + letter for letter in "ABCDEFGH"]
            source = root / "storage_test.go"
            source.write_text("\n".join("func " + name + "(t *testing.T) {}" for name in names))
            for groups, expected in [(4, [("storage-group-%d" % n, 2) for n in range(1, 5)]), (1, [("storage", 8)])]:
                with self.subTest(groups=groups):
                    q = module.Qualification.__new__(module.Qualification)
                    q.output, q.counts = root, {}
                    q.server = without_server
                    seen = []

                    def run(args, label, **kwargs):
                        seen.append((label, kwargs.get("roots")))
                        if "-list" in args:
                            return ("\n".join(names) + "\n").encode()
                        selected = args[args.index("-run") + 1][2:-2].split("|") if groups > 1 else names
                        raw = go_events(selected)
                        (root / (label + ".stdout")).write_bytes(raw)
                        (root / (label + ".stderr")).write_bytes(b"")
                        return raw

                    with mock.patch.object(q, "run", run):
                        q.tests("./fixture", "^Test", [source], "storage", groups=groups)
                    self.assertEqual(seen, [("storage-discovery", None)] + expected)

    def test_table_has_a_row_per_receipt_with_roots_seconds_and_share_of_timeout(self):
        table = module.phase_table([receipt("cli-discovery", 21.4), receipt("cli-group-1", 181.3, roots=44),
                                    receipt("server-init", 12.0)])
        rows = [[cell.strip() for cell in line.strip("|").split("|")] for line in table.splitlines() if line.startswith("| ")]
        self.assertEqual(rows, [["Phase", "Roots", "Seconds", "% of go test timeout"],
                                ["cli-discovery", "", "21.4", ""],
                                ["cli-group-1", "44", "181.3", "20%"],
                                ["server-init", "", "12.0", ""]])

    def test_group_at_sixty_percent_of_its_timeout_warns_and_nothing_else_does(self):
        self.assertEqual(module.slow_group_warnings([receipt("cli-group-1", 539.9, roots=44),
                                                     receipt("server-init", 600.0)]), [])
        [warning] = module.slow_group_warnings([receipt("cli-discovery", 30.0), receipt("cli-group-2", 540.0, roots=44)])
        self.assertTrue(warning.startswith("::warning"))
        self.assertIn("cli-group-2", warning)
        self.assertIn("60%", warning)

    def test_failed_run_still_publishes_the_table_and_the_warning(self):
        class Failing:
            def __init__(self, args):
                self.output = Path(args.output)
                self.output.mkdir()
                self.receipts = [receipt("cli-group-1", 600.0, roots=44)]

            def execute(self, leg):
                raise RuntimeError("group failed")

        flags = {"rest": ("bd", "dolt", "artifacts", "output", "bdp-checkout", "client-manifest", "node"),
                 "storage-a": ("bd", "dolt", "artifacts", "output")}
        for leg, names in flags.items():
            with self.subTest(leg=leg), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                argv = ["graph-c0-qualify.py", "--leg", leg] + [part for flag in names for part in ("--" + flag, str(root / flag))]
                summary = root / "step-summary.md"
                handlers = {number: signal.getsignal(number) for number in (signal.SIGTERM, signal.SIGINT)}
                try:
                    with mock.patch.object(module, "Qualification", Failing), mock.patch.object(sys, "argv", argv), \
                         mock.patch.dict(os.environ, {"GITHUB_STEP_SUMMARY": str(summary)}), \
                         contextlib.redirect_stdout(io.StringIO()) as log, self.assertRaisesRegex(RuntimeError, "group failed"):
                        module.main()
                finally:
                    for number, handler in handlers.items():
                        signal.signal(number, handler)
                self.assertTrue((root / "output" / "receipts.json").exists())
                self.assertTrue(summary.exists(), "no step summary was written for the failed run")
                self.assertIn("cli-group-1", summary.read_text())
                self.assertIn("::warning", log.getvalue())


class FailureTail(unittest.TestCase):
    def test_tail_is_the_last_forty_lines_of_test_output_and_of_stderr(self):
        stdout = b"".join(json.dumps(dict(Action="output", Test="TestA", Output=f"out {n}\n")).encode() + b"\n"
                          for n in range(100)) + json.dumps(event("fail")).encode() + b"\n"
        stderr = "".join(f"err {n}\n" for n in range(100)).encode()
        self.assertEqual(module.output_tail(stdout, stderr),
                         ([f"out {n}" for n in range(60, 100)], [f"err {n}" for n in range(60, 100)]))

    def test_output_that_is_not_a_go_test_event_is_kept_as_it_is(self):
        stdout = (b'{"error": "init failed"}\n'
                  + json.dumps(dict(Action="output", Test="TestA", Output="boom\n")).encode() + b"\n"
                  + json.dumps(event("run", "TestA")).encode() + b"\n")
        self.assertEqual(module.output_tail(stdout, b""), (['{"error": "init failed"}', "boom"], []))

    def test_failed_or_timed_out_command_prints_its_tail_into_the_log(self):
        code = ("import sys, time\n"
                "for n in range(100):\n"
                "    print('out', n, flush=True)\n"
                "    print('err', n, file=sys.stderr, flush=True)\n"
                "if sys.argv[1] == 'hang':\n"
                "    time.sleep(60)\n"
                "sys.exit(1)\n")
        for mode, failure, timeout in [("exit", RuntimeError, 30), ("hang", subprocess.TimeoutExpired, 3)]:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory, \
                 contextlib.redirect_stdout(io.StringIO()) as log:
                with self.assertRaises(failure):
                    runner(directory).run([sys.executable, "-c", code, mode], "failing", timeout=timeout)
            text = log.getvalue()
            for line in ("out 99\n", "err 99\n", "out 60\n", "err 60\n"):
                self.assertIn(line, text)
            for line in ("out 59\n", "err 59\n"):
                self.assertNotIn(line, text)


class ServerPerGroup(unittest.TestCase):
    NAMES = ["Test" + letter for letter in "ABCDEFGH"]

    def trace(self, groups, label, failing_run=None):
        # Records where each server context opens and closes relative to every
        # command tests() runs; no Dolt and no Go.
        steps = []
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "storage_test.go"
            source.write_text("\n".join("func " + name + "(t *testing.T) {}" for name in self.NAMES))
            q = module.Qualification.__new__(module.Qualification)
            q.output, q.counts = root, {}

            @contextlib.contextmanager
            def server(server_label):
                steps.append(("open", server_label))
                try:
                    yield 0
                finally:
                    steps.append(("close", server_label))

            def run(args, run_label, **kwargs):
                steps.append(("run", run_label))
                if "-list" in args:
                    return ("\n".join(self.NAMES) + "\n").encode()
                if run_label == failing_run:
                    raise subprocess.TimeoutExpired(args, 960)
                selected = args[args.index("-run") + 1][2:-2].split("|") if groups > 1 else self.NAMES
                raw = go_events(selected)
                (root / (run_label + ".stdout")).write_bytes(raw)
                (root / (run_label + ".stderr")).write_bytes(b"")
                return raw

            q.server = server
            with mock.patch.object(q, "run", run):
                if failing_run:
                    with self.assertRaises(subprocess.TimeoutExpired):
                        q.tests("./fixture", "^Test", [source], label, groups=groups)
                else:
                    q.tests("./fixture", "^Test", [source], label, groups=groups)
        return steps

    def test_every_group_runs_in_its_own_server_and_discovery_in_none(self):
        self.assertEqual(self.trace(4, "storage"), [("run", "storage-discovery")] + [
            step for n in range(1, 5)
            for step in (("open", f"storage-group-{n}"), ("run", f"storage-group-{n}"), ("close", f"storage-group-{n}"))])

    def test_single_group_package_runs_in_its_own_server_too(self):
        self.assertEqual(self.trace(1, "graphread"), [("run", "graphread-discovery"), ("open", "graphread"),
                                                      ("run", "graphread"), ("close", "graphread")])

    def test_failed_or_timed_out_group_still_closes_its_server_and_stops_the_lane(self):
        self.assertEqual(self.trace(4, "storage", failing_run="storage-group-2"), [
            ("run", "storage-discovery"),
            ("open", "storage-group-1"), ("run", "storage-group-1"), ("close", "storage-group-1"),
            ("open", "storage-group-2"), ("run", "storage-group-2"), ("close", "storage-group-2")])

    def test_capture_and_smoke_share_one_server_opened_after_the_python_tests(self):
        # The capture, the smoke and the Python tests are the rest leg's (be-vm7gou G1).
        servers = self_calls("execute_rest", "server")
        self.assertEqual(len(servers), 1)
        self.assertEqual(resolve(servers[0].args[0]), "capture")
        self.assertEqual(module.CAPTURE_SERVER, "capture")
        runs = {ast.literal_eval(c.args[1]): c.lineno for c in self_calls("execute_rest", "run")
                if len(c.args) > 1 and isinstance(c.args[1], ast.Constant)}
        captures = [c.lineno for c in self_calls("execute_rest", "capture")]
        self.assertEqual(len(captures), 2)
        self.assertLess(runs["python-example-tests"], servers[0].lineno)
        self.assertTrue(all(servers[0].lineno < line for line in captures + [runs["bdp-http-capture"]]))

    def test_no_entry_point_starts_a_lane_wide_server(self):
        for entry in ("execute", "execute_storage", "execute_rest"):
            with self.subTest(entry=entry):
                self.assertFalse([n for n in ast.walk(lane_function(entry)) if isinstance(n, ast.Attribute) and n.attr == "Popen"],
                                 f"{entry} must not start a lane-wide server")
        for entry in ("execute", "execute_storage"):
            self.assertEqual(self_calls(entry, "server"), [], f"{entry} opens its servers through tests(), one per group")


FAKE_DOLT = '''#!{python}
import json, os, signal, socket, sys, time
args = sys.argv[1:]
print(json.dumps(args), flush=True)
mode = os.environ.get("FAKE_DOLT_MODE", "")
if mode == "exit-early":
    sys.exit(3)
signal.signal(signal.SIGTERM, signal.SIG_IGN if mode == "ignore-term" else (lambda *_: sys.exit(0)))
sock = socket.socket()
sock.bind(("127.0.0.1", int(args[args.index("--port") + 1])))
sock.listen()
while True:
    time.sleep(0.05)
'''


class ServerLifecycle(unittest.TestCase):
    # A stand-in dolt binds the port it is given on loopback, the way the
    # nested-ownership controls bind a real listener; no database is involved.
    def lane(self, directory, mode=""):
        fake = Path(directory) / "dolt"
        fake.write_text(FAKE_DOLT.format(python=sys.executable))
        fake.chmod(0o755)
        q = runner(directory)
        q.output = Path(directory) / "output"
        q.output.mkdir()
        q.dolt = fake
        q.env["FAKE_DOLT_MODE"] = mode
        return q

    def test_server_is_fresh_owned_exported_for_the_run_and_shut_down_cleanly(self):
        with tempfile.TemporaryDirectory() as directory:
            q = self.lane(directory)
            ports = []
            for label in ("first", "second"):
                with q.server(label) as port:
                    ports.append(port)
                    socket.create_connection(("127.0.0.1", port), timeout=5).close()
                    self.assertEqual(q.env["BEADS_GRAPH_TEST_SERVER_PORT"], str(port))
                    self.assertEqual(len(q.children), 1)
                with self.assertRaises(ConnectionRefusedError):
                    socket.create_connection(("127.0.0.1", port), timeout=1)
                self.assertNotIn("BEADS_GRAPH_TEST_SERVER_PORT", q.env)
                self.assertEqual(q.children, set())
                started = json.loads((q.output / (label + "-server.log")).read_text().splitlines()[0])
                self.assertEqual(started[started.index("--data-dir") + 1], str(q.output / (label + "-server-data")))
                self.assertEqual(started[started.index("--host") + 1], "127.0.0.1")
                self.assertTrue((q.output / (label + "-server-data")).is_dir())
            self.assertEqual([(r["label"], r["port"], r["exit"], r["forced"], r["groupGone"], r["portClosed"])
                              for r in q.receipts],
                             [("first-server-shutdown", ports[0], 0, False, True, True),
                              ("second-server-shutdown", ports[1], 0, False, True, True)])
            self.assertTrue(all(r["elapsedSeconds"] >= 0 for r in q.receipts))

    def test_unclean_shutdown_fails_with_the_existing_message(self):
        with tempfile.TemporaryDirectory() as directory:
            q = self.lane(directory, mode="ignore-term")
            with mock.patch.object(module, "SERVER_STOP_TIMEOUT", 0.3), \
                 self.assertRaisesRegex(RuntimeError, r"server cleanup failed: exit=-9, forced=True, groupGone=True, portClosed=True"):
                with q.server("stubborn"):
                    pass
            self.assertEqual(q.children, set())
            self.assertTrue(q.receipts[-1]["forced"])

    def test_failed_run_still_reaps_its_server(self):
        with tempfile.TemporaryDirectory() as directory:
            q = self.lane(directory)
            with self.assertRaisesRegex(RuntimeError, "group failed"):
                with q.server("failing") as port:
                    raise RuntimeError("group failed")
            with self.assertRaises(ConnectionRefusedError):
                socket.create_connection(("127.0.0.1", port), timeout=1)
            self.assertEqual(q.children, set())
            self.assertEqual(q.receipts[-1]["exit"], 0)
            self.assertNotIn("BEADS_GRAPH_TEST_SERVER_PORT", q.env)

    def test_server_that_exits_before_readiness_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            q = self.lane(directory, mode="exit-early")
            with self.assertRaisesRegex(RuntimeError, "server cleanup failed: exit=3") as refused:
                with q.server("early"):
                    self.fail("the run must not start against a dead server")
            self.assertIn("owned server exited before readiness", str(refused.exception.__context__))
            self.assertEqual(q.children, set())
            self.assertEqual(q.receipts[-1]["exit"], 3)
class SmokeDeadlines(unittest.TestCase):
    # The HTTP/BDP smoke starts with a server-backed `bd init`. Hosted runners differ by
    # about 2x: that init took 34 s on a fast runner and 58 s and 65 s on slow ones, and the
    # smoke's 60 s per-command default killed it on both slow attempts. Pin the deadlines the
    # qualification really hands the smoke, parsed the way the smoke parses them (so a dropped
    # flag falls back to the smoke's own default and is judged as such), and the job budget
    # around them, so none can slide back under a slow runner's measured times.
    SLOWEST_INIT_SECONDS = 65
    INIT_HEADROOM = 2.5
    # The wrapper must outlast the smoke's whole run by the time the smoke needs to reap its
    # children and report its own labelled failure; the original margin was this much.
    WRAPPER_MARGIN_SECONDS = 60
    # The smoke runs in the rest leg since the graph qualification was split into legs (be-vm7gou, ruling
    # be-jmdn71 G2), so the cap that must outlast a slow runner is that leg's.
    JOB_NAME = "Graph qualification / CLI groups, HTTP and BDP"
    # The one-job lane's whole-job durations on hosted runners were 27 min on a fast one, 43 to
    # 44 min on five slow ones (each stopped at the smoke's first command), and one slow runner
    # whose job was cut off by the 50 min cap 11 s into the smoke; the headroom over its 51 min
    # floor (1.75, kept) gave that job its 90 min cap. The rest leg's slowest healthy whole-leg
    # run is MODELED at 907.4 s, 15.1 min, not measured: PM's first measured runs of the legs
    # replace this number (A8), and GraphLaneCaps' constant for the leg follows it.
    SLOWEST_JOB_MINUTES = 15.1
    JOB_HEADROOM = 1.75

    @staticmethod
    def job_timeout_minutes(name):
        # The job lives in fork-qualification.yml (be-jvluf7 section 3.5), not pr.yml; read as text, like workflow_jobs.
        named = [lines for lines in workflow_jobs().values() if job_value(lines, "name") == name]
        if len(named) != 1:
            raise AssertionError(f"expected exactly one job named {name!r}, found {len(named)}")
        found = job_value(named[0], "timeout-minutes")
        if found is None or not found.isdigit():
            raise AssertionError(f"expected exactly one numeric timeout-minutes on job {name!r}, found {found!r}")
        return int(found)

    def test_slow_runner_deadlines_nest_inside_the_job_budget(self):
        smoke_path = Path(__file__).resolve().parents[1] / "graph-bdp-read-smoke.py"
        spec = importlib.util.spec_from_file_location("graph_bdp_read_smoke", smoke_path)
        smoke = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(smoke)
        q = module.Qualification.__new__(module.Qualification)
        q.root, q.output = Path("/repo"), Path("/out")
        q.bd, q.node = Path("/bin/bd"), Path("/bin/node")
        q.client_checkout, q.client_manifest = Path("/client"), Path("/client.json")
        argv, wrapper = q.smoke_command(0)
        self.assertEqual(Path(argv[1]), Path("/repo/scripts/graph-bdp-read-smoke.py"))
        args = smoke.build_parser().parse_args(argv[2:])
        command, total = args.command_timeout, args.total_timeout
        with self.subTest("one command outlasts a slow runner's server-backed init"):
            floor = self.INIT_HEADROOM * self.SLOWEST_INIT_SECONDS
            self.assertGreaterEqual(command, floor,
                                    f"per-command deadline {command}s is under {self.INIT_HEADROOM}x the slowest measured init ({self.SLOWEST_INIT_SECONDS}s)")
        with self.subTest("per-command < total < wrapper, with the wrapper's cleanup margin"):
            self.assertLess(command, total, "a single command may not outlast the smoke's whole run")
            self.assertGreaterEqual(wrapper - total, self.WRAPPER_MARGIN_SECONDS,
                                    f"wrapper {wrapper}s must outlast the smoke total {total}s by {self.WRAPPER_MARGIN_SECONDS}s so the smoke reports its own failure")
        with self.subTest("the job's own cap outlasts a slow runner's whole job"):
            minutes = self.job_timeout_minutes(self.JOB_NAME)
            floor = self.SLOWEST_JOB_MINUTES * self.JOB_HEADROOM
            self.assertGreaterEqual(minutes, floor,
                                    f"job cap {minutes} min is under {self.JOB_HEADROOM}x the slowest measured whole job ({self.SLOWEST_JOB_MINUTES} min)")
            self.assertGreater(minutes * 60, wrapper, "the job cap must outlast the smoke wrapper")


LEG_JOBS = {leg: "graph-" + leg for leg in module.LEGS}  # each leg's job id in fork-qualification.yml
AGGREGATOR = "graph-c0"  # the id and display name the graph core job has always had; PM's tool selects by them
AGGREGATOR_NAME = "Graph core / real engines, installed CLI and BDP"
SHA = "${{ github.sha }}"


class LegSplit(unittest.TestCase):
    # be-vm7gou, ruling be-jmdn71 G7: the leg set, the pairing and the group counts are one constants block in the
    # qualifier, and the workflow must agree with it, so a six-group response is a change to that block and to the
    # workflow and to nothing else.
    def test_storage_legs_partition_every_storage_group_and_the_rest_leg_runs_every_cli_group(self):
        paired = [number for groups in module.STORAGE_LEGS.values() for number in groups]
        self.assertEqual(sorted(paired), list(range(1, module.STORAGE_GROUPS + 1)))
        self.assertEqual(module.LEG_GROUPS[module.REST_LEG], {"cli": tuple(range(1, module.CLI_GROUPS + 1))})
        self.assertEqual(module.LEGS, (*module.STORAGE_LEGS, module.REST_LEG))
        module.check_leg_constants()

    def test_a_split_that_drops_or_repeats_a_group_or_a_label_is_refused(self):
        groups, labels = module.LEG_GROUPS, module.LEG_LABELS
        for name, change, message in [
                ("a storage group in two legs", dict(LEG_GROUPS={**groups, "storage-b": {"storage": (2, 3, 4)}}), "groups 1..4 once each"),
                ("a storage group in no leg", dict(LEG_GROUPS={**groups, "storage-b": {"storage": (2,)}}), "groups 1..4 once each"),
                ("a cmd/bd group dropped", dict(LEG_GROUPS={**groups, "rest": {"cli": (1, 2)}}), "groups 1..3 once each"),
                ("a label in two legs", dict(LEG_LABELS={**labels, "storage-a": ("storage", "config")}), "config: must run in exactly one leg"),
                ("a label in no leg", dict(LEG_LABELS={**labels, "rest": tuple(l for l in labels["rest"] if l != "graphpatch")}),
                 "graphpatch: must run in exactly one leg"),
                ("a grouped label dropped while its groups remain", dict(LEG_LABELS={**labels, "storage-a": ()}),
                 "groups must be listed for exactly"),
                ("a leg with no groups entry", dict(LEG_GROUPS={k: v for k, v in groups.items() if k != "rest"}),
                 "every leg needs its labels and groups"),
                ("a leg with labels and groups that is not in the leg set", dict(LEGS=module.LEGS[:2]),
                 "every leg needs its labels and groups"),
                ("a leg in the leg set with no labels or groups", dict(LEGS=(*module.LEGS, "storage-c")),
                 "every leg needs its labels and groups")]:
            with self.subTest(name), mock.patch.multiple(module, **change), self.assertRaisesRegex(RuntimeError, message):
                module.check_leg_constants()

    def test_every_go_test_run_owns_a_fresh_server_and_the_legs_start_nineteen_between_them(self):
        servers = {leg: module.expected_servers(leg) for leg in module.LEGS}
        for leg, numbers in module.STORAGE_LEGS.items():
            self.assertEqual(servers[leg], [f"storage-group-{n}" for n in numbers])
        singles = [label for label in module.REST_LABELS if label not in module.GROUPED]
        self.assertEqual(sorted(servers[module.REST_LEG]), sorted(
            [*singles, *(f"cli-group-{n}" for n in range(1, module.CLI_GROUPS + 1)), module.CAPTURE_SERVER]))
        every = [name for names in servers.values() for name in names]
        self.assertEqual(len(every), len(set(every)), "no server may belong to two legs")
        # The one lane's count: eleven single go test runs, the four storage and three cmd/bd groups, the capture server.
        self.assertEqual(len(every), 19)

    def test_the_workflow_has_one_job_per_leg_named_for_the_groups_it_runs(self):
        jobs = workflow_jobs()
        self.assertEqual({job for job in jobs if job.startswith("graph-")}, {*LEG_JOBS.values(), AGGREGATOR})
        for leg, numbers in module.STORAGE_LEGS.items():
            self.assertEqual(job_value(jobs[LEG_JOBS[leg]], "name"),
                             "Graph qualification / storage groups " + " and ".join(map(str, numbers)))
        self.assertEqual(job_value(jobs[LEG_JOBS[module.REST_LEG]], "name"), SmokeDeadlines.JOB_NAME)
        names = [job_value(lines, "name") for lines in jobs.values()]
        self.assertEqual([name for name in names if name and "Graph core" in name], [AGGREGATOR_NAME])

    def test_each_leg_job_runs_its_own_leg_with_exactly_the_options_that_leg_takes(self):
        jobs = workflow_jobs()
        for leg in module.LEGS:
            with self.subTest(leg=leg):
                [qualify] = [step for step in job_steps(jobs[LEG_JOBS[leg]]) if "graph-c0-qualify.py" in step["run"]]
                self.assertEqual(re.findall(r"--leg (\S+)", qualify["run"]), [leg])
                mode = leg if leg == module.REST_LEG else "storage"
                self.assertEqual(set(re.findall(r"--([a-z-]+)", qualify["run"])), {"leg", "output", *module.OPTIONS[mode]})
                self.assertIn('--output "$RUNNER_TEMP/' + module.EVIDENCE + '"', qualify["run"])

    def test_each_leg_checks_its_verifier_controls_first_and_always_uploads_its_own_artifact(self):
        jobs = workflow_jobs()
        for leg in module.LEGS:
            with self.subTest(leg=leg):
                lines = jobs[LEG_JOBS[leg]]
                steps = job_steps(lines)
                self.assertEqual(job_value(lines, "needs"), "build-artifacts")
                self.assertEqual(job_value(lines, "runs-on"), "ubuntu-latest")
                self.assertIsNone(job_value(lines, "if"))
                self.assertNotIn("continue-on-error", "\n".join(lines))
                downloads = [step["with"] for step in steps if step["uses"].startswith("actions/download-artifact@")]
                self.assertEqual([found["name"] for found in downloads], ["fork-build-artifacts"])
                self.assertTrue(all("pattern" not in found for found in downloads))
                runs = [step["run"] for step in steps]
                [verifier] = [i for i, run in enumerate(runs) if "graph-c0-verifier-test.py" in run]
                [qualify] = [i for i, run in enumerate(runs) if "graph-c0-qualify.py" in run]
                self.assertLess(verifier, qualify, "the negative controls must run before the qualification")
                [upload] = [step for step in steps if step["uses"].startswith("actions/upload-artifact@")]
                self.assertEqual(upload["with"]["name"], module.LEG_ARTIFACT.format(leg=leg, commit=SHA))
                self.assertEqual(upload["if"], "${{ always() }}", "a failed leg must still leave its evidence")
                self.assertIn("${{ runner.temp }}/" + module.EVIDENCE + "/", upload["with"]["path"].splitlines())
                self.assertEqual(upload["with"]["if-no-files-found"], "error")

    def test_the_aggregator_keeps_the_graph_core_id_name_and_artifact_and_fails_when_any_leg_does(self):
        lines = workflow_jobs()[AGGREGATOR]
        steps = job_steps(lines)
        self.assertEqual(job_value(lines, "name"), AGGREGATOR_NAME)
        self.assertEqual(sorted(re.findall(r"[\w-]+", job_value(lines, "needs"))), sorted(["build-artifacts", *LEG_JOBS.values()]))
        self.assertIsNone(job_value(lines, "if"), "a leg that failed, was cancelled or was skipped must skip the aggregator")
        self.assertNotIn("continue-on-error", "\n".join(lines))
        downloads = [step["with"] for step in steps if step["uses"].startswith("actions/download-artifact@")]
        self.assertEqual(sorted(found["name"] for found in downloads),
                         sorted(module.LEG_ARTIFACT.format(leg=leg, commit=SHA) for leg in module.LEGS))
        for found in downloads:
            self.assertNotIn("pattern", found)
            self.assertEqual(found["path"], "${{ runner.temp }}/graph-legs/" + found["name"])
        [qualify] = [step for step in steps if "graph-c0-qualify.py" in step["run"]]
        self.assertEqual(re.findall(r"--leg (\S+)", qualify["run"]), [module.AGGREGATE])
        self.assertEqual(set(re.findall(r"--([a-z-]+)", qualify["run"])), {"leg", "output", *module.OPTIONS[module.AGGREGATE]})
        self.assertIn('--commit "$GRAPH_COMMIT"', qualify["run"])
        self.assertIn("GRAPH_COMMIT: " + SHA, "\n".join(lines))
        self.assertIn('--legs-dir "$RUNNER_TEMP/graph-legs"', qualify["run"])
        self.assertIn('--output "$RUNNER_TEMP/graph-c0-combined"', qualify["run"])
        [upload] = [step for step in steps if step["uses"].startswith("actions/upload-artifact@")]
        self.assertEqual(upload["with"]["name"], "graph-c0-" + SHA)
        self.assertEqual(upload["with"]["path"], "${{ runner.temp }}/graph-c0-combined")
        self.assertIsNone(upload["if"], "only an aggregation that passed may leave a combined artifact")


class GraphLaneCaps(unittest.TestCase):
    # G3 (be-jmdn71): no graph-lane job's cap is ever raised past the old 90 minutes, and each outlasts its own job's
    # slowest healthy run by the 1.75x the one job carried. The healthy minutes are MODELED, not measured (R4.4): PM's
    # first measured runs (A8) replace them and the caps follow. One named constant per job.
    CEILING = 90
    HEADROOM = SmokeDeadlines.JOB_HEADROOM
    SLOWEST_HEALTHY_MINUTES = {"graph-storage-a": 19.5, "graph-storage-b": 19.7,
                               "graph-rest": SmokeDeadlines.SLOWEST_JOB_MINUTES, AGGREGATOR: 1.0}

    def test_the_graph_lane_is_exactly_the_jobs_that_have_a_floor(self):
        self.assertEqual({job for job in workflow_jobs() if job.startswith("graph-")}, set(self.SLOWEST_HEALTHY_MINUTES))

    def test_every_cap_is_within_the_ceiling_and_outlasts_its_jobs_slowest_healthy_run_with_headroom(self):
        jobs = workflow_jobs()
        for job, slowest in self.SLOWEST_HEALTHY_MINUTES.items():
            with self.subTest(job=job):
                found = job_value(jobs[job], "timeout-minutes")
                self.assertTrue(found and found.isdigit(), f"{job} needs one numeric timeout-minutes, found {found!r}")
                self.assertLessEqual(int(found), self.CEILING)
                self.assertGreaterEqual(int(found), slowest * self.HEADROOM,
                                        f"{job}: cap {found} min is under {self.HEADROOM}x its slowest healthy run ({slowest} min)")


class HostInfo(unittest.TestCase):
    # H1 (be-jmdn71): each leg records its runner's hardware in its own artifact, from named fields only. It is latency
    # evidence for reading the legs and gates nothing.
    FIELDS = {"nproc", "cpuModel", "memTotal", "kernel", "runnerImageVersion"}

    def test_named_fields_only_never_an_environment_dump(self):
        with mock.patch.dict(os.environ, {"SECRET_TOKEN": "hunter2", "ImageVersion": "20260301.5"}):
            info = module.host_info()
        self.assertEqual(set(info), self.FIELDS)
        self.assertEqual(info["runnerImageVersion"], "20260301.5")
        self.assertNotIn("hunter2", json.dumps(info))

    def test_cpu_model_and_memory_come_from_proc(self):
        texts = {"/proc/cpuinfo": "processor\t: 0\nmodel name\t: Fixture CPU @ 3.00GHz\n",
                 "/proc/meminfo": "MemTotal:       16384000 kB\nMemFree:         1000 kB\n"}
        with mock.patch.object(Path, "read_text", autospec=True, side_effect=lambda path, *args, **kwargs: texts[str(path)]):
            info = module.host_info()
        self.assertEqual((info["cpuModel"], info["memTotal"]), ("Fixture CPU @ 3.00GHz", "16384000 kB"))

    def test_an_unreadable_proc_file_is_none_and_never_an_error(self):
        with mock.patch.object(Path, "read_text", autospec=True, side_effect=OSError("denied")):
            info = module.host_info()
        self.assertEqual(set(info), self.FIELDS)
        self.assertIsNone(info["cpuModel"])
        self.assertIsNone(info["memTotal"])

    def test_host_json_stays_out_of_the_republished_artifact(self):
        self.assertIn("host.json", module.LEG_PRIVATE)


class PartialGroupRun(unittest.TestCase):
    # A storage leg runs only its own groups of the one plan (be-vm7gou, ruling G1). It proves the roots of those groups;
    # the aggregator proves the rest across the legs, so only a run of every group may call the label complete.
    NAMES = ["Test" + letter for letter in "ABCDEFGH"]  # round-robin over four groups: A,E / B,F / C,G / D,H

    def trace(self, run_groups, required=(), omit=None):
        steps = []
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "storage_test.go"
            source.write_text("\n".join("func " + name + "(t *testing.T) {}" for name in self.NAMES))
            q = module.Qualification.__new__(module.Qualification)
            q.output, q.counts = root, {}

            @contextlib.contextmanager
            def server(label):
                steps.append(("open", label))
                try:
                    yield 0
                finally:
                    steps.append(("close", label))

            def run(args, label, **kwargs):
                steps.append(("run", label))
                if "-list" in args:
                    return ("\n".join(self.NAMES) + "\n").encode()
                events = []
                for name in args[args.index("-run") + 1][2:-2].split("|"):
                    events.append(event("run", name))
                    if name != omit:
                        events += [event("run", name + "/embedded"), event("pass", name + "/embedded")]
                    events.append(event("pass", name))
                raw = ("\n".join(json.dumps(item) for item in events + [event("pass")]) + "\n").encode()
                (root / (label + ".stdout")).write_bytes(raw)
                (root / (label + ".stderr")).write_bytes(b"")
                return raw

            q.server = server
            with mock.patch.object(q, "run", run):
                q.tests("./fixture", "^Test", [source], "storage", required, groups=4, run_groups=run_groups)
            plan = json.loads((root / "storage-groups.json").read_text())
            parts = b"".join((root / f"storage-group-{n}.stdout").read_bytes() for n in (run_groups or range(1, 5)))
            joined = (root / "storage.stdout").read_bytes() == parts
        return steps, plan, q.counts, joined

    def test_only_the_legs_groups_run_each_in_its_own_server_and_the_label_is_not_complete(self):
        steps, plan, counts, joined = self.trace((1, 4))
        self.assertEqual(steps, [("run", "storage-discovery")] + [
            step for n in (1, 4) for step in (("open", f"storage-group-{n}"), ("run", f"storage-group-{n}"), ("close", f"storage-group-{n}"))])
        self.assertEqual([group["passed"] for group in plan["groups"]], [True, False, False, True])
        self.assertEqual([group["roots"] for group in plan["groups"]],
                         module.partition_required_tests(self.NAMES, 4), "the plan still holds every group, for the aggregator")
        self.assertIs(plan["complete"], False)
        self.assertEqual(counts, {}, "a label this leg only partly ran is not counted as passed")
        self.assertTrue(joined, "the leg's combined output is its groups' output in group order")

    def test_a_run_of_every_group_is_complete_however_it_is_asked_for(self):
        for run_groups in (None, (1, 2, 3, 4)):
            with self.subTest(run_groups=run_groups):
                steps, plan, counts, joined = self.trace(run_groups)
                self.assertIs(plan["complete"], True)
                self.assertEqual(counts, {"storage": 8})
                self.assertTrue(all(group["passed"] for group in plan["groups"]) and joined)

    def test_required_tuples_are_demanded_only_for_the_roots_the_leg_ran(self):
        self.trace((1, 4), ("TestA/embedded", "TestB/embedded"))  # TestB is group 2's: the other storage leg proves it
        with self.assertRaisesRegex(RuntimeError, "required engine proof absent"):
            self.trace((1, 4), ("TestA/embedded",), omit="TestA")  # TestA ran here and its proof is missing

    def test_roots_the_groups_ran_must_be_discoverys_roots_each_once_or_a_part_of_them_for_a_partial_leg(self):
        # partition_required_tests refuses an incomplete partition first, so each case has it hand back the partition it is told
        # to, and holds the run's own check of the roots its groups ran alone.
        real = module.partition_required_tests
        for name, run_groups, change in [
                ("a root dropped, every group run", None, lambda groups: [groups[0][1:], *groups[1:]]),
                ("a root run in two groups, every group run", None, lambda groups: [groups[0], [*groups[1], groups[0][0]], *groups[2:]]),
                ("a root discovery lacks, a leg's groups run", (1, 4), lambda groups: [[*groups[0], "TestZ"], *groups[1:]]),
                ("a root run in two of a leg's groups", (1, 4), lambda groups: [*groups[:3], [*groups[3], groups[0][0]]])]:
            with self.subTest(name), mock.patch.object(module, "partition_required_tests", lambda names, groups: change(real(names, groups))), \
                    self.assertRaisesRegex(RuntimeError, "grouped test union differs from complete discovery"):
                self.trace(run_groups)

    def test_groups_to_run_must_be_distinct_ascending_numbers_inside_the_plan(self):
        for run_groups in [(), (1, 1), (4, 1), (0, 1), (5,), (1, 2, 3, 4, 5)]:
            with self.subTest(run_groups=run_groups), self.assertRaisesRegex(RuntimeError, "groups to run must be distinct numbers in 1..4"):
                self.trace(run_groups)

    def test_a_single_run_label_cannot_be_given_groups(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "x_test.go"
            source.write_text("func TestX(t *testing.T) {}\n")
            q = module.Qualification.__new__(module.Qualification)
            q.output, q.counts = Path(directory), {}
            with mock.patch.object(q, "run", return_value=b"TestX\n"), self.assertRaisesRegex(RuntimeError, "run_groups needs a grouped run"):
                q.tests("./fixture", "^Test", [source], "single", groups=1, run_groups=(1,))


class EntryPoints(unittest.TestCase):
    # Each mode takes exactly its own options (be-vm7gou): a storage leg is never handed a BDP client, and the aggregator
    # never builds a Qualification, so it needs neither Go nor Dolt.
    EVERY_OPTION = sorted({option for options in module.OPTIONS.values() for option in options})

    @staticmethod
    def mode(leg):
        return leg if leg in (module.REST_LEG, module.AGGREGATE) else "storage"

    def start(self, argv):
        """main() under argv with everything it would start replaced: (aggregate, Qualification, exit code or None)."""
        code = None
        with mock.patch.object(sys, "argv", ["graph-c0-qualify.py", *argv]), \
                mock.patch.object(module, "aggregate", return_value={"label": 1}) as aggregate, \
                mock.patch.object(module, "Qualification") as qualification, mock.patch.object(module, "publish_phase_report"), \
                mock.patch.object(module.signal, "signal"), contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()), tempfile.TemporaryDirectory() as directory:
            qualification.return_value.output, qualification.return_value.receipts = Path(directory), []
            try:
                module.main()
            except SystemExit as stop:
                code = stop.code
        return aggregate, qualification, code

    @staticmethod
    def argv(leg, options):
        return ["--leg", leg, "--output", "out", *(word for option in options for word in ("--" + option, option))]

    def test_each_mode_runs_with_exactly_its_options(self):
        for leg in (*module.LEGS, module.AGGREGATE):
            with self.subTest(leg=leg):
                aggregate, qualification, code = self.start(self.argv(leg, module.OPTIONS[self.mode(leg)]))
                self.assertIsNone(code)
                if leg == module.AGGREGATE:
                    aggregate.assert_called_once_with("legs-dir", "commit", "out")
                    qualification.assert_not_called()
                else:
                    qualification.assert_called_once()
                    qualification.return_value.execute.assert_called_once_with(leg)
                    aggregate.assert_not_called()

    def test_a_missing_or_extra_option_is_refused_before_anything_runs(self):
        for leg in (*module.LEGS, module.AGGREGATE):
            options = module.OPTIONS[self.mode(leg)]
            for name, given in [*((f"without --{option}", [o for o in options if o != option]) for option in options),
                                *((f"with --{option}", [*options, option]) for option in self.EVERY_OPTION if option not in options)]:
                with self.subTest(leg=leg, case=name):
                    aggregate, qualification, code = self.start(self.argv(leg, given))
                    self.assertEqual(code, 2)
                    aggregate.assert_not_called()
                    qualification.assert_not_called()

    def test_an_unknown_leg_or_a_missing_leg_or_output_is_refused(self):
        rest = [word for option in module.OPTIONS[module.REST_LEG] for word in ("--" + option, option)]  # all else is right
        for argv in (["--leg", "storage-c", "--output", "out", *rest], ["--output", "out", *rest], ["--leg", module.REST_LEG, *rest]):
            with self.subTest(argv=argv):
                aggregate, qualification, code = self.start(argv)
                self.assertEqual(code, 2)
                aggregate.assert_not_called()
                qualification.assert_not_called()


COMMIT = "0123456789abcdef0123456789abcdef01234567"
OTHER_COMMIT = "fedcba9876543210fedcba9876543210fedcba98"
LINK = "TestLinkUnlinkDefaultSourceReadBudget"  # the storage root that owns the required engine tuples

# Synthetic legs: the real Qualification.execute, run against a fake repository with fake commands, servers and
# capture, writes each leg's evidence, and the aggregator is held to that evidence. No Go, Dolt or network is used.
FIXTURE = {  # label -> (its test source in the fake repository, its test roots)
    "graphpatch": ("internal/graphpatch/patch_test.go", ["TestPatch"]),
    "storage": ("internal/storage/graphstore/store_test.go",
                [LINK, "TestMerge", "TestNodes", "TestOrder", "TestPatchStore", "TestQuery", "TestRange", "TestStore"]),
    "config": ("internal/configfile/graph_mode_test.go", ["TestGraphModeCase"]),
    "query-config": ("internal/storage/issueops/config_strict_test.go", ["TestResolveCustomConfigStrict"]),
    "utc-issueops": ("internal/storage/issueops/prepare_timestamps_utc_test.go",
                     ["TestPrepareIssueForInsertNormalizesOptionalTimestampsToUTC"]),
    "utc-db": ("internal/storage/domain/db/normalize_timestamps_utc_test.go",
               ["TestNormalizeIssueTimestampsConvertsOptionalTimestampsToUTC"]),
    "utc-types": ("internal/types/types_test.go", ["TestNormalizeOptionalTimestampsToUTCCoversEveryPointerTimestamp"]),
    "agent-template": ("internal/templates/agents/agents_test.go", ["TestGraphPreviewAgents"]),
    "graph-claude-setup": ("cmd/bd/setup/graph_claude_test.go", ["TestGraphPreviewClaudeSetup"]),
    "cli": ("cmd/bd/graph_cli_test.go", sorted({name.split("/")[0] for name in module.CLI_REQUIRED})),
    "bdpwire": ("internal/httpapi/bdpwire/wire_test.go", ["TestWire"]),
    "graphread": ("internal/httpapi/graphread/graphread_test.go", ["TestAuthoritativeRecordsProjectToPublicWire"]),
    "graph-http": ("internal/httpapi/graph_read_test.go", ["TestGraphReadHTTPAuthorityAndSecurity"]),
}
ROOTS = {label: roots for label, (_, roots) in FIXTURE.items()}
HTTP_SOURCES = (("harness_sha256", "scripts/graph-bdp-read-smoke.py"),
                ("client_harness_sha256", "scripts/graph-bdp-read-client.mjs"),
                ("python_example_sha256", "examples/bdp-read/read_beads.py"))


def write(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    return path


def edit_json(path, change):
    data = json.loads(path.read_text())
    change(data)
    path.write_text(json.dumps(data, indent=2) + "\n")


def root_events(label, name):
    """What go test -json prints for one root: it and every required subtest of it run and pass."""
    nested = [test for test in module.REQUIRED.get(label, ()) if test != name and test.split("/", 1)[0] == name]
    return [event("run", name), *(item for test in nested for item in (event("run", test), event("pass", test))), event("pass", name)]


def fake_leg(workdir, artifact, leg, fail_on=None):
    """A Qualification for one leg whose commands, servers and capture are fakes. Its output is the leg's evidence directory."""
    q = module.Qualification.__new__(module.Qualification)
    q.root, q.output = workdir / "repo", artifact / module.EVIDENCE
    q.bd, q.dolt, q.artifacts = workdir / "bin/bd", workdir / "bin/dolt", workdir / "build"
    q.client_checkout, q.client_manifest, q.node = workdir / "client", workdir / "client-build/client-build.json", workdir / "bin/node"
    q.children, q.receipts, q.counts, q.env = set(), [], {}, {}
    (q.output / "home").mkdir(parents=True)

    def respond(label, args):
        if label == "head":
            return COMMIT.encode() + b"\n", b""
        if label == "source-status":
            return b"", b""
        if label == "dolt-version":
            return b"dolt version 2.1.8\n", b""
        if label == "python-example-tests":
            return b"", b"...\nRan 3 tests in 0.001s\n\nOK\n"
        if label == "bdp-http-capture":
            write(q.output / "http/summary.json", json.dumps({field: module.digest(q.root / relative) for field, relative in HTTP_SOURCES}))
            return b"", b""
        if label.endswith("-discovery"):
            return ("\n".join(ROOTS[label[:-len("-discovery")]]) + "\nok  \texample.test/package\t0.012s\n").encode(), b""
        base = re.sub(r"-group-\d+$", "", label)
        chosen = args[args.index("-run") + 1][2:-2].split("|") if base != label else ROOTS[label]
        events = [item for name in chosen for item in root_events(base, name)] + [event("pass")]
        return "".join(json.dumps(item) + "\n" for item in events).encode(), b""

    def run(args, label, roots=None, **_):
        if label == fail_on:
            raise subprocess.TimeoutExpired(args, 960)
        out, err = respond(label, [str(a) for a in args])
        (q.output / (label + ".stdout")).write_bytes(out)
        (q.output / (label + ".stderr")).write_bytes(err)
        q.receipts.append(dict(label=label, argv=[str(a) for a in args], pid=1, exit=0, stdoutSHA256=hashlib.sha256(out).hexdigest(),
                               elapsedSeconds=0.01, **({} if roots is None else dict(roots=roots))))
        return out

    @contextlib.contextmanager
    def server(label):
        (q.output / (label + "-server-data")).mkdir()
        (q.output / (label + "-server.log")).write_text("{}\n")
        try:
            yield 41000 + len(q.receipts)
        finally:
            q.receipts.append(dict(label=label + "-server-shutdown", pid=2, port=41000 + len(q.receipts), exit=0, forced=False,
                                   groupGone=True, portClosed=True, elapsedSeconds=0.5))

    q.run, q.server, q.capture = run, server, lambda engine, port: None
    return q


def run_leg(workdir, artifact, leg, fail_on=None):
    """One leg as the workflow runs it: execute, then main()'s receipts.json, then the upload's build files."""
    q = fake_leg(workdir, artifact, leg, fail_on)
    try:
        with mock.patch.object(module, "verify_http_capture"):
            q.execute(leg)
    finally:
        (q.output / "receipts.json").write_text(json.dumps(q.receipts, indent=2) + "\n")
        build = artifact / "graph-c0-build"
        build.mkdir()
        for name in ("build-manifest.txt", "SHA256SUMS"):
            shutil.copy2(workdir / "build" / name, build / name)
        if leg == module.REST_LEG:
            shutil.copytree(workdir / "client-build", artifact / "bdp-client-build")
    return q


def artifact_dir(base, leg, commit=COMMIT):
    return base / module.LEG_ARTIFACT.format(leg=leg, commit=commit)


def build_workdir(workdir):
    """A fake repository, a build and three good legs: workdir/legs holds the artifacts the aggregator is given."""
    for label, (relative, roots) in FIXTURE.items():
        write(workdir / "repo" / relative, "".join(f"func {name}(t *testing.T) {{}}\n" for name in roots))
    for _, relative in HTTP_SOURCES:
        write(workdir / "repo" / relative, f"stand-in for {relative}\n")
    write(workdir / "repo/examples/bdp-read/test_read.py", "".join(f"    def test_{n}(self):\n        pass\n" for n in "abc"))
    for name in ("bd", "dolt", "node"):
        write(workdir / "bin" / name, "#!/bin/sh\nexit 0\n").chmod(0o755)
    write(workdir / "build/build-manifest.txt",
          f"commit={COMMIT}\ngo_version=go1.26.0\nbuild_tags=gms_pure_go\nartifact=bd-linux-gms-pure\n")
    write(workdir / "build/SHA256SUMS", f"{module.digest(workdir / 'bin/bd')}  bd-linux-gms-pure\n")
    write(workdir / "client-build/client-build.json", json.dumps({"files": {}}))
    write(workdir / "client-build/build.log", "built\n")
    for leg in module.LEGS:
        run_leg(workdir, artifact_dir(workdir / "legs", leg), leg)


_WORKDIR = None  # the good legs, built by the first test that needs them and removed by tearDownModule


def synthetic_legs():
    global _WORKDIR
    if _WORKDIR is None:
        _WORKDIR = tempfile.TemporaryDirectory()
        build_workdir(Path(_WORKDIR.name))
    return Path(_WORKDIR.name)


def tearDownModule():
    if _WORKDIR is not None:
        _WORKDIR.cleanup()


def snapshot(root):
    """The tree under root as {"relative/path": its bytes, or None for a directory}, each directory before what is in it, read
    once. Plain os calls and string keys on purpose: AggregatorControls takes one of these for every case, and the pin that runs
    those cases under Bazel is held to what they cost in CPU."""
    shot = {}

    def read(directory, prefix):
        with os.scandir(directory) as scan:
            entries = list(scan)
        for entry in entries:
            assert not entry.is_symlink(), f"{entry.path} is a link, which restore() does not put back"
            key = prefix + entry.name
            if entry.is_dir(follow_symlinks=False):
                shot[key] = None
                read(entry.path, key + "/")
            else:
                with open(entry.path, "rb") as stream:
                    shot[key] = stream.read()

    read(os.fspath(root), "")
    return shot


def restore(root, shot):
    """Put the tree under root back as shot holds it, touching only what differs: what is not in shot goes, what changed is
    rewritten and what is missing is made. Copying the whole tree again for every case is the cost this takes away."""
    root = os.fspath(root)
    os.makedirs(root, exist_ok=True)

    def sweep(directory, prefix):
        with os.scandir(directory) as scan:
            entries = list(scan)
        for entry in entries:
            kept = shot.get(prefix + entry.name, False)  # None for a directory, bytes for a file, False for neither
            is_dir = entry.is_dir(follow_symlinks=False)
            if kept is False or entry.is_symlink() or (kept is None) != is_dir:
                if is_dir:
                    shutil.rmtree(entry.path)
                else:
                    os.unlink(entry.path)
            elif is_dir:
                sweep(entry.path, prefix + entry.name + "/")

    sweep(root, "")
    for relative, contents in shot.items():  # snapshot() puts a directory before what is in it, so each parent is made first
        path = os.path.join(root, relative)
        if contents is None:
            try:
                os.mkdir(path)
            except FileExistsError:
                pass
            continue
        try:
            with open(path, "rb") as stream:
                if stream.read() == contents:
                    continue
        except FileNotFoundError:
            pass
        with open(path, "wb") as stream:
            stream.write(contents)


class FixtureRestore(unittest.TestCase):
    # AggregatorControls puts three good legs back with restore() before each of its cases instead of copying them again, so
    # whatever a case can do to the legs has to be undone exactly, and what it did not touch has to be left alone.
    def setUp(self):
        scratch = tempfile.TemporaryDirectory()
        self.addCleanup(scratch.cleanup)
        self.root = Path(scratch.name) / "tree"
        for relative in ("top.json", "leg/one.json", "leg/deeper/two.json", "leg/deeper/still/three.json"):
            write(self.root / relative, f"{relative}\n")
        (self.root / "empty").mkdir()
        self.shot = snapshot(self.root)

    def test_what_a_case_can_do_to_a_tree_is_undone_exactly(self):
        def replace(relative, text):
            return lambda: (self.root / relative).write_text(text)

        def remove_then(relative, make):
            return lambda: (shutil.rmtree(self.root / relative), make(self.root / relative))

        for name, change in [
                ("nothing", lambda: None),
                ("a file rewritten to the same length", replace("top.json", "TOP.JSON\n")),
                ("a file emptied", replace("leg/one.json", "")),
                ("a file removed", lambda: (self.root / "leg/deeper/two.json").unlink()),
                ("a directory removed with what is in it", lambda: shutil.rmtree(self.root / "leg/deeper")),
                ("a directory renamed", lambda: (self.root / "leg").rename(self.root / "other")),
                ("a file added", replace("added.json", "added\n")),
                ("a directory added with a file in it", lambda: write(self.root / "extra/file.json", "extra\n")),
                ("an empty directory removed", lambda: (self.root / "empty").rmdir()),
                ("a file where a directory was", remove_then("leg/deeper", lambda path: path.write_text("file\n"))),
                ("a directory where a file was", lambda: ((self.root / "top.json").unlink(), (self.root / "top.json").mkdir())),
                ("a link where a file was", lambda: ((self.root / "top.json").unlink(), (self.root / "top.json").symlink_to("leg/one.json"))),
                ("everything removed", lambda: shutil.rmtree(self.root))]:
            with self.subTest(name):
                restore(self.root, self.shot)
                change()
                restore(self.root, self.shot)
                self.assertEqual(snapshot(self.root), self.shot)

    def test_what_a_case_did_not_change_is_not_written_again(self):
        untouched = [self.root / "leg/one.json", self.root / "leg/deeper/still/three.json", self.root / "empty"]
        before = [(path.stat().st_ino, path.stat().st_mtime_ns) for path in untouched]
        (self.root / "top.json").write_text("changed\n")
        (self.root / "leg/deeper/two.json").unlink()
        restore(self.root, self.shot)
        self.assertEqual(snapshot(self.root), self.shot)
        self.assertEqual([(path.stat().st_ino, path.stat().st_mtime_ns) for path in untouched], before)

    def test_a_tree_with_a_link_is_refused_because_restore_would_not_put_it_back(self):
        (self.root / "link").symlink_to("top.json")
        with self.assertRaisesRegex(AssertionError, "is a link"):
            snapshot(self.root)


class LegReceipts(unittest.TestCase):
    # What a leg writes: host.json early for latency reading, and leg.json, its completion receipt, only at the very end.
    @classmethod
    def setUpClass(cls):
        cls.workdir = synthetic_legs()

    def test_the_fixture_covers_every_label_the_lane_passes(self):
        self.assertEqual(set(FIXTURE), set(module.PASSED_LABELS))

    def test_a_finished_leg_names_what_it_ran_and_what_it_proved(self):
        for leg in module.LEGS:
            with self.subTest(leg=leg):
                evidence = artifact_dir(self.workdir / "legs", leg) / module.EVIDENCE
                record = json.loads((evidence / "leg.json").read_text())
                self.assertEqual((record["leg"], record["commit"], record["complete"]), (leg, COMMIT, True))
                self.assertEqual(record["labels"], list(module.LEG_LABELS[leg]))
                self.assertEqual(record["groups"], {label: list(numbers) for label, numbers in module.LEG_GROUPS[leg].items()})
                self.assertEqual(record["discovery"], {label: module.roots_digest(ROOTS[label]) for label in module.LEG_LABELS[leg]})
                self.assertEqual("facts" in record, leg == module.REST_LEG)
                if leg == module.REST_LEG:
                    self.assertEqual(record["facts"], module.REST_FACTS)
                    self.assertEqual(record["passed"], {label: len(ROOTS[label]) for label in module.LEG_LABELS[leg]})
                else:
                    self.assertEqual(record["passed"], {}, "a leg that ran only some groups of a label does not count it as passed")

    def test_each_leg_records_its_runners_hardware_in_its_own_artifact_only(self):
        for leg in module.LEGS:
            with self.subTest(leg=leg):
                host = json.loads((artifact_dir(self.workdir / "legs", leg) / module.EVIDENCE / "host.json").read_text())
                self.assertEqual(set(host), {"nproc", "cpuModel", "memTotal", "kernel", "runnerImageVersion"})

    def test_the_storage_legs_plans_say_which_groups_ran(self):
        for leg, numbers in module.STORAGE_LEGS.items():
            with self.subTest(leg=leg):
                plan = json.loads((artifact_dir(self.workdir / "legs", leg) / module.EVIDENCE / "storage-groups.json").read_text())
                self.assertIs(plan["complete"], False)
                self.assertEqual([n for n, group in enumerate(plan["groups"], 1) if group["passed"]], list(numbers))

    def test_a_leg_that_fails_writes_its_host_but_no_completion_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            artifact = artifact_dir(Path(directory), "storage-a")
            with self.assertRaises(subprocess.TimeoutExpired):
                run_leg(self.workdir, artifact, "storage-a", fail_on="storage-group-4")
            evidence = artifact / module.EVIDENCE
            self.assertTrue((evidence / "host.json").is_file())
            self.assertTrue((evidence / "receipts.json").is_file(), "the failed leg's evidence is still uploaded")
            self.assertFalse((evidence / "leg.json").exists())
            plan = json.loads((evidence / "storage-groups.json").read_text())
            self.assertEqual([group["passed"] for group in plan["groups"]], [True, False, False, False])

    def test_a_leg_that_leaves_an_owned_child_behind_writes_no_completion_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            artifact = artifact_dir(Path(directory), "storage-a")
            q = fake_leg(self.workdir, artifact, "storage-a")
            q.execute_storage = lambda groups: q.children.add(12345)
            with mock.patch.object(q, "prelude", lambda: COMMIT), self.assertRaisesRegex(RuntimeError, "owned child process remains"):
                q.execute("storage-a")
            self.assertFalse((q.output / "leg.json").exists())


class AggregatorControls(unittest.TestCase):
    # Ruling be-jmdn71 G8: the aggregator believes no receipt. Each case starts from three good legs, makes what a failed,
    # cancelled, skipped, mismatched or forged leg would leave, and must be refused with nothing published. A refused
    # aggregator fails its job, and fork-qualification-gate fails on that.
    # About a hundred cases, each from fresh legs. Copying the legs again for every case (about two hundred small files and
    # directories created, attribute-copied and removed each time) was most of what the pin that runs these under Bazel spent,
    # so the legs are made once and restore() puts back only what a case changed. reset() then checks they are exactly the good legs.
    @classmethod
    def setUpClass(cls):
        scratch = tempfile.TemporaryDirectory()
        cls.addClassCleanup(scratch.cleanup)
        cls.shot = snapshot(synthetic_legs() / "legs")
        cls.legs, cls.combined = Path(scratch.name) / "legs", Path(scratch.name) / "combined"

    def setUp(self):
        self.reset()

    def reset(self):
        restore(self.legs, self.shot)
        after = snapshot(self.legs)
        if after != self.shot:
            drift = sorted(name for name in after.keys() | self.shot.keys() if after.get(name, False) != self.shot.get(name, False))
            self.fail(f"restore() did not put the good legs back: {drift}")
        shutil.rmtree(self.combined, ignore_errors=True)

    def artifact(self, leg):
        return artifact_dir(self.legs, leg)

    def evidence(self, leg):
        return self.artifact(leg) / module.EVIDENCE

    def edit(self, leg, name, change):
        edit_json(self.evidence(leg) / name, change)

    def rewrite_group(self, leg, label, number, change):
        """Change one group's retained go test events the way a careful forger would: the plan's digest of that output follows."""
        part = self.evidence(leg) / f"{label}-group-{number}.stdout"
        events = [json.loads(line) for line in part.read_text().splitlines()]
        part.write_text("".join(json.dumps(item) + "\n" for item in change(events)))
        self.edit(leg, label + "-groups.json", lambda plan: plan["groups"][number - 1].update(stdoutSHA256=module.digest(part)))

    def refuse(self, cases, commit=COMMIT):
        """Each case is (name, change, message): make the change to fresh good legs; the aggregator must refuse and publish nothing."""
        for name, change, message in cases:
            with self.subTest(name):
                self.reset()
                change()
                with self.assertRaisesRegex(RuntimeError, message):
                    module.aggregate(self.legs, commit, self.combined)
                self.assertFalse(self.combined.exists(), "a refused qualification must publish nothing")

    def test_good_legs_aggregate_into_the_evidence_the_single_lane_left(self):
        passed = module.aggregate(self.legs, COMMIT, self.combined)
        self.assertEqual(list(passed), list(module.PASSED_LABELS))
        self.assertEqual(passed, {label: len(ROOTS[label]) for label in module.PASSED_LABELS})
        evidence = self.combined / module.EVIDENCE
        summary = json.loads((evidence / "summary.json").read_text())
        self.assertEqual(list(summary), ["commit", "passed", "installedCLICommands", "engines", "childrenRemaining", "bdpHTTP", "pythonBDP"])
        self.assertEqual((summary["commit"], summary["passed"]), (COMMIT, passed))
        self.assertEqual([summary[key] for key in ("installedCLICommands", "engines", "childrenRemaining", "bdpHTTP", "pythonBDP")],
                         [10, ["embedded", "server"], 0, True, True])
        for label in ("storage", "cli"):
            parts = [(evidence / f"{label}-group-{n}.stdout").read_bytes() for n in range(1, module.GROUPED[label] + 1)]
            self.assertEqual((evidence / (label + ".stdout")).read_bytes(), b"".join(parts), "the combined output is the groups' output in group order")
            plan = json.loads((evidence / (label + "-groups.json")).read_text())
            self.assertIs(plan["complete"], True)
            self.assertTrue(all(group["passed"] for group in plan["groups"]))
        receipts = json.loads((evidence / "receipts.json").read_text())
        shutdowns = [r["label"] for r in receipts if r["label"].endswith("-server-shutdown")]
        self.assertEqual((len(shutdowns), len(set(shutdowns))), (19, 19), "one fresh server per go test run, and the capture server")
        self.assertEqual(len({r["label"] for r in receipts}), len(receipts), "no receipt is republished twice")
        for private in module.LEG_PRIVATE[:2]:
            self.assertFalse((evidence / private).exists(), f"{private} belongs to its leg")
        legs = json.loads((evidence / "legs.json").read_text())
        self.assertEqual((legs["commit"], legs["servers"], list(legs["legs"])), (COMMIT, 19, list(module.LEGS)))
        self.assertEqual(sorted((self.combined / "graph-c0-build").iterdir()),
                         [self.combined / "graph-c0-build/SHA256SUMS", self.combined / "graph-c0-build/build-manifest.txt"])
        self.assertTrue((self.combined / "bdp-client-build/client-build.json").is_file())

    def test_a_missing_host_record_never_fails_the_aggregate(self):
        for leg in module.LEGS:
            (self.evidence(leg) / "host.json").unlink()
        module.aggregate(self.legs, COMMIT, self.combined)

    def test_a_leg_artifact_that_is_missing_renamed_or_extra_is_refused(self):
        def rename(leg):
            return lambda: self.artifact(leg).rename(artifact_dir(self.legs, leg, OTHER_COMMIT))
        self.refuse([
            (f"{leg} cancelled, skipped or never uploaded", lambda leg=leg: shutil.rmtree(self.artifact(leg)), "leg artifacts present")
            for leg in module.LEGS
        ] + [
            (f"{leg} uploaded for another commit", rename(leg), "leg artifacts present") for leg in module.LEGS
        ] + [
            ("an unexpected fourth artifact", lambda: (self.legs / "graph-leg-extra").mkdir(), "leg artifacts present"),
            ("the combined artifact among the legs", lambda: (self.legs / ("graph-c0-" + COMMIT)).mkdir(), "leg artifacts present"),
        ])
        self.reset()
        with self.assertRaisesRegex(RuntimeError, "leg artifacts present"):
            module.aggregate(self.legs, OTHER_COMMIT, self.combined)

    def test_a_leg_that_did_not_finish_or_finished_as_another_is_refused(self):
        def swap(**change):
            return lambda: self.edit("storage-b", "leg.json", lambda record: record.update(change))
        self.refuse([
            (f"{leg} failed before its completion receipt", lambda leg=leg: (self.evidence(leg) / "leg.json").unlink(),
             "no completion receipt") for leg in module.LEGS
        ] + [
            ("a leg receipt for the other storage leg", swap(leg="storage-a"), "completion receipt is for another leg"),
            ("an incomplete leg receipt", swap(complete=False), "completion receipt is for another leg or incomplete"),
            ("a leg receipt that omits completeness", lambda: self.edit("storage-b", "leg.json", lambda record: record.pop("complete")),
             "completion receipt is for another leg or incomplete"),
            ("a leg receipt for another commit", swap(commit=OTHER_COMMIT), "receipt commit"),
            ("a leg receipt with other labels", swap(labels=["storage", "config"]), "receipt labels or groups differ from the leg set"),
            ("a leg receipt with other groups", swap(groups={"storage": [1, 2]}), "receipt labels or groups differ from the leg set"),
            ("evidence from another commit (source)", lambda: self.edit("rest", "source.json", lambda source: source.update(commit=OTHER_COMMIT)),
             "evidence commit differs"),
            ("evidence from another commit (head)", lambda: (self.evidence("rest") / "head.stdout").write_text(OTHER_COMMIT + "\n"),
             "evidence commit differs"),
        ])

    def test_group_coverage_across_the_storage_legs_is_exact(self):
        def plan(leg, change, label="storage"):
            return lambda: self.edit(leg, label + "-groups.json", change)
        self.refuse([
            ("group 4 not passed and its output gone", lambda: (
                self.edit("storage-a", "storage-groups.json", lambda p: p["groups"][3].update(passed=False)),
                (self.evidence("storage-a") / "storage-group-4.stdout").unlink()), "groups run across the legs are"),
            ("group 1 passed in both storage legs", plan("storage-b", lambda p: p["groups"][0].update(passed=True)), "duplicate group across legs"),
            ("a leg that ran another leg's group", lambda: (
                self.edit("storage-a", "storage-groups.json", lambda p: p["groups"][3].update(passed=False)),
                self.edit("storage-b", "storage-groups.json", lambda p: p["groups"][3].update(passed=True))), "ran storage groups"),
            ("a partial leg that calls its plan complete", plan("storage-a", lambda p: p.update(complete=True)), "plan differs from its discovery"),
            ("a plan of another package", plan("storage-a", lambda p: p.update(package="./internal/storage/other")), "plan differs from its discovery"),
            ("a plan of another selector", plan("storage-b", lambda p: p.update(selector="^(TestOther)$")), "plan differs from its discovery"),
            ("a plan whose source roots are not the discovery's", plan("storage-a", lambda p: p.update(sourceRoots=p["sourceRoots"][:-1])),
             "plan differs from its discovery"),
            ("a plan whose compiled roots are not the discovery's", plan("storage-b", lambda p: p.update(compiledRoots=p["compiledRoots"][:-1])),
             "plan differs from its discovery"),
            ("source and compiled roots that agree with each other and not with the discovery", plan("storage-a", lambda p: p.update(
                sourceRoots=p["sourceRoots"][:-1], compiledRoots=p["compiledRoots"][:-1])), "plan differs from its discovery"),
            ("a cmd/bd plan whose compiled roots are not the discovery's", plan("rest", lambda p: p.update(compiledRoots=p["compiledRoots"][:-1]), "cli"),
             "plan differs from its discovery"),
            ("a plan of the wrong size", plan("storage-a", lambda p: p["groups"].pop()), "plan has the wrong number of groups"),
            ("a group that is not the partition's", plan("storage-a", lambda p: p["groups"][0].update(roots=["TestMerge"])),
             "group 1 differs from the partition"),
            ("a group labelled as another", plan("storage-a", lambda p: p["groups"][0].update(label="storage-group-2")),
             "group 1 differs from the partition"),
            ("a group whose selector is not its roots'", plan("storage-b", lambda p: p["groups"][1].update(selector="^(TestOther)$")),
             "group 2 differs from the partition"),
            ("group output that is not the plan's", lambda: (self.evidence("storage-a") / "storage-group-1.stdout").write_text("{}\n"),
             "group 1 output differs from its plan"),
            ("a passed group whose output is gone", lambda: (self.evidence("storage-b") / "storage-group-2.stdout").unlink(),
             "group 2 output does not match its plan"),
            ("a cmd/bd group not passed", plan("rest", lambda p: p["groups"][2].update(passed=False), "cli"), "groups run across the legs are"),
        ])

    def test_every_root_must_run_exactly_once_with_its_required_proof(self):
        def without(root):
            return lambda events: [e for e in events if e.get("Test", "").split("/")[0] != root]

        def twice(root):
            return lambda events: events[:-1] + [e for e in events if e.get("Test", "").split("/")[0] == root] + events[-1:]

        def without_subtests(events):
            return [e for e in events if "/" not in e.get("Test", "")]

        self.refuse([
            ("a root that never ran", lambda: self.rewrite_group("storage-a", "storage", 4, without("TestOrder")),
             "test discovery/execution mismatch"),
            ("a root that ran twice", lambda: self.rewrite_group("storage-a", "storage", 4, twice("TestOrder")), "duplicate test run"),
            ("a root of another group", lambda: self.rewrite_group("storage-b", "storage", 2, lambda events: events[:-1] + [
                event("run", "TestOrder"), event("pass", "TestOrder")] + events[-1:]), "unexpected test or subtest"),
            ("a failed root", lambda: self.rewrite_group("storage-a", "storage", 4, lambda events: events[:-1] + [event("fail", "TestOrder")] + events[-1:]),
             "required test did not pass"),
            ("a skipped root", lambda: self.rewrite_group("storage-a", "storage", 4, lambda events: events[:-1] + [event("skip", "TestOrder")] + events[-1:]),
             "required test did not pass"),
            ("a storage root without its engine tuples", lambda: self.rewrite_group("storage-a", "storage", 1, without_subtests),
             "storage: required engine proof absent"),
            ("a cmd/bd root without its engine tuples", lambda: self.rewrite_group("rest", "cli", 1, without_subtests),
             "cli: required engine proof absent"),
            ("a cmd/bd root that never ran", lambda: self.rewrite_group("rest", "cli", 2, without(ROOTS["cli"][1])),
             "test discovery/execution mismatch"),
            ("a package that never completed", lambda: self.rewrite_group("storage-a", "storage", 4, lambda events: events[:-1]),
             "missing successful package completion"),
        ])

        # check_grouped asks twice that every root ran once: group by group against the partition, as above, and again over the
        # runs of every leg together. The first refuses first, so this takes it away and holds the second alone.
        roots_run = lambda events, expected: len({e["Test"] for e in events if e.get("Action") == "run" and "/" not in e.get("Test", "")})
        once = "a root did not run exactly once across the legs"
        with mock.patch.object(module, "verify_tests", roots_run):
            self.refuse([
                ("a root that never ran, group by group unchecked", lambda: self.rewrite_group("storage-a", "storage", 4, without("TestOrder")), once),
                ("a root that ran twice, group by group unchecked", lambda: self.rewrite_group("storage-a", "storage", 4, twice("TestOrder")), once),
                ("a root of another group, group by group unchecked", lambda: self.rewrite_group("storage-b", "storage", 2, lambda events: events[:-1] + [
                    event("run", "TestOrder"), event("pass", "TestOrder")] + events[-1:]), once),
                ("a cmd/bd root that never ran, group by group unchecked", lambda: self.rewrite_group("rest", "cli", 2, without(ROOTS["cli"][1])), once),
            ])

    def test_single_run_labels_are_held_to_their_own_evidence(self):
        rest = lambda: self.evidence("rest")
        self.refuse([
            ("a label with no output", lambda: (rest() / "graphpatch.stdout").unlink(), "missing evidence file"),
            ("a label whose root did not run", lambda: (rest() / "bdpwire.stdout").write_text(
                json.dumps(event("pass")) + "\n"), "test discovery/execution mismatch"),
            ("graphread without its engine tuples", lambda: (rest() / "graphread.stdout").write_text("".join(
                json.dumps(e) + "\n" for e in [event("run", ROOTS["graphread"][0]), event("pass", ROOTS["graphread"][0]), event("pass")])),
             "graphread: required engine proof absent"),
            ("a count the receipt does not share", lambda: self.edit("rest", "leg.json", lambda record: record["passed"].update(graphpatch=99)),
             "graphpatch: leg receipt differs from its evidence"),
            ("a cmd/bd count the receipt does not share", lambda: self.edit("rest", "leg.json", lambda record: record["passed"].update(cli=1)),
             "cli: leg receipt differs from its evidence"),
            ("a discovery digest the receipt does not share", lambda: self.edit(
                "rest", "leg.json", lambda record: record["discovery"].update(graphpatch="0" * 64)),
             "graphpatch: leg receipt differs from its evidence"),
        ])

    def test_discovery_must_match_its_digest_its_listing_the_receipt_and_the_other_leg(self):
        def forged(label, roots):
            """The record, the compiled listing and the receipt's digest all say `roots`, so only the roots themselves are wrong."""
            def change():
                digest = module.roots_digest(roots)
                self.edit("rest", label + "-discovery.json", lambda found: found.update(roots=roots, sha256=digest))
                (self.evidence("rest") / (label + "-discovery.stdout")).write_text("".join(root + "\n" for root in roots))
                self.edit("rest", "leg.json", lambda record: record["discovery"].update({label: digest}))
            return change

        self.refuse([
            ("no roots discovered, agreed everywhere", forged("bdpwire", []), "bdpwire: discovery digest does not match its roots"),
            ("a root discovered twice, agreed everywhere", forged("bdpwire", ["TestWire", "TestWire"]),
             "bdpwire: discovery digest does not match its roots"),
            ("roots out of order, agreed everywhere", forged("bdpwire", ["TestWire2", "TestWire"]),
             "bdpwire: discovery digest does not match its roots"),
        ])
        self.refuse([
            ("a digest that is not the roots'", lambda: self.edit("storage-a", "storage-discovery.json", lambda d: d.update(sha256="0" * 64)),
             "discovery digest does not match its roots"),
            ("a root dropped from a discovery record", lambda: self.edit("storage-a", "storage-discovery.json", lambda d: d["roots"].pop()),
             "discovery digest does not match its roots"),
            ("a listing that is not the record's", lambda: (self.evidence("rest") / "cli-discovery.stdout").write_text("TestOne\n"),
             "retained compiled listing differs from the discovery record"),
            ("a receipt digest that is not the evidence's", lambda: self.edit(
                "storage-b", "leg.json", lambda record: record["discovery"].update(storage="0" * 64)),
             "receipt discovery digest differs from its evidence"),
            ("storage legs that discovered differently", lambda: self.edit(
                "storage-b", "storage-discovery.json", lambda d: d.update(package="./internal/storage/other")),
             "discovery differs between legs"),
        ])

    def test_the_legs_must_have_run_one_binary_and_this_runner_script(self):
        def sums(leg, text):
            return lambda: (self.artifact(leg) / "graph-c0-build/SHA256SUMS").write_text(text)

        def every_leg(change):
            return lambda: [change(leg) for leg in module.LEGS]

        other = "1" * 64
        self.refuse([
            ("another runner script in one leg", lambda: self.edit("storage-b", "source.json", lambda s: s.update(runnerSHA256=other)),
             "different binaries or runner scripts"),
            ("another binary in one leg", lambda: self.edit("storage-a", "source.json", lambda s: s.update(binarySHA256=other)),
             "different binaries or runner scripts"),
            ("another Dolt in one leg", lambda: self.edit("rest", "source.json", lambda s: s.update(doltSHA256=other)),
             "different binaries or runner scripts"),
            ("another runner script than the aggregator's", every_leg(
                lambda leg: self.edit(leg, "source.json", lambda s: s.update(runnerSHA256=other))),
             "different runner script than the aggregator"),
            ("other build sums in one leg", sums("storage-b", f"{other}  bd-linux-gms-pure\n"), "different build artifacts"),
            ("build sums that are not the binary's", every_leg(
                lambda leg: (self.artifact(leg) / "graph-c0-build/SHA256SUMS").write_text(f"{other}  bd-linux-gms-pure\n")),
             "different build artifacts"),
            ("a build of another commit", every_leg(lambda leg: (self.artifact(leg) / "graph-c0-build/build-manifest.txt").write_text(
                (self.artifact(leg) / "graph-c0-build/build-manifest.txt").read_text().replace(COMMIT, OTHER_COMMIT))),
             "different build artifacts"),
            ("a leg without its build record", lambda: (self.artifact("storage-a") / "graph-c0-build/build-manifest.txt").unlink(),
             "missing evidence file"),
        ])

    def test_commands_and_servers_must_have_run_cleanly_once_per_group(self):
        def receipts(leg, change):
            return lambda: self.edit(leg, "receipts.json", change)

        def shutdown(label, **change):
            return lambda items: [item.update(change) for item in items if item["label"] == label + "-server-shutdown"]

        self.refuse([
            ("a command that exited nonzero", receipts("storage-a", lambda items: items[0].update(exit=1)), "did not exit cleanly"),
            ("a command that recorded a failure", receipts("rest", lambda items: items[0].update(failure="TimeoutExpired")),
             "did not exit cleanly"),
            ("a group without a server receipt", receipts("storage-a", lambda items: items.__setitem__(
                slice(None), [i for i in items if i["label"] != "storage-group-1-server-shutdown"])), "not one fresh server per group"),
            ("a group that shared a server", receipts("storage-a", lambda items: items.append(dict(items[-1]))), "not one fresh server per group"),
            ("a server that had to be killed", receipts("storage-a", shutdown("storage-group-4", forced=True)), "unclean server shutdown"),
            ("a server whose process group survived", receipts("storage-b", shutdown("storage-group-2", groupGone=False)),
             "unclean server shutdown"),
            ("a server whose port stayed open", receipts("rest", shutdown("capture", portClosed=False)), "unclean server shutdown"),
        ])

    def test_a_server_shared_or_missing_across_the_legs_is_refused(self):
        # Ruling be-jmdn71 I1: the aggregator checks the total server count and that no group shared a server. check_leg holds
        # each leg to its own servers and refuses first, so each case has check_leg report the servers it is told to, and
        # holds the aggregator's own count alone.
        real = module.check_leg

        def reporting(change):
            def check_leg(leg, evidence, commit):
                record, source, receipts, servers = real(leg, evidence, commit)
                return record, source, receipts, change(leg, servers)
            return check_leg

        for name, change in [
                ("a server two legs both started", lambda leg, servers: ["storage-group-1" if s == "storage-group-2" else s for s in servers]),
                ("a server fewer than the legs start", lambda leg, servers: servers[1:] if leg == module.REST_LEG else servers),
                ("a server more than the legs start", lambda leg, servers: [*servers, "storage-group-9"] if leg == "storage-a" else servers)]:
            with mock.patch.object(module, "check_leg", reporting(change)):
                self.refuse([(name, lambda: None, "a server was shared or missing across the legs")])

    def test_evidence_or_a_receipt_that_two_legs_both_hold_is_refused_unless_it_is_the_shared_record(self):
        def stray(*legs):
            return lambda: [(self.evidence(leg) / "stray.json").write_text("{}\n") for leg in legs]

        def receipt_in(*legs, label):
            return lambda: [self.edit(leg, "receipts.json", lambda items: items.append(dict(label=label, exit=0))) for leg in legs]

        self.refuse([
            ("a file both storage legs wrote", stray("storage-a", "storage-b"), "stray.json is in both storage-a and storage-b"),
            ("a file the rest leg and a storage leg wrote", stray("rest", "storage-b"), "stray.json is in both rest and storage-b"),
            ("a receipt both storage legs hold", receipt_in("storage-a", "storage-b", label="stray"), "receipt stray is in both storage-a and storage-b"),
            ("a receipt of the rest leg's in a storage leg", receipt_in("storage-a", label="bdpwire"), "receipt bdpwire is in both rest and storage-a"),
        ])
        # What every leg writes is the same record in each, and the combined artifact holds it once.
        self.reset()
        module.aggregate(self.legs, COMMIT, self.combined)
        for leg in module.LEGS:
            self.assertTrue((self.evidence(leg) / "head.stdout").is_file(), "every leg holds the shared record")
        self.assertEqual(len([r for r in json.loads((self.combined / module.EVIDENCE / "receipts.json").read_text()) if r["label"] == "head"]), 1)

    def test_the_rest_leg_must_show_the_installed_cli_http_and_python_facts_and_its_client_build(self):
        self.refuse([
            ("facts removed", lambda: self.edit("rest", "leg.json", lambda record: record.pop("facts")), "receipt lacks the installed CLI"),
            ("another engine set", lambda: self.edit("rest", "leg.json", lambda record: record["facts"].update(engines=["embedded"])),
             "receipt lacks the installed CLI"),
            ("children remaining", lambda: self.edit("rest", "leg.json", lambda record: record["facts"].update(childrenRemaining=1)),
             "receipt lacks the installed CLI"),
            ("no client build record", lambda: (self.artifact("rest") / "bdp-client-build/client-build.json").unlink(), "missing evidence file"),
            ("no client build at all", lambda: shutil.rmtree(self.artifact("rest") / "bdp-client-build"), "missing evidence file"),
        ])


if __name__ == "__main__":
    unittest.main()
