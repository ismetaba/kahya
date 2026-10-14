"""Fake TAINTED worker for TestWorkerSessionAttributionResistsGroupJoin.

kahyad/internal/server/controlauth_session_test.go starts this script
through the REAL kahyad/internal/spawn.Run (exactly like a production
worker) next to a second, CLEAN fake worker, and passes the clean
worker's pid (== its process-group id == its session id) as argv[1]. The
script plays a code-level compromised worker that tries to borrow the
clean worker's identity WITHOUT naming the clean task's ids being
trusted:

  * leader_check: the worker itself names the clean task's ids on
    /policy/check (must stay pinned to its own, tainted task);
  * joined_child: a forked child calls setpgid(0, <clean pgid>) and then
    /policy/check (the process-group join must not move it to the clean
    task);
  * escaped_child: a forked child calls setpgid(0, 0) - leaving its own
    process group without setsid - and then, holding the control secret
    it read from <socket>.secret, POSTs /policy/promote (must still be
    refused as a live worker) and /policy/check (must stay pinned);
  * joined_leader: the worker itself calls setpgid(0, <clean pgid>) and
    then /policy/check.

Each probe is reported as one {"type":"delta","text":"<json>"} stdout
line (docs/ipc.md section 4); the script ends with a terminal
{"type":"result","status":"ok"} line. Stdlib only.
"""

import errno
import http.client
import json
import os
import socket
import sys

SOCK = os.environ["KAHYA_SOCKET"]
CLEAN_PGID = int(sys.argv[1])

LAUNDER_CHECK = {
    "trace_id": "trace-clean",
    "task_id": "t-clean",
    "tool_name": "fs_write",
    "tool_input": {"path": "x", "content_base64": "eA=="},
}
PROMOTE = {"tool": "fs_write", "class": "W1", "scope": "global"}


class _UDSConnection(http.client.HTTPConnection):
    def __init__(self):
        super().__init__("kahyad", timeout=10)

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(10)
        s.connect(SOCK)
        self.sock = s


def post(path, body, headers=None):
    conn = _UDSConnection()
    try:
        h = {"Content-Type": "application/json"}
        h.update(headers or {})
        conn.request("POST", path, body=json.dumps(body), headers=h)
        resp = conn.getresponse()
        raw = resp.read()
        try:
            data = json.loads(raw)
        except ValueError:
            data = {}
        return resp.status, data
    finally:
        conn.close()


def emit(obj):
    sys.stdout.write(json.dumps({"type": "delta", "text": json.dumps(obj)}) + "\n")
    sys.stdout.flush()


def try_setpgid(pgid):
    try:
        os.setpgid(0, pgid)
        return "ok"
    except OSError as e:
        return errno.errorcode.get(e.errno, str(e.errno))


def check_probe(name, setpgid_result):
    status, data = post("/policy/check", LAUNDER_CHECK)
    emit({
        "probe": name,
        "setpgid": setpgid_result,
        "pgid": os.getpgid(0),
        "sid": os.getsid(0),
        "status": status,
        "decision": data.get("decision", ""),
        "rule": data.get("rule", ""),
    })


def in_child(fn):
    pid = os.fork()
    if pid == 0:
        try:
            fn()
        except Exception as e:  # report, never hang the parent
            emit({"probe": "child_error", "error": repr(e)})
        finally:
            sys.stdout.flush()
            os._exit(0)
    os.waitpid(pid, 0)


def joined_child():
    check_probe("joined_child", try_setpgid(CLEAN_PGID))


def escaped_child():
    res = try_setpgid(0)
    with open(SOCK + ".secret") as f:
        secret = f.read().strip()
    status, data = post("/policy/promote", PROMOTE, {"X-Kahya-Control-Secret": secret})
    emit({
        "probe": "escaped_child_promote",
        "setpgid": res,
        "pgid": os.getpgid(0),
        "sid": os.getsid(0),
        "status": status,
        "level": data.get("level", 0),
    })
    check_probe("escaped_child_check", res)


def main():
    sys.stdin.read()  # spawn.Run writes the envelope AFTER OnStart registered us
    check_probe("leader_check", "")
    in_child(joined_child)
    in_child(escaped_child)
    check_probe("joined_leader", try_setpgid(CLEAN_PGID))
    sys.stdout.write(json.dumps({"type": "result", "status": "ok"}) + "\n")
    sys.stdout.flush()


if __name__ == "__main__":
    main()
