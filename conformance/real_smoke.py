#!/usr/bin/env python3
"""Smoke PluginBridge v4 against a REAL OpenCode v1 server.

Run only against a disposable server/data directory and project: creates sessions,
switches their native models, and consumes tokens from the configured provider.
Credentials come from OPENCODE_SERVER_PASSWORD and are never written to logs.
"""

import argparse
import base64
import json
import os
import pathlib
import queue
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--server-url", required=True)
parser.add_argument(
    "--cwd",
    required=True,
    help="Disposable project directory for the smoke sessions",
)
parser.add_argument(
    "--model",
    required=True,
    help="Configured providerID/modelID; uses real model inference",
)
parser.add_argument(
    "--adapter",
    default=str(
        pathlib.Path(__file__).resolve().parents[1] / "bin/prism-plugin-opencode"
    ),
)
parser.add_argument(
    "--output-dir",
    required=True,
    help="Directory for smoke logs (contains prompts and model output)",
)
args = parser.parse_args()
ROOT = pathlib.Path(args.output_dir).resolve()
ROOT.mkdir(parents=True, exist_ok=True)
PROJECT = pathlib.Path(args.cwd).resolve()
PROJECT.mkdir(parents=True, exist_ok=True)
URL = args.server_url.rstrip("/")
password = os.environ.get("OPENCODE_SERVER_PASSWORD", "")
AUTH = "Basic " + base64.b64encode(("opencode:" + password).encode()).decode()


def http(path, data=None, method=None):
    req = urllib.request.Request(
        URL + path,
        data=None if data is None else json.dumps(data).encode(),
        method=method,
        headers={"Authorization": AUTH, "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=20) as r:
        b = r.read()
        return json.loads(b) if b else None


print("health", http("/global/health"), flush=True)
if password:
    try:
        urllib.request.urlopen(URL + "/global/health", timeout=10)
        raise AssertionError("auth not enforced")
    except urllib.error.HTTPError as e:
        assert e.code == 401
    print("PASS auth rejects anonymous request", flush=True)
env = dict(
    os.environ, PRISM_PLUGIN_PROTOCOL_VERSION="4", OPENCODE_SERVER_PASSWORD=password
)
p = subprocess.Popen(
    [args.adapter, "--server-url", URL],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=(ROOT / "adapter.log").open("w"),
    text=True,
    env=env,
)
q = queue.Queue()
frames = []


def reader():
    for line in p.stdout:
        frame = json.loads(line)
        frames.append(frame)
        q.put(frame)
        with (ROOT / "plugin-frames.jsonl").open("a") as f:
            f.write(json.dumps(frame) + "\n")


threading.Thread(target=reader, daemon=True).start()
seq = 0


def rpc(method, params=None, timeout=90):
    global seq
    seq += 1
    ident = str(seq)
    p.stdin.write(
        json.dumps({"id": ident, "method": "adapter." + method, "params": params})
        + "\n"
    )
    p.stdin.flush()
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            f = q.get(timeout=deadline - time.monotonic())
        except queue.Empty:
            raise TimeoutError(method)
        if f.get("id") == ident:
            assert f.get("ok"), (method, f)
            print("PASS", method, flush=True)
            return f.get("payload")
    raise TimeoutError(method)


def history(session, stream):
    rpc(
        "readHistoryStream",
        {"session": session, "request": {"stream_id": stream, "limit": 10}},
    )
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        matched = [
            f["payload"]
            for f in frames
            if f.get("event") == "history.stream"
            and f["payload"].get("stream_id") == stream
        ]
        if any(f.get("type") == "end" for f in matched):
            return matched
        time.sleep(0.05)
    raise TimeoutError("history")


try:
    assert rpc("probe")["available"]
    assert rpc("discover")["verified"]
    native = http(
        "/session?" + urllib.parse.urlencode({"directory": str(PROJECT)}),
        {"title": "Prism real CLI smoke"},
    )
    sid = native["id"]
    session = rpc("attachSession", {"native_session_id": sid, "cwd": str(PROJECT)})
    assert any(x["native_session_id"] == sid for x in rpc("listSessions"))
    selected = rpc(
        "controlSession", {"session": session, "action": "conversation.select"}
    )
    assert (
        selected["ok"]
        and selected["thread_id"] == sid
        and selected["details"]["selected"]
    )
    detail = rpc("readDetail", {"session": session})
    assert detail["model_options"]
    assert rpc(
        "controlSession",
        {"session": session, "action": "model.switch", "target": args.model},
    )["ok"]

    assert rpc("subscribe", {"subscription_id": "real-smoke", "session": session})[
        "subscribed"
    ]
    assert any(x["type"] == "end" for x in history(session, "empty-history"))
    marker = "PRISM_SMOKE_OK_" + str(time.time_ns())
    rpc(
        "readHistoryStream",
        {
            "session": session,
            "request": {"stream_id": "live-smoke", "limit": 10, "live": True},
        },
    )
    time.sleep(0.3)
    text = "Reply with exactly " + marker + ". Do not use any tools."
    assert rpc(
        "send",
        {
            "session": session,
            "message": {
                "prism_message_id": "smoke-1",
                "text": text,
                "source_device": "smoke",
            },
        },
    )["accepted"]
    terminal = rpc("waitForRun", {"session": session, "run_id": "smoke-1"}, timeout=120)
    print("terminal:", json.dumps(terminal, ensure_ascii=False), flush=True)
    assert terminal["type"] == "run.completed"
    assert rpc("verifyVisibility", {"session": session, "marker": text})["visible"]
    h = history(session, "final-history")
    messages = [m for e in h if e.get("turn") for m in e["turn"]["messages"]]
    assert any(
        m["role"] == "assistant" and marker in m.get("content", "") for m in messages
    ), messages
    pe = [f["payload"]["event"] for f in frames if f.get("event") == "plugin.event"]
    assert any(
        e["type"] == "message.part.updated"
        and marker in e.get("payload", {}).get("text", "")
        and e["payload"].get("message_id")
        in {m["id"] for m in messages if m["role"] == "assistant"}
        for e in pe
    ), pe
    assert any(e["type"] == "run.completed" for e in pe), pe
    live = [
        f["payload"]
        for f in frames
        if f.get("event") == "history.stream"
        and f["payload"].get("stream_id") == "live-smoke"
        and f["payload"].get("source") == "live"
    ]
    assert any(
        m["role"] == "assistant" and marker in m.get("content", "")
        for e in live
        if e.get("turn")
        for m in e["turn"]["messages"]
    ), live
    rpc("cancelHistoryStream", {"stream_id": "live-smoke"})
    print(
        "PASS real model response, associated SSE text, live and snapshot history match",
        flush=True,
    )
    # Wait for a real assistant to start before exercising plugin.interrupt.
    frame_start = len(frames)
    before = len(http("/session/" + sid + "/message"))
    assert rpc(
        "send",
        {
            "session": session,
            "message": {
                "prism_message_id": "smoke-abort",
                "text": "Output the integers from 1 through 10000, one integer per line. Do not use tools.",
            },
        },
    )["accepted"]
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        native_messages = http("/session/" + sid + "/message")
        if (
            len(native_messages) >= before + 2
            and native_messages[-1]["info"]["role"] == "assistant"
        ):
            assert not native_messages[-1]["info"]["time"].get("completed"), (
                "Run already ended before interrupt"
            )
            break
        time.sleep(0.05)
    else:
        raise TimeoutError("active assistant before interrupt")
    assert rpc("interrupt", {"session": session})["ok"]
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        terminal_events = [
            f["payload"]["event"]
            for f in frames[frame_start:]
            if f.get("event") == "plugin.event"
            and f["payload"]["event"]["type"].startswith("run.")
        ]
        if terminal_events:
            assert all(
                e["type"] == "run.failed" and e["payload"].get("reason") == "aborted"
                for e in terminal_events
            ), terminal_events
            break
        time.sleep(0.05)
    else:
        raise TimeoutError("interrupted run terminal event")
    native_error = http("/session/" + sid + "/message")[-1]["info"].get("error", {})
    assert native_error.get("name") == "MessageAbortedError", native_error
    print(
        "PASS active interrupt: native MessageAbortedError -> plugin run.failed/aborted",
        flush=True,
    )
    opened = rpc("openDraft", {"draft_id":"smoke-draft", "cwd":str(PROJECT)})
    assert opened["draft_id"] == "smoke-draft" and opened["controls"]["model_options"]
    rpc("controlDraft", {"draft_id":"smoke-draft", "action":"model.switch", "target":{"option_id":args.model}})
    started = rpc(
        "startDraftWithMessage",
        {
            "draft_id": "smoke-draft",
            "cwd": str(PROJECT),
            "message": {
                "prism_message_id": "smoke-2",
                "text": "Reply exactly PRISM_START_OK. Do not use tools.",
            },
        },
        timeout=90,
    )
    assert started["receipt"]["accepted"] and started["visibility"]["visible"]
    assert started["session"]["cwd"] == str(PROJECT)
    native_model = http("/session/" + started["session"]["native_session_id"])["model"]
    assert native_model["providerID"] + "/" + native_model["id"] == args.model
    started_terminal = rpc(
        "waitForRun", {"session": started["session"], "run_id": "smoke-2"}, timeout=120
    )
    assert (
        started_terminal["type"] == "run.completed"
        and "PRISM_START_OK" in started_terminal["summary"]
    ), started_terminal
    rpc("unsubscribe", {"subscription_id": "real-smoke"})
    rpc("close")
    print("REAL SMOKE PASS", flush=True)
finally:
    p.stdin.close()
    try:
        p.wait(timeout=5)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait()
