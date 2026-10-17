#!/usr/bin/env python3
"""Required graph/BDP Read proof: real engines, exact CLI and public clients.

Includes the C0 captures and every discovered graph test, including the mixed
Issue/Memory/Link installed workflow, HTTP security and authenticated independent
Node/Python BDP reads. This is not full Memory, History or HTTP Write proof.
The caller supplies the ordinary released Dolt binary and CI Build Artifacts.
All databases and process groups belong to this run; no existing server is used.

The proof runs as three legs on separate runners (--leg storage-a, storage-b, rest) and
--leg aggregate then checks them as one: the legs together must prove exactly what the
single lane did. The constants below fix the split.
"""
import argparse
import collections
import contextlib
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import socket
import subprocess
import sys
import time

# The leg split (be-vm7gou, ruling be-jmdn71 G7). This one block fixes the group counts, the legs and the whole
# groups each storage leg runs. The verifier test asserts that it equals the workflow's leg ids and that the storage
# legs partition groups 1..STORAGE_GROUPS, so a six-group response or a third storage leg is a change to this block
# and to fork-qualification.yml and to nothing else.
STORAGE_GROUPS = 4  # go test groups of ./internal/storage/graphstore (sorted round-robin partition)
CLI_GROUPS = 3  # go test groups of ./cmd/bd; all of them run in the rest leg
STORAGE_LEGS = {"storage-a": (1, 4), "storage-b": (2, 3)}  # leg -> the whole groups it runs
REST_LEG = "rest"
LEGS = (*STORAGE_LEGS, REST_LEG)
AGGREGATE = "aggregate"
GROUPED = {"storage": STORAGE_GROUPS, "cli": CLI_GROUPS}  # label -> its number of groups
# Every label of summary.json's "passed", in the order the single lane ran them.
PASSED_LABELS = ("graphpatch", "storage", "config", "query-config", "utc-issueops", "utc-db", "utc-types",
                 "agent-template", "graph-claude-setup", "cli", "bdpwire", "graphread", "graph-http")
REST_LABELS = tuple(label for label in PASSED_LABELS if label != "storage")
LEG_LABELS = {**{leg: ("storage",) for leg in STORAGE_LEGS}, REST_LEG: REST_LABELS}
LEG_GROUPS = {**{leg: {"storage": numbers} for leg, numbers in STORAGE_LEGS.items()},
              REST_LEG: {"cli": tuple(range(1, CLI_GROUPS + 1))}}  # leg -> label -> the groups it runs
CAPTURE_SERVER = "capture"  # the rest leg's one shared server for the HTTP/BDP capture and smoke
# What the rest leg proves beyond go test, as summary.json states it.
REST_FACTS = dict(installedCLICommands=10, engines=["embedded", "server"], childrenRemaining=0, bdpHTTP=True, pythonBDP=True)
EVIDENCE = "graph-c0-evidence"  # each leg artifact's evidence directory; the combined artifact keeps the name
LEG_ARTIFACT = "graph-leg-{leg}-{commit}"  # the name fork-qualification.yml uploads a leg's artifact under and downloads it by

# Subtests that must pass in their package's own run. A storage leg holds only some of the groups, so the storage
# tuples are checked across the legs by the aggregator; every tuple is checked there.
STORAGE_REQUIRED = tuple("TestLinkUnlinkDefaultSourceReadBudget/" + engine + "/" + case
                         for engine in ("embedded", "server") for case in ("id-default", "pair-default", "id-explicit"))
CLI_REQUIRED = (
    "TestGraphPreviewAgentInstructionsWorkflow/embedded/fresh",
    "TestGraphPreviewAgentInstructionsWorkflow/embedded/shared-file",
    "TestGraphPreviewAgentInstructionsWorkflow/embedded/skip-agents",
    "TestGraphPreviewAgentInstructionsWorkflow/embedded/full-profile-refusal",
    "TestGraphPreviewAgentInstructionsWorkflow/server/fresh",
    "TestGraphPreviewAgentInstructionsWorkflow/server/shared-file",
    "TestGraphPreviewAgentInstructionsWorkflow/server/skip-agents",
    "TestGraphPreviewAgentInstructionsWorkflow/server/full-profile-refusal",
    "TestGraphPreviewUsabilityWorkflow/embedded", "TestGraphPreviewUsabilityWorkflow/server",
    "TestGraphPreviewClaudeStopWorkflow/embedded", "TestGraphPreviewClaudeStopWorkflow/server",
    "TestGraphPreviewClaudeStopAutoInit/embedded", "TestGraphPreviewClaudeStopAutoInit/server",
    "TestGraphPreviewClaudeStopAutoInitPreflight",
    "TestGraphPreviewClaudeStopAutoInitRefusalMessages/global-plugin", "TestGraphPreviewClaudeStopAutoInitRefusalMessages/claude-symlink",
    "TestGraphPreviewClaudeStopInstallFailure/embedded", "TestGraphPreviewClaudeStopInstallFailure/server",
    "TestGraphPreviewClaudeStopSkipAgentsAlone/embedded", "TestGraphPreviewClaudeStopSkipAgentsAlone/server",
    "TestGraphPreviewClaudeStopHookAdmissionNonBlocking/embedded", "TestGraphPreviewClaudeStopHookAdmissionNonBlocking/server",
    "TestGraphPreviewClaudeStopProfileRefusal/embedded/missing", "TestGraphPreviewClaudeStopProfileRefusal/embedded/minimal", "TestGraphPreviewClaudeStopProfileRefusal/embedded/stale",
    "TestGraphPreviewClaudeStopProfileRefusal/server/missing", "TestGraphPreviewClaudeStopProfileRefusal/server/minimal", "TestGraphPreviewClaudeStopProfileRefusal/server/stale",
    "TestGraphPreviewCompatibilityDefaultsWorkflow/embedded", "TestGraphPreviewCompatibilityDefaultsWorkflow/server",
    "TestGraphPreviewIssueAuthoringWorkflow/embedded", "TestGraphPreviewIssueAuthoringWorkflow/server",
    "TestGraphPreviewIssueAppendWorkflow/embedded", "TestGraphPreviewIssueAppendWorkflow/server",
    "TestGraphPreviewIssueClaimWorkflow/embedded", "TestGraphPreviewIssueClaimWorkflow/server",
    "TestGraphPreviewIssueAssignmentWorkflow/embedded", "TestGraphPreviewIssueAssignmentWorkflow/server",
    "TestGraphPreviewMixedCoreWorkflow/embedded", "TestGraphPreviewMixedCoreWorkflow/server",
    "TestGraphPreviewMemoryDeleteWorkflow/embedded", "TestGraphPreviewMemoryDeleteWorkflow/server",
    "TestGraphPreviewQueryWorkflow/embedded", "TestGraphPreviewQueryWorkflow/server",
    "TestGraphPreviewMemoryReadsWorkflow/embedded", "TestGraphPreviewMemoryReadsWorkflow/server",
    "TestGraphPreviewPropertiesPatchWorkflow/embedded", "TestGraphPreviewPropertiesPatchWorkflow/server")
GRAPHREAD_REQUIRED = ("TestAuthoritativeRecordsProjectToPublicWire/embedded", "TestAuthoritativeRecordsProjectToPublicWire/server")
GRAPH_HTTP_REQUIRED = ("TestGraphReadHTTPAuthorityAndSecurity/issue-priority-assignment",
                       "TestGraphReadHTTPAuthorityAndSecurity/issue-append-notes")
REQUIRED = {"storage": STORAGE_REQUIRED, "cli": CLI_REQUIRED, "graphread": GRAPHREAD_REQUIRED, "graph-http": GRAPH_HTTP_REQUIRED}

OWNER_CLEANUP_GRACE = 30
GO_TEST_TIMEOUT = 900  # seconds; the -timeout=15m every go test group is given
SLOW_GROUP_SECONDS = GO_TEST_TIMEOUT * 6 // 10
TAIL_LINES = 40
SERVER_READY_TIMEOUT = 30
SERVER_STOP_TIMEOUT = 15
# Deadlines for the HTTP/BDP smoke, in seconds. Each outlasts the one before it: one
# command < the smoke's whole run < the wrapper that kills it from outside, so a slow runner
# ends in the smoke's own labelled failure and never in an unlabelled outer kill. The
# smoke's first command is a server-backed bd init: 35 s of a 45 s smoke on a fast runner,
# and 34 s, 58 s and 65 s for the qualification's own init on fast and slow runners. One
# command gets 180 s, about 2.8x the slowest of those. The total covers a full-length init
# plus the rest of the smoke at the same slowdown (about 230 s) with room to spare.
SMOKE_COMMAND_TIMEOUT = 180
SMOKE_TOTAL_TIMEOUT = 600
SMOKE_WRAPPER_TIMEOUT = 660  # total + 60 s for the smoke to reap its children and report


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def group_exited(pid, timeout=5):
    # A command may exit just before its already-exiting child is reaped.
    # Allow a bounded grace period, but never count forced cleanup as success.
    deadline = time.monotonic() + timeout
    while True:
        try:
            os.killpg(pid, 0)
        except ProcessLookupError:
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.05)


def verify_tests(events, expected):
    runs, passes = set(), set()
    for event in events:
        name, action = event.get("Test"), event.get("Action")
        require(action not in ("fail", "skip"), f"required test did not pass: {event}")
        if name:
            require(name.split("/", 1)[0] in expected, f"unexpected test or subtest: {name}")
            if action == "run":
                require(name not in runs, f"duplicate test run: {name}")
                runs.add(name)
            elif action == "pass":
                require(name in runs and name not in passes, f"unmatched test pass: {name}")
                passes.add(name)
    roots = {name for name in runs if "/" not in name}
    require(expected and roots == expected and runs == passes,
            f"test discovery/execution mismatch: expected roots={sorted(expected)}, runs={sorted(runs)}, passes={sorted(passes)}")
    require(any(e.get("Action") == "pass" and not e.get("Test") for e in events),
            "missing successful package completion")
    return len(roots)


def partition_required_tests(names, groups):
    require(names and len(names) == len(set(names)), "empty or duplicate required test roots")
    require(type(groups) is int and 1 < groups <= len(names), "required groups must be nonempty")
    ordered = sorted(names)
    selections = [ordered[index::groups] for index in range(groups)]
    union = [name for group in selections for name in group]
    require(len(union) == len(set(union)) and set(union) == set(names), "incomplete required test partition")
    return selections


def roots_digest(names):
    return hashlib.sha256(("\n".join(sorted(names)) + "\n").encode()).hexdigest()


def host_info():
    """The runner's hardware, for reading leg latency. Named fields only, never an environment dump; never gates."""
    def read(path, pattern):
        try:
            found = re.search(pattern, Path(path).read_text(), re.MULTILINE)
        except OSError:
            return None
        return found and found.group(1).strip()

    return dict(nproc=len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else os.cpu_count(),
                cpuModel=read("/proc/cpuinfo", r"^model name\s*:\s*(.+)$"),
                memTotal=read("/proc/meminfo", r"^MemTotal:\s*(.+)$"),
                kernel=platform.release(), runnerImageVersion=os.environ.get("ImageVersion"))


def check_leg_constants():
    """The split must run every label, and every group of a grouped label exactly once, across the legs."""
    require(set(LEGS) == set(LEG_LABELS) == set(LEG_GROUPS), "every leg needs its labels and groups")
    for leg in LEGS:
        require(set(LEG_GROUPS[leg]) == {label for label in LEG_LABELS[leg] if label in GROUPED},
                f"{leg}: groups must be listed for exactly the grouped labels it runs")
    for label in PASSED_LABELS:
        legs = [leg for leg in LEGS if label in LEG_LABELS[leg]]
        if label in GROUPED:
            numbers = sorted(n for leg in legs for n in LEG_GROUPS[leg][label])
            require(numbers == list(range(1, GROUPED[label] + 1)), f"{label}: the legs must run groups 1..{GROUPED[label]} once each")
        else:
            require(len(legs) == 1, f"{label}: must run in exactly one leg")


def expected_servers(leg):
    """The fresh servers one leg starts: one per go test run (one per group of a grouped label) and the capture server."""
    servers = []
    for label in LEG_LABELS[leg]:
        servers += [f"{label}-group-{n}" for n in LEG_GROUPS[leg][label]] if label in GROUPED else [label]
    return servers + ([CAPTURE_SERVER] if leg == REST_LEG else [])


def cleanup_registered_groups(path):
    """Fallback for an interrupted/failed owner, including dead group leaders."""
    active = set()
    if path is not None and path.exists():
        for line in path.read_text().splitlines():
            event = json.loads(line)
            require(type(event.get("pid")) is int and event["pid"] > 1, "invalid owned process identity")
            require(event.get("action") in ("start", "end"), "invalid process registry event")
            if event["action"] == "start":
                active.add(event["pid"])
            else:
                active.discard(event["pid"])
    forced = []
    for pid in active:
        try:
            os.killpg(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    for pid in active:
        if not group_exited(pid, timeout=5):
            forced.append(pid)
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        require(group_exited(pid, timeout=5), f"owned process group survived cleanup: {pid}")
    return dict(registeredGroups=len(active), forcedGroups=forced, groupsGone=True)


def verify_clean_receipt(receipt, listener=False):
    require(receipt.get("exit_code") == 0 and receipt.get("failure") is None
            and receipt.get("forced") is False and receipt.get("group_gone") is True,
            "HTTP capture process did not finish cleanly")
    if listener:
        require(receipt.get("port_closed") is True, "HTTP listener remained open")


def verify_http_capture(root, binary_hash):
    summary = json.loads((root / "summary.json").read_text())
    require(summary.get("passed") is True and summary.get("failure") is None
            and summary.get("active_children") == 0 and summary.get("cli_commands") == 16,
            "incomplete installed HTTP capture")
    require(summary.get("client_pin") == "53bdbd03136875f952af184fce7b3c7af8f74e96"
            and summary.get("installed_binary_sha256") == binary_hash, "HTTP source/binary provenance mismatch")
    require(summary.get("python") == {"passed": True, "beads": 4, "limit": 1,
            "authenticated": True, "mechanism": "unchanged standard-library example over BDP HTTP"},
            "Python BDP consumer proof missing")
    require(summary.get("python_pages") == 4, "Python continuation observations missing")
    processes = list(root.glob("*/receipt.json"))
    require(len(processes) == 21, "missing seed/delete/serve/client/Python process receipt")
    for path in processes:
        receipt = json.loads(path.read_text())
        verify_clean_receipt(receipt, listener=path.parent.name == "serve")
        for stream in ("stdout", "stderr"):
            require(digest(path.parent / (stream + ".log")) == receipt[stream + "_sha256"],
                    "HTTP process output hash differs")
    client = json.loads((root / "client-results.json").read_text())
    require(client.get("passed") is True and client.get("failure") is None
            and len(client.get("checks", [])) == 13, "incomplete public-client checks")
    network = json.loads((root / "client-network.json").read_text())
    require(network and client["requests"] == len(network), "HTTP network receipts incomplete")
    for observation in network:
        require(observation["url"].startswith(summary["scope"])
                and re.fullmatch(r"[0-9a-f]{64}", observation["bodySha256"]), "invalid HTTP observation")
    verify_issue_append_capture(root, summary, binary_hash, network)
    verify_issue_authoring_capture(root, summary, binary_hash, network)
    verify_memory_deletion_capture(root, summary)


def verify_issue_append_capture(root, summary, binary_hash, network):
    """Keep the real writer receipts, public records and raw HTTP observations linked."""
    def data(relative):
        return json.loads((root / relative).read_text())
    artifacts = data("client-artifacts.json")
    notes = artifacts["issueNotes"]
    scope, actor = summary["scope"], "bdp-read-note-holder"
    work_id = scope + "beads/work"
    before, claimed, after = notes["before"], notes["claimed"], notes["after"]
    require(before == artifacts["issueEdit"]["after"], "append preimage lost preceding Issue-edit proof")
    require(all(item["id"] == work_id and item["type"] == before["type"] for item in (claimed, after)),
            "append changed canonical Issue identity/Type")
    expected_claim = dict(before["properties"])
    for key in ("status", "assignee", "started_at", "updated_at", "lease_expires_at", "heartbeat_at", "lease_granted_node"):
        if key in claimed["properties"]:
            expected_claim[key] = claimed["properties"][key]
        else:
            expected_claim.pop(key, None)
    require(claimed["properties"] == expected_claim and claimed["properties"]["status"] == "in_progress"
            and claimed["properties"]["assignee"] == actor and claimed["properties"].get("notes", "") == ""
            and claimed["revision"] != before["revision"], "incomplete installed claim precondition")
    require(notes["noopRecord"] == claimed and notes["noopMutation"]["result"]["changed"] is False
            and notes["noopMutation"]["result"]["issue"] == notes["claimMutation"]["result"]["issue"],
            "empty append was not a complete semantic no-op")
    text = "  Progress — 雪\r\nsecond line\t  "
    require(notes["appendText"] == text and after["properties"] == dict(claimed["properties"],
            notes=text, updated_at=after["properties"]["updated_at"]), "appended properties/lease changed or bytes lost")
    require(after["revision"] != claimed["revision"] and after["attribution"]["principal"] == actor
            and after["ownedLinks"] == claimed["ownedLinks"] == before["ownedLinks"], "append ownership/revision proof missing")
    require(notes["properties"] == after["properties"] and notes["inventory"]["next"] is None
            and len(notes["inventory"]["items"]) == 4
            and [item for item in notes["inventory"]["items"] if item["id"] == work_id] == [after]
            and notes["incident"] == artifacts["issueEdit"]["incidentAfter"], "append BDP views disagree")
    require(notes["etagBefore"] and notes["etagAfter"] and notes["etagBefore"] != notes["etagAfter"],
            "append did not change ETag")
    binary = data("02-memory-create/receipt.json")["argv"][0]
    commands = (
        ("client-cli-issue-claim", "claimMutation", ["update", work_id, "--claim", "--actor", actor, "--json"], True, claimed),
        ("client-cli-issue-append-noop", "noopMutation", ["update", work_id, "--append-notes=", "--if-revision", claimed["revision"], "--actor", actor, "--json"], False, claimed),
        ("client-cli-issue-append", "appendMutation", ["update", work_id, "--append-notes", text, "--if-revision", claimed["revision"], "--actor", actor, "--json"], True, after),
    )
    for name, key, args, changed, record in commands:
        receipt, mutation = data(name + ".json"), data(name + ".stdout.log")
        require(receipt["argv"] == [binary, *args] and receipt["binarySha256"] == binary_hash
                and receipt["exitCode"] == 0 and receipt["signal"] is None, "append CLI source/exit/guard differs")
        for stream in ("stdout", "stderr"):
            require(digest(root / (name + "." + stream + ".log")) == receipt[stream + "Sha256"],
                    "append CLI output hash differs")
        require(mutation == notes[key] and mutation["schemaVersion"] == 1 and mutation["preview"] is True
                and mutation["result"]["changed"] is changed and mutation["result"]["issue"]["id"] == work_id
                and mutation["result"]["issue"]["revision"] == record["revision"], "append CLI/public record mismatch")
    for label, index_key, status, etag, record in (
        ("before", "claimNetworkIndex", 200, notes["etagBefore"], claimed),
        ("noop", "noopNetworkIndex", 304, notes["etagBefore"], None),
        ("after", "appendNetworkIndex", 200, notes["etagAfter"], after),
    ):
        index = notes[index_key]
        require(type(index) is int and 0 <= index < len(network), "append HTTP observation index missing")
        observation = network[index]
        body = root / ("client-issue-notes-" + label + ".body")
        require(observation["url"] == work_id and observation["method"] == "GET" and observation["status"] == status
                and observation["headers"].get("etag") == etag and observation["bodyBytes"] == body.stat().st_size
                and observation["bodySha256"] == digest(body), "append HTTP status/ETag/raw bytes disagree")
        require((body.read_bytes() == b"") if record is None else json.loads(body.read_bytes()) == record,
                "append raw HTTP body differs from public record")
    require(notes["claimNetworkIndex"] < notes["noopNetworkIndex"] < notes["appendNetworkIndex"],
            "append HTTP observations are not ordered")



def verify_issue_authoring_capture(root, summary, binary_hash, network):
    """Independently connect initial CLI fields, guarded edits and BDP observations."""
    def data(name):
        return json.loads((root / name).read_text())
    proof = data("client-artifacts.json")["issueAuthoring"]
    before, edited, after = (proof[key] for key in ("before", "edited", "after"))
    resource = summary["scope"] + "beads/prereq"
    initial = {"design": "Initial design — 雪", "acceptance_criteria": "Ready\r\n",
               "assignee": "author", "estimated_minutes": 0, "external_ref": " tracker #1 ",
               "spec_id": " spec ", "notes": " Initial\r\n雪 ", "due_at": "2000-01-01T00:00:00Z"}
    require(proof["initialFields"] == initial and all(before["properties"].get(k) == v for k, v in initial.items()),
            "initial authoring fields/nullable zero/literal notes missing")
    require(not before["properties"].get("lease_expires_at"), "initial assignment unexpectedly claimed")
    seed = [path for path in root.glob("*-issue-create/stdout.log") if data(str(path.relative_to(root)))["result"]["id"] == resource]
    require(len(seed) == 1, "missing unique initial authoring CLI seed")
    seeded = json.loads(seed[0].read_text())["result"]
    require(seeded["properties"] == before["properties"] and seeded["revision"] == before["revision"],
            "initial CLI/BDP authoring records differ")
    expected = dict(before["properties"], design="Revised", acceptance_criteria="Accepted", estimated_minutes=45,
                    external_ref="revised", spec_id="revised spec", due_at="2100-01-01T00:00:00Z", notes=initial["notes"] + "\nProgress",
                    updated_at=edited["properties"]["updated_at"])
    require(edited["properties"] == expected and edited["revision"] != before["revision"], "combined edit changed unrelated authoring properties")
    expected = dict(edited["properties"], estimated_minutes=0, updated_at=after["properties"]["updated_at"])
    expected.pop("external_ref")
    expected.pop("spec_id")
    expected.pop("due_at")
    require(after["properties"] == expected and after["revision"] != edited["revision"], "nullable clear lost notes or unrelated fields")
    require(all(item["id"] == resource and item["type"] == before["type"] and item["ownedLinks"] == before["ownedLinks"] for item in (edited, after)),
            "authoring changed identity/type/ownership")
    actor = "bdp-read-author"
    require(all(item["attribution"]["principal"] == actor for item in (edited, after)), "authoring attribution missing")
    require(proof["properties"] == after["properties"] and proof["inventory"]["next"] is None
            and len(proof["inventory"]["items"]) == 4
            and [item for item in proof["inventory"]["items"] if item["id"] == resource] == [after], "authoring BDP current/properties/inventory disagree")
    require([item for item in data("python-after-delete/stdout.log") if item["id"] == resource] == [after],
            "unchanged Python consumer lost final authored Issue")
    require(proof["noop"]["result"]["issue"] == proof["edit"]["result"]["issue"], "scalar noop changed Issue")
    binary = data("02-memory-create/receipt.json")["argv"][0]
    edits = ["--estimate=45", "--external-ref=revised", "--spec-id=revised spec", "--due=2100-01-01T00:00:00Z"]
    for name, key, flags, predecessor, changed, record in (
        ("edit", "edit", edits + ["--design=Revised", "--acceptance=Accepted", "--append-notes=Progress"], before, True, edited),
        ("noop", "noop", edits, edited, False, edited),
        ("clear", "clear", ["--estimate=0", "--external-ref=", "--spec-id=", "--due="], edited, True, after),
    ):
        prefix = "client-cli-issue-authoring-" + name
        receipt, mutation = data(prefix + ".json"), data(prefix + ".stdout.log")
        require(receipt["argv"] == [binary, "update", resource, *flags, "--if-revision", predecessor["revision"], "--actor", actor, "--json"]
                and receipt["binarySha256"] == binary_hash and receipt["exitCode"] == 0 and receipt["signal"] is None,
                "authoring installed command provenance/guard/exit differs")
        for stream in ("stdout", "stderr"):
            require(digest(root / (prefix + "." + stream + ".log")) == receipt[stream + "Sha256"], "authoring output hash differs")
        require(mutation == proof[key] and mutation["schemaVersion"] == 1 and mutation["preview"] is True
                and mutation["result"]["changed"] is changed and mutation["result"]["issue"]["id"] == resource
                and mutation["result"]["issue"]["revision"] == record["revision"]
                and mutation["result"]["issue"]["properties"] == record["properties"], "authoring CLI/BDP mismatch")
    require(proof["beforeETag"] and proof["editedETag"] and proof["beforeETag"] != proof["editedETag"], "authoring edit ETag unchanged")
    for label, index_key, status, etag, record in (
        ("before", "beforeIndex", 200, proof["beforeETag"], before),
        ("edited", "editedIndex", 200, proof["editedETag"], edited),
        ("noop", "noopIndex", 304, proof["editedETag"], None),
    ):
        index = proof[index_key]
        require(type(index) is int and 0 <= index < len(network), "authoring HTTP index missing")
        observation = network[index]
        body = root / ("client-issue-authoring-" + label + ".body")
        require(observation["url"] == resource and observation["method"] == "GET" and observation["status"] == status
                and observation["headers"].get("etag") == etag and observation["bodyBytes"] == body.stat().st_size
                and observation["bodySha256"] == digest(body), "authoring HTTP status/ETag/body receipt differs")
        require(body.read_bytes() == b"" if record is None else json.loads(body.read_bytes()) == record, "authoring raw body differs")
    require(proof["beforeIndex"] < proof["editedIndex"] < proof["noopIndex"], "authoring HTTP observations unordered")


def verify_memory_deletion_capture(root, summary):
    """Check the new proof without overwriting any pre-deletion evidence."""
    def data(relative):
        return json.loads((root / relative).read_text())
    alpha = data("02-memory-create/stdout.log")["result"]
    scope = summary["scope"]
    require(alpha["id"] == scope + "beads/alpha", "wrong deletion fixture identity")
    deletion = data("16-memory-delete/stdout.log")
    require(deletion.get("preview") is True and deletion.get("schemaVersion") == 1
            and deletion.get("result") == {"memory": alpha, "preview": False, "deleted": True},
            "deletion did not retain/disclose exact final live state")
    argv = data("16-memory-delete/receipt.json")["argv"]
    require(argv == [data("02-memory-create/receipt.json")["argv"][0], "delete", "beads/alpha", "--force",
                     "--if-revision", alpha["revision"], "--json"], "deletion did not use original observed guard")
    require(summary.get("memory_delete") == {"passed": True, "id": alpha["id"], "final_live_revision": alpha["revision"]},
            "Memory deletion summary missing")
    require(summary.get("python_after_delete") == {"passed": True, "beads": 3, "limit": 1,
            "authenticated": True, "mechanism": "unchanged standard-library example over BDP HTTP"}
            and summary.get("python_after_delete_pages") == 3, "post-delete Python summary missing")
    observations = data("python-after-delete-network.json")
    require(len(observations) == 4 and observations[0]["url"] == scope + "bdp.json"
            and observations[0]["document"]["scope"] == scope
            and observations[0]["document"]["profile"] == "read", "post-delete discovery missing")
    next_url, records = scope + "beads/?limit=1", []
    for observation in observations:
        require(observation["method"] == "GET" and observation["status"] == 200
                and observation["url"].startswith(scope) and observation["body_bytes"] > 0
                and re.fullmatch(r"[0-9a-f]{64}", observation["body_sha256"]), "invalid post-delete HTTP observation")
    for observation in observations[1:]:
        page = observation["document"]
        require(observation["url"] == next_url and len(page["items"]) == 1,
                "post-delete Python did not follow actual one-record next page")
        records.extend(page["items"])
        next_url = page["next"]
    require(next_url is None and len(records) == 3 and {item["id"] for item in records} ==
            {scope + "beads/" + name for name in ["plan", "work", "prereq"]}
            and data("python-after-delete/stdout.log") == records,
            "post-delete Python enumeration retained deleted state or omitted survivors")


def output_tail(stdout, stderr, limit=TAIL_LINES):
    """Last lines of a failed command's test output and of its stderr.

    go test -json carries the test text in each event's Output field; any
    other stdout line is kept as it is.
    """
    text = []
    for line in stdout.decode(errors="replace").splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            text.append(line)
            continue
        text.extend(item.get("Output", "").splitlines() if isinstance(item, dict) and "Action" in item else [line])
    return text[-limit:], stderr.decode(errors="replace").splitlines()[-limit:]


def report_failure_tail(label, stdout, stderr):
    output, errors = output_tail(stdout, stderr)
    for title, lines in (("test output", output), ("stderr", errors)):
        if lines:
            print(f"--- {label}: last {len(lines)} lines of {title} ---\n" + "\n".join(lines), flush=True)


def phase_table(receipts):
    """One row per receipt; go test groups also show their share of the timeout."""
    rows = ["### Graph core C0 phases", "", "| Phase | Roots | Seconds | % of go test timeout |", "|---|---:|---:|---:|"]
    for item in receipts:
        seconds, roots = item.get("elapsedSeconds"), item.get("roots")
        rows.append("| {} | {} | {} | {} |".format(
            item["label"], "" if roots is None else roots, "" if seconds is None else f"{seconds:.1f}",
            "" if roots is None or seconds is None else f"{seconds / GO_TEST_TIMEOUT:.0%}"))
    return "\n".join(rows) + "\n"


def slow_group_warnings(receipts):
    """Advisory annotations for go test groups that used 60% or more of their timeout."""
    return [f"::warning title=Slow go test group::{item['label']} took {item['elapsedSeconds']:.0f} s, "
            f"{item['elapsedSeconds'] / GO_TEST_TIMEOUT:.0%} of its {GO_TEST_TIMEOUT} s go test timeout"
            for item in receipts
            if item.get("roots") is not None and item.get("elapsedSeconds", 0) >= SLOW_GROUP_SECONDS]


def publish_phase_report(receipts, summary_path):
    for warning in slow_group_warnings(receipts):
        print(warning, flush=True)
    if summary_path:
        try:
            with open(summary_path, "a") as summary:
                summary.write(phase_table(receipts))
        except OSError as error:
            print(f"::warning title=Phase table not written::{error}", flush=True)


# The aggregator. It takes the expected legs from the constants above, never from the workflow, and believes no receipt:
# it parses every leg's retained go test output again, so a proof counts only if the evidence itself shows it.
SERVER_SUFFIX = "-server-shutdown"
LEG_PRIVATE = ("host.json", "leg.json", "receipts.json")  # per leg and not republished as they are
REBUILT = tuple(label + suffix for label in GROUPED for suffix in (".stdout", ".stderr", "-groups.json"))  # built from the groups
SHARED = ("source.json", "home", *(f"{label}.{ext}" for label in ("head", "source-status", "dolt-version") for ext in ("stdout", "stderr")),
          *(f"{label}-discovery.{ext}" for label in GROUPED for ext in ("stdout", "stderr", "json")))  # the same in every leg that has them
SHARED_RECEIPTS = ("head", "source-status", "dolt-version", *(label + "-discovery" for label in GROUPED))
MERGE_ORDER = (REST_LEG, *STORAGE_LEGS)  # the leg whose copy of a shared file is republished comes first


def read_bytes(path):
    require(path.is_file(), f"missing evidence file: {path}")
    return path.read_bytes()


def read_json(path):
    return json.loads(read_bytes(path))


def go_events(path):
    return [json.loads(line) for line in read_bytes(path).splitlines()]


def passed_tests(events):
    return {e["Test"] for e in events if e.get("Action") == "pass" and e.get("Test")}


def check_discovery(evidence, label):
    """A label's discovery record, held to its own digest and to the compiled listing kept beside it."""
    found = read_json(evidence / (label + "-discovery.json"))
    roots = found["roots"]
    require(roots and roots == sorted(set(roots)) and found["sha256"] == roots_digest(roots),
            f"{label}: discovery digest does not match its roots")
    listing = (evidence / (label + "-discovery.stdout")).read_text()
    require(sorted(re.findall(r"^Test\w+$", listing, re.MULTILINE)) == roots,
            f"{label}: retained compiled listing differs from the discovery record")
    return found


def check_single(label, evidence, record):
    found = check_discovery(evidence, label)
    events = go_events(evidence / (label + ".stdout"))
    count = verify_tests(events, set(found["roots"]))
    require(record["passed"].get(label) == count and record["discovery"].get(label) == found["sha256"],
            f"{label}: leg receipt differs from its evidence")
    require(set(REQUIRED.get(label, ())) <= passed_tests(events), f"{label}: required engine proof absent")
    return found


def check_grouped(label, evidence_by_leg, records):
    """A grouped label across the legs that run it: each group once, each root once, every required tuple present."""
    discovery, plans, ran = None, {}, {}
    for leg, evidence in evidence_by_leg.items():
        found = check_discovery(evidence, label)
        require(discovery in (None, found), f"{label}: discovery differs between legs")
        discovery = found
        require(records[leg]["discovery"].get(label) == found["sha256"], f"{leg}: receipt discovery digest differs from its evidence")
        plans[leg] = read_json(evidence / (label + "-groups.json"))
        require(len(plans[leg]["groups"]) == GROUPED[label], f"{leg}: {label} plan has the wrong number of groups")
        ran[leg] = {number for number, group in enumerate(plans[leg]["groups"], 1) if group["passed"] is True}
    every = [number for numbers in ran.values() for number in numbers]
    require(len(every) == len(set(every)), f"{label}: duplicate group across legs: {sorted(n for n in set(every) if every.count(n) > 1)}")
    require(sorted(every) == list(range(1, GROUPED[label] + 1)), f"{label}: groups run across the legs are {sorted(every)}, not 1..{GROUPED[label]}")
    selections = partition_required_tests(discovery["roots"], GROUPED[label])
    completed, runs = set(), collections.Counter()
    for leg, evidence in evidence_by_leg.items():
        plan = plans[leg]
        require(ran[leg] == set(LEG_GROUPS[leg][label]), f"{leg}: ran {label} groups {sorted(ran[leg])}, not its own {list(LEG_GROUPS[leg][label])}")
        require(plan["complete"] is (len(ran[leg]) == GROUPED[label]) and plan["package"] == discovery["package"]
                and plan["selector"] == discovery["selector"] and plan["sourceRoots"] == plan["compiledRoots"] == discovery["roots"],
                f"{leg}: {label} plan differs from its discovery")
        for number, (group, names) in enumerate(zip(plans[leg]["groups"], selections), 1):
            part = evidence / f"{label}-group-{number}.stdout"
            require(group["label"] == f"{label}-group-{number}" and group["roots"] == names
                    and group["selector"] == "^(" + "|".join(names) + ")$", f"{leg}: {label} group {number} differs from the partition")
            require(part.exists() == (number in ran[leg]), f"{leg}: {label} group {number} output does not match its plan")
            if number in ran[leg]:
                require(group["stdoutSHA256"] == digest(part), f"{leg}: {label} group {number} output differs from its plan")
                events = go_events(part)
                verify_tests(events, set(names))
                completed |= passed_tests(events)
                runs.update(e["Test"] for e in events if e.get("Action") == "run" and "/" not in e.get("Test", ""))
    require(set(runs) == set(discovery["roots"]) and set(runs.values()) == {1}, f"{label}: a root did not run exactly once across the legs")
    require(set(REQUIRED.get(label, ())) <= completed, f"{label}: required engine proof absent")
    return discovery, {number: leg for leg, numbers in ran.items() for number in numbers}


def check_leg(leg, evidence, commit):
    """The leg's completion receipt, which only a leg that finished writes, held to the evidence beside it."""
    require((evidence / "leg.json").is_file(), f"{leg}: no completion receipt (leg.json); the leg did not finish")
    record, source = read_json(evidence / "leg.json"), read_json(evidence / "source.json")
    require(record.get("leg") == leg and record.get("complete") is True, f"{leg}: completion receipt is for another leg or incomplete")
    require(record.get("commit") == commit, f"{leg}: receipt commit {record.get('commit')} differs from {commit}")
    require(source.get("commit") == commit and (evidence / "head.stdout").read_text().strip() == commit,
            f"{leg}: evidence commit differs from {commit}")
    require(record.get("labels") == list(LEG_LABELS[leg])
            and record.get("groups") == {label: list(numbers) for label, numbers in LEG_GROUPS[leg].items()},
            f"{leg}: receipt labels or groups differ from the leg set")
    receipts = read_json(evidence / "receipts.json")
    require(all(r.get("exit") == 0 and "failure" not in r for r in receipts), f"{leg}: a command did not exit cleanly")
    shutdowns = [r for r in receipts if r["label"].endswith(SERVER_SUFFIX)]
    servers = sorted(r["label"][:-len(SERVER_SUFFIX)] for r in shutdowns)
    require(servers == sorted(expected_servers(leg)), f"{leg}: servers {servers} are not one fresh server per group")
    require(all(r["forced"] is False and r["groupGone"] is True and r["portClosed"] is True for r in shutdowns),
            f"{leg}: unclean server shutdown")
    return record, source, receipts, servers


def aggregate(legs_dir, commit, output):
    """Check the legs as one lane, then republish their evidence in the layout the single lane had.

    The legs are in legs_dir, one directory per leg artifact, named as the artifact is. The combined artifact is
    written to output: graph-c0-evidence/, graph-c0-build/ and bdp-client-build/ beside each other.
    """
    check_leg_constants()
    legs_dir, output = Path(legs_dir).resolve(), Path(output).resolve()
    names = {leg: LEG_ARTIFACT.format(leg=leg, commit=commit) for leg in LEGS}
    present = sorted(path.name for path in legs_dir.iterdir()) if legs_dir.is_dir() else []
    require(present == sorted(names.values()), f"leg artifacts present {present}, expected {sorted(names.values())}")
    evidence = {leg: legs_dir / names[leg] / EVIDENCE for leg in LEGS}
    records, sources, receipts, servers = {}, {}, {}, []
    for leg in LEGS:
        records[leg], sources[leg], receipts[leg], found = check_leg(leg, evidence[leg], commit)
        servers += found
    require(len(servers) == len(set(servers)) and len(servers) == sum(len(expected_servers(leg)) for leg in LEGS),
            "a server was shared or missing across the legs")
    identity = {leg: {key: sources[leg][key] for key in ("binarySHA256", "doltSHA256", "runnerSHA256")} for leg in LEGS}
    require(all(found == identity[REST_LEG] for found in identity.values()), "the legs ran different binaries or runner scripts")
    require(identity[REST_LEG]["runnerSHA256"] == digest(Path(__file__)), "the legs ran a different runner script than the aggregator")
    builds = {leg: [read_bytes(legs_dir / names[leg] / "graph-c0-build" / name) for name in ("build-manifest.txt", "SHA256SUMS")] for leg in LEGS}
    manifest = dict(line.split("=", 1) for line in builds[REST_LEG][0].decode().splitlines())
    require(all(found == builds[REST_LEG] for found in builds.values()) and manifest.get("commit") == commit
            and builds[REST_LEG][1].decode().split()[0] == identity[REST_LEG]["binarySHA256"], "the legs ran different build artifacts")
    read_bytes(legs_dir / names[REST_LEG] / "bdp-client-build" / "client-build.json")
    require(records[REST_LEG].get("facts") == REST_FACTS, f"{REST_LEG}: receipt lacks the installed CLI, engine, HTTP and Python facts")
    passed, owners = {}, {}
    for label in PASSED_LABELS:
        holders = {leg: evidence[leg] for leg in LEGS if label in LEG_LABELS[leg]}
        if label in GROUPED:
            found, owners[label] = check_grouped(label, holders, records)
            passed[label] = len(found["roots"])
            if label != "storage":
                require(records[REST_LEG]["passed"].get(label) == passed[label], f"{label}: leg receipt differs from its evidence")
        else:
            (leg, directory), = holders.items()
            passed[label] = len(check_single(label, directory, records[leg])["roots"])
    merge(legs_dir, names, evidence, receipts, owners, output, commit, passed)
    return passed


def merge_grouped(label, evidence, owners, combined):
    """The label's plan and combined output, built from its groups in group order, whichever leg ran each one."""
    plans = {leg: read_json(directory / (label + "-groups.json")) for leg, directory in evidence.items()}
    plan = json.loads(json.dumps(next(iter(plans.values()))))
    for number, leg in owners.items():
        plan["groups"][number - 1] = plans[leg]["groups"][number - 1]
    plan["complete"] = True
    for suffix in ("stdout", "stderr"):
        data = b"".join((evidence[owners[number]] / f"{label}-group-{number}.{suffix}").read_bytes() for number in sorted(owners))
        (combined / f"{label}.{suffix}").write_bytes(data)
        plan[suffix + "SHA256"] = hashlib.sha256(data).hexdigest()
    (combined / (label + "-groups.json")).write_text(json.dumps(plan, indent=2) + "\n")


def merge(legs_dir, names, evidence, receipts, owners, output, commit, passed):
    """Write the combined artifact: every leg's evidence under the file names the single lane used."""
    taken, merged, seen = {}, [], {}
    for leg in MERGE_ORDER:  # a file or receipt two legs both hold is refused here, before anything is written
        for entry in sorted(evidence[leg].iterdir()):
            if entry.name in LEG_PRIVATE or entry.name in REBUILT:
                continue
            if entry.name in taken:
                require(entry.name in SHARED, f"{entry.name} is in both {taken[entry.name][0]} and {leg}")
                continue
            taken[entry.name] = (leg, entry)
        for receipt in receipts[leg]:
            if receipt["label"] in seen:
                require(receipt["label"] in SHARED_RECEIPTS, f"receipt {receipt['label']} is in both {seen[receipt['label']]} and {leg}")
                continue
            seen[receipt["label"]] = leg
            merged.append(receipt)
    combined = output / EVIDENCE
    combined.mkdir(parents=True, exist_ok=False)
    for _, entry in taken.values():
        if entry.is_dir():
            shutil.copytree(entry, combined / entry.name, symlinks=True)
        else:
            shutil.copy2(entry, combined / entry.name)
    for label in GROUPED:
        merge_grouped(label, {leg: evidence[leg] for leg in LEGS if label in LEG_LABELS[leg]}, owners[label], combined)
    (combined / "receipts.json").write_text(json.dumps(merged, indent=2) + "\n")
    (combined / "summary.json").write_text(json.dumps(dict(commit=commit, passed=passed, **REST_FACTS), indent=2) + "\n")
    (combined / "legs.json").write_text(json.dumps(dict(commit=commit, servers=sum(len(expected_servers(leg)) for leg in LEGS), legs={
        leg: dict(artifact=names[leg], labels=list(LEG_LABELS[leg]), servers=expected_servers(leg),
                  groups={label: list(numbers) for label, numbers in LEG_GROUPS[leg].items()}) for leg in LEGS}), indent=2) + "\n")
    build = output / "graph-c0-build"
    build.mkdir()
    for name in ("build-manifest.txt", "SHA256SUMS"):
        shutil.copy2(legs_dir / names[REST_LEG] / "graph-c0-build" / name, build / name)
    shutil.copytree(legs_dir / names[REST_LEG] / "bdp-client-build", output / "bdp-client-build", symlinks=True)


class Qualification:
    def __init__(self, args):
        self.root = Path(__file__).resolve().parents[2]
        self.output = Path(args.output).resolve()
        self.output.mkdir(parents=True, exist_ok=False)
        self.bd = Path(args.bd).resolve()
        self.dolt = Path(args.dolt).resolve()
        self.artifacts = Path(args.artifacts).resolve()
        self.children = set()
        self.receipts = []
        self.counts = {}
        # Only the rest leg builds and runs the public BDP client; a storage leg has no Node or client checkout.
        self.client_checkout, self.client_manifest, self.node = (
            None if getattr(args, name) is None else Path(getattr(args, name)).resolve()
            for name in ("bdp_checkout", "client_manifest", "node"))
        self.env = {k: os.environ[k] for k in (
            "PATH", "GOCACHE", "GOMODCACHE", "GOROOT", "DEVELOPER_DIR", "TMPDIR") if k in os.environ}
        home = self.output / "home"
        home.mkdir()
        for key in ("GOCACHE", "GOMODCACHE"):
            if key not in self.env:
                self.env[key] = subprocess.check_output(["go", "env", key], cwd=self.root, text=True).strip()
        self.env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"),
                        GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=str(home / "empty-gitconfig"),
                        CGO_ENABLED="1", PYTHONDONTWRITEBYTECODE="1", BD_DISABLE_METRICS="1", BD_DISABLE_EVENT_FLUSH="1",
                        DOLT_METRICS_DISABLED="1", DOLT_DISABLE_EVENT_FLUSH="1",
                        BEADS_DOLT_AUTO_START="0", NO_COLOR="1",
                        BEADS_TEST_BD_BINARY=str(self.bd), BEADS_TEST_IGNORE_REPO_CONFIG="1")

    def run(self, args, label, cwd=None, timeout=120, stdin=None, expected=0, owned_groups=None, roots=None):
        print(f"C0 {label}: {args}", flush=True)
        started = time.monotonic()

        def timing():
            return dict(elapsedSeconds=round(time.monotonic() - started, 3), **({} if roots is None else dict(roots=roots)))

        process = subprocess.Popen([str(a) for a in args], cwd=cwd or self.root,
                                   env=self.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
        self.children.add(process.pid)
        try:
            out, err = process.communicate(stdin, timeout=timeout)
        except BaseException as original:
            # The smoke process owns independent groups. Give its signal
            # handler a bounded chance to drain them before killing the owner.
            try:
                process.send_signal(signal.SIGTERM)
            except ProcessLookupError:
                pass
            forced_owner = False
            cleanup_errors = []
            out, err = b"", b""
            nested = None
            try:
                out, err = process.communicate(timeout=OWNER_CLEANUP_GRACE)
            except BaseException as drain_error:
                forced_owner = True
                if not isinstance(drain_error, subprocess.TimeoutExpired):
                    cleanup_errors.append(f"owner drain: {drain_error!r}")
                # Keep the owner alive while its children stop, so it can reap
                # them. A registry failure must never leave the owner alive.
                try:
                    cleanup_registered_groups(owned_groups)
                except BaseException as cleanup_error:
                    cleanup_errors.append(f"nested before owner kill: {cleanup_error!r}")
                finally:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                try:
                    out, err = process.communicate(timeout=10)
                except BaseException as reap_error:
                    cleanup_errors.append(f"owner reap: {reap_error!r}")
                    if isinstance(reap_error, subprocess.TimeoutExpired):
                        out, err = reap_error.output or b"", reap_error.stderr or b""
            if not group_exited(process.pid):
                forced_owner = True
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                if not group_exited(process.pid):
                    cleanup_errors.append("owner process group survived forced cleanup")
            # Retry after the owner has retired, even if the first registry
            # pass failed. Preserve the triggering failure and every cleanup
            # failure in the receipt instead of replacing the original error.
            try:
                nested = cleanup_registered_groups(owned_groups)
            except BaseException as cleanup_error:
                cleanup_errors.append(f"nested after owner retirement: {cleanup_error!r}")
            (self.output / f"{label}.stdout").write_bytes(out)
            (self.output / f"{label}.stderr").write_bytes(err)
            self.receipts.append(dict(label=label, argv=[str(a) for a in args], pid=process.pid,
                                      exit=process.returncode, failure=type(original).__name__,
                                      forcedOwner=forced_owner, nestedCleanup=nested,
                                      cleanupErrors=cleanup_errors,
                                      stdoutSHA256=hashlib.sha256(out).hexdigest(), **timing()))
            report_failure_tail(label, out, err)
            raise
        finally:
            self.children.discard(process.pid)
        (self.output / f"{label}.stdout").write_bytes(out)
        (self.output / f"{label}.stderr").write_bytes(err)
        if not group_exited(process.pid):
            os.killpg(process.pid, signal.SIGKILL)
            raise RuntimeError(f"{label} left a live process group after exit")
        self.receipts.append(dict(label=label, argv=[str(a) for a in args], pid=process.pid,
                                  exit=process.returncode, stdoutSHA256=hashlib.sha256(out).hexdigest(), **timing()))
        if owned_groups is not None:
            nested = cleanup_registered_groups(owned_groups)
            require(nested["registeredGroups"] == 0, "completed owner left nested groups registered")
        if process.returncode != expected:
            report_failure_tail(label, out, err)
        require(process.returncode == expected,
                f"{label} exited {process.returncode}, expected {expected}; see saved stdout/stderr")
        return out

    @contextlib.contextmanager
    def server(self, label):
        """One owned Dolt server for one command: fresh data directory and port, own log and shutdown receipt.

        The port is exported as BEADS_GRAPH_TEST_SERVER_PORT while the server is up and only then. Every
        server must stop cleanly (exit 0, not forced, group gone, port closed) or the step fails.
        """
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        server_root = self.output / (label + "-server-data")
        server_root.mkdir()
        log = (self.output / (label + "-server.log")).open("wb")
        started = time.monotonic()
        server = subprocess.Popen([str(self.dolt), "sql-server", "--host", "127.0.0.1", "--port", str(port), "--data-dir", str(server_root)],
                                  cwd=server_root, env=self.env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        self.children.add(server.pid)
        try:
            deadline = time.monotonic() + SERVER_READY_TIMEOUT
            while True:
                require(server.poll() is None, "owned server exited before readiness")
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=1):
                        break
                except OSError:
                    require(time.monotonic() < deadline, "server readiness timed out")
                    time.sleep(0.2)
            self.env["BEADS_GRAPH_TEST_SERVER_PORT"] = str(port)
            yield port
            require(server.poll() is None, "server exited during qualification")
        finally:
            self.env.pop("BEADS_GRAPH_TEST_SERVER_PORT", None)
            server.terminate()
            forced = False
            try:
                code = server.wait(timeout=SERVER_STOP_TIMEOUT)
            except subprocess.TimeoutExpired:
                forced = True
                os.killpg(server.pid, signal.SIGKILL)
                code = server.wait()
            finally:
                self.children.discard(server.pid)
                log.close()
            group_gone = group_exited(server.pid)
            if not group_gone:
                os.killpg(server.pid, signal.SIGKILL)
            port_closed = False
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=1):
                    pass
            except ConnectionRefusedError:
                port_closed = True
            self.receipts.append(dict(label=label + "-server-shutdown", pid=server.pid, port=port,
                                      exit=code, forced=forced, groupGone=group_gone,
                                      portClosed=port_closed, elapsedSeconds=round(time.monotonic() - started, 3)))
            require(code == 0 and not forced and group_gone and port_closed,
                    f"server cleanup failed: exit={code}, forced={forced}, groupGone={group_gone}, portClosed={port_closed}")

    def tests(self, package, selector, files, label, required_subtests=(), groups=1, run_groups=None):
        """Discover a package's test roots, then run them under fresh servers: one for the package, or one per group.

        run_groups names the (1-based) groups of a grouped label this call runs, for a leg that holds only some of
        them; None runs them all. Such a leg proves only the roots of its own groups, and the aggregator proves the
        rest across the legs, so only a run of every group sets self.counts[label] and the plan's "complete".
        """
        # Source discovery also sees accidentally build-excluded added tests.
        source_names = []
        for path in files:
            names = re.findall(r"^func (Test\w+)\(t \*testing.T\)", path.read_text(), re.MULTILINE)
            source_names.extend(name for name in names if re.search(selector, name))
        expected = set(source_names)
        require(len(source_names) == len(expected), f"{label}: duplicate source test roots")
        listing = self.run(["go", "test", "-tags", "gms_pure_go", "-list", selector, package],
                           label + "-discovery", timeout=600).decode()
        compiled_names = re.findall(r"^Test\w+$", listing, re.MULTILINE)
        listed = set(compiled_names)
        require(len(compiled_names) == len(listed), f"{label}: duplicate compiled test roots")
        require(listed == expected and expected, f"{label}: source and compiled test discovery disagree")
        (self.output / (label + "-discovery.json")).write_text(json.dumps(
            dict(package=package, selector=selector, roots=sorted(expected), sha256=roots_digest(expected)), indent=2) + "\n")
        require(groups > 1 or run_groups is None, f"{label}: run_groups needs a grouped run")
        flags = ["go", "test", "-tags", "gms_pure_go", "-json", "-count=1", "-p=1",
                 "-parallel=1", "-timeout=15m", "-run"]
        proved = expected
        if groups == 1:
            with self.server(label):
                out = self.run([*flags, selector, package], label, timeout=960, roots=len(expected))
        else:
            ran = tuple(range(1, groups + 1)) if run_groups is None else tuple(run_groups)
            require(ran and list(ran) == sorted(set(ran)) and set(ran) <= set(range(1, groups + 1)),
                    f"{label}: groups to run must be distinct numbers in 1..{groups}")
            selections = partition_required_tests(source_names, groups)
            plan = dict(package=package, selector=selector, sourceRoots=sorted(expected),
                        compiledRoots=sorted(listed), complete=False, groups=[
                            dict(label=f"{label}-group-{index}", roots=names,
                                 selector="^(" + "|".join(names) + ")$", passed=False)
                            for index, names in enumerate(selections, 1)])
            aggregate = self.output / (label + ".stdout")
            aggregate.write_bytes(b"")
            aggregate_error = self.output / (label + ".stderr")
            aggregate_error.write_bytes(b"")
            try:
                for index, group in enumerate(plan["groups"], 1):
                    if index not in ran:
                        continue
                    group_label, names, selected = group["label"], group["roots"], group["selector"]
                    (self.output / (label + "-groups.json")).write_text(json.dumps(plan, indent=2) + "\n")
                    try:
                        with self.server(group_label):
                            result = self.run([*flags, selected, package], group_label, timeout=960, roots=len(names))
                        verify_tests([json.loads(line) for line in result.splitlines()], set(names))
                        group["passed"] = True
                    finally:
                        # Keep actual partial output even if the group fails or times out.
                        for suffix, combined in (("stdout", aggregate), ("stderr", aggregate_error)):
                            part = self.output / (group_label + "." + suffix)
                            if part.exists():
                                with combined.open("ab") as target:
                                    target.write(part.read_bytes())
                                group[suffix + "SHA256"] = digest(part)
                executed = [name for index, group in enumerate(plan["groups"], 1) if index in ran for name in group["roots"]]
                proved = set(executed)
                require(len(executed) == len(proved) and (proved == expected if len(ran) == groups else proved <= expected),
                        f"{label}: grouped test union differs from complete discovery")
                out = aggregate.read_bytes()
                verify_tests([json.loads(line) for line in out.splitlines()], proved)
                plan["complete"] = len(ran) == groups
            finally:
                plan["stdoutSHA256"] = digest(aggregate)
                plan["stderrSHA256"] = digest(aggregate_error)
                (self.output / (label + "-groups.json")).write_text(json.dumps(plan, indent=2) + "\n")
        events = [json.loads(line) for line in out.splitlines()]
        count = verify_tests(events, proved)
        completed = {e.get("Test") for e in events if e.get("Action") == "pass"}
        required = set(required_subtests)
        if proved == expected:
            self.counts[label] = count
        else:
            # Only the tuples whose root is in this leg's groups can be here; the aggregator checks the rest.
            required = {name for name in required if name.split("/", 1)[0] in proved}
        require(required <= completed, f"{label}: required engine proof absent")

    def cli(self, work, label, *args):
        return json.loads(self.run([self.bd, *args, "--json"], label, cwd=work))

    def smoke_command(self, port):
        argv = [sys.executable, str(self.root / "scripts/graph-bdp-read-smoke.py"),
                "--bd", str(self.bd), "--server-port", str(port),
                "--bdp-checkout", str(self.client_checkout), "--client-manifest", str(self.client_manifest),
                "--node", str(self.node), "--output-dir", str(self.output / "http"),
                "--owned-groups", str(self.output / "http-owned-groups.jsonl"),
                "--command-timeout", str(SMOKE_COMMAND_TIMEOUT), "--total-timeout", str(SMOKE_TOTAL_TIMEOUT)]
        return argv, SMOKE_WRAPPER_TIMEOUT

    def capture(self, engine, port):
        work = self.output / (engine + "-workspace")
        work.mkdir()
        scope = f"https://example.invalid/ci-c0/{engine}/"
        self.run(["git", "init", "-q", str(work)], engine + "-git-init")
        self.run(["git", "-C", str(work), "config", "core.hooksPath", ".git/hooks"], engine + "-git-hooks")
        flags = ["--server", "--external", "--server-host", "127.0.0.1", "--server-port", str(port), "--server-user", "root"] if engine == "server" else []
        initialized = self.cli(work, engine + "-init", "init", "--graph-mode", "link", "--scope-url", scope,
                               "--skip-hooks", "--skip-agents", "--non-interactive", *flags)
        require(initialized["result"]["backend"] == engine and initialized["result"]["scope"] == scope,
                "initialization selected the wrong backend or Scope")
        created = self.cli(work, engine + "-create", "remember", "Durable C0 body — 記憶", "--id", "beads/plan", "--title", "Plan")
        record = created["result"]
        require(record["id"] == scope + "beads/plan" and record["type"] == scope + "types/preview-memory-v2"
                and record["properties"] == {"title": "Plan", "body": "Durable C0 body — 記憶"}
                and record["owned"] == [] and record["revision"] and record["version"], "incomplete/non-Memory creation")
        # Each call starts and reaps a distinct installed CLI process.
        for suffix, identity in [("show", "beads/plan"), ("reopen", record["id"])]:
            require(self.cli(work, engine + "-" + suffix, "show", identity) == created,
                    "fresh-process read differs from committed creation")
        status = self.cli(work, engine + "-status", "status", "--graph")
        require(status["result"]["capabilities"]["memoryRead"] is True, "Memory read not advertised")

    def prelude(self):
        """What every leg checks before its tests: the installed CLI, a clean tree at the build's commit, the pinned Dolt."""
        require(self.bd.is_file() and os.access(self.bd, os.X_OK), "missing installed CLI")
        require(self.dolt.is_file() and os.access(self.dolt, os.X_OK), "missing released Dolt")
        head = self.run(["git", "rev-parse", "HEAD"], "head").decode().strip()
        require(not self.run(["git", "status", "--porcelain", "--untracked-files=all"], "source-status"),
                "qualification requires clean source, including no untracked files")
        manifest = dict(line.split("=", 1) for line in (self.artifacts / "build-manifest.txt").read_text().splitlines())
        require(manifest["commit"] == head and manifest["build_tags"] == "gms_pure_go", "CLI artifact source/build flags mismatch")
        expected_hash = (self.artifacts / "SHA256SUMS").read_text().split()[0]
        require(digest(self.bd) == expected_hash, "installed CLI hash differs from build artifact")
        version = self.run([self.dolt, "version"], "dolt-version").decode().splitlines()
        require(version and version[0].strip() == "dolt version 2.1.8", "C0 requires released Dolt2.1.8")
        (self.output / "source.json").write_text(json.dumps(dict(commit=head, binarySHA256=digest(self.bd),
            doltSHA256=digest(self.dolt), runnerSHA256=digest(Path(__file__))), indent=2) + "\n")
        return head

    def execute(self, leg):
        """Run one leg. leg.json, its completion receipt, is written last: a leg that did not finish has none."""
        head = self.prelude()
        (self.output / "host.json").write_text(json.dumps(host_info(), indent=2) + "\n")
        if leg == REST_LEG:
            self.execute_rest()
        else:
            self.execute_storage(STORAGE_LEGS[leg])
        require(not self.children, "owned child process remains")
        discovery = {label: json.loads((self.output / (label + "-discovery.json")).read_text())["sha256"] for label in LEG_LABELS[leg]}
        (self.output / "leg.json").write_text(json.dumps(dict(
            leg=leg, commit=head, complete=True, labels=list(LEG_LABELS[leg]),
            groups={label: list(numbers) for label, numbers in LEG_GROUPS[leg].items()},
            discovery=discovery, passed=self.counts, **({"facts": REST_FACTS} if leg == REST_LEG else {})), indent=2) + "\n")

    def execute_storage(self, groups):
        """A storage leg: whole groups of the graphstore tests, each under its own fresh server."""
        self.tests("./internal/storage/graphstore", "^Test", (self.root / "internal/storage/graphstore").glob("*_test.go"), "storage",
                   STORAGE_REQUIRED, groups=STORAGE_GROUPS, run_groups=groups)

    def execute_rest(self):
        """The rest leg: every other package, the installed CLI groups, the HTTP/BDP capture and smoke and the Python example."""
        with contextlib.ExitStack() as servers:
            # Sequential package runs/provisioning; concurrency inside same-store tests remains exercised.
            self.tests("./internal/graphpatch", "^Test", (self.root / "internal/graphpatch").glob("*_test.go"), "graphpatch")
            self.tests("./internal/configfile", "^TestGraphMode", (self.root / "internal/configfile").glob("graph_mode_test.go"), "config")
            self.tests("./internal/storage/issueops", "^TestResolve(CustomConfigStrict|InfraTypesStrict|ConfigLegacy)",
                       (self.root / "internal/storage/issueops").glob("config_strict_test.go"), "query-config")
            self.tests("./internal/storage/issueops", "^TestPrepareIssueForInsertNormalizesOptionalTimestampsToUTC$",
                       (self.root / "internal/storage/issueops").glob("prepare_timestamps_utc_test.go"), "utc-issueops")
            self.tests("./internal/storage/domain/db", "^TestNormalizeIssueTimestampsConvertsOptionalTimestampsToUTC$",
                       (self.root / "internal/storage/domain/db").glob("normalize_timestamps_utc_test.go"), "utc-db")
            self.tests("./internal/types", "^TestNormalizeOptionalTimestampsToUTCCoversEveryPointerTimestamp$",
                       (self.root / "internal/types").glob("types_test.go"), "utc-types")
            self.tests("./internal/templates/agents", "^TestGraphPreview",
                       (self.root / "internal/templates/agents").glob("*_test.go"), "agent-template")
            self.tests("./cmd/bd/setup", "^TestGraphPreview", (self.root / "cmd/bd/setup").glob("graph_claude_test.go"), "graph-claude-setup")
            self.tests("./cmd/bd", "^Test(GraphModeCLI|GraphPreview)", (self.root / "cmd/bd").glob("graph*test.go"), "cli",
                       CLI_REQUIRED, groups=CLI_GROUPS)
            self.env["BDP_SPEC_AT_PIN"] = str(self.client_checkout / "docs/specs/bdp.md")
            self.tests("./internal/httpapi/bdpwire", "^Test", (self.root / "internal/httpapi/bdpwire").glob("*_test.go"), "bdpwire")
            self.tests("./internal/httpapi/graphread", "^Test", (self.root / "internal/httpapi/graphread").glob("*_test.go"), "graphread",
                       GRAPHREAD_REQUIRED)
            self.tests("./internal/httpapi", "^TestGraphRead", (self.root / "internal/httpapi").glob("graph_read*test.go"), "graph-http",
                       GRAPH_HTTP_REQUIRED)
            self.run([sys.executable, "-m", "unittest", "discover", "-s", "examples/bdp-read", "-p", "test_*.py"], "python-example-tests")
            python_roots = sum(len(re.findall(r"^    def test_\w+\(", path.read_text(), re.MULTILINE))
                               for path in (self.root / "examples/bdp-read").glob("test_*.py"))
            python_result = (self.output / "python-example-tests.stderr").read_text()
            require(python_roots > 0 and re.search(rf"Ran {python_roots} tests? in", python_result)
                    and "skipped=" not in python_result, "Python example tests missing or skipped")
            port = servers.enter_context(self.server("capture"))
            self.capture("embedded", port)
            self.capture("server", port)
            # Per command < smoke total < this wrapper (SMOKE_*): the wrapper outlasts the smoke,
            # so a command that outruns its deadline surfaces as the smoke's labelled failure.
            argv, timeout = self.smoke_command(port)
            self.run(argv, "bdp-http-capture", timeout=timeout,
                     owned_groups=self.output / "http-owned-groups.jsonl")
            verify_http_capture(self.output / "http", digest(self.bd))
            http_summary = json.loads((self.output / "http/summary.json").read_text())
            for field, relative in [("harness_sha256", "scripts/graph-bdp-read-smoke.py"),
                                    ("client_harness_sha256", "scripts/graph-bdp-read-client.mjs"),
                                    ("python_example_sha256", "examples/bdp-read/read_beads.py")]:
                require(http_summary[field] == digest(self.root / relative), "HTTP capture source changed")


# The options each mode takes besides --leg and --output: exactly these, so a storage leg is never given a BDP client.
OPTIONS = {"storage": ("bd", "dolt", "artifacts"),
           REST_LEG: ("bd", "dolt", "artifacts", "bdp-checkout", "client-manifest", "node"),
           AGGREGATE: ("commit", "legs-dir")}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--leg", required=True, choices=(*LEGS, AGGREGATE))
    parser.add_argument("--output", required=True)
    for name in sorted({name for names in OPTIONS.values() for name in names}):
        parser.add_argument("--" + name)
    args = parser.parse_args()
    mode = args.leg if args.leg in (REST_LEG, AGGREGATE) else "storage"
    given = {name for names in OPTIONS.values() for name in names if getattr(args, name.replace("-", "_")) is not None}
    if given != set(OPTIONS[mode]):
        parser.error(f"--leg {args.leg} takes exactly --" + ", --".join(OPTIONS[mode]) + " besides --leg and --output")
    check_leg_constants()
    if args.leg == AGGREGATE:
        passed = aggregate(args.legs_dir, args.commit, args.output)
        print(f"C0 aggregate: {len(LEGS)} legs, {sum(passed.values())} test roots, combined evidence in {args.output}", flush=True)
        return

    def interrupted(signum, _frame):
        raise RuntimeError(f"qualification interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    qualification = Qualification(args)
    try:
        qualification.execute(args.leg)
    finally:
        (qualification.output / "receipts.json").write_text(json.dumps(qualification.receipts, indent=2) + "\n")
        publish_phase_report(qualification.receipts, os.environ.get("GITHUB_STEP_SUMMARY"))


if __name__ == "__main__":
    main()
