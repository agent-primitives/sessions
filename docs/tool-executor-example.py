#!/usr/bin/env python3
"""Local transactional publish demo, not an external-service exactly-once adapter.
Usage: python3 tool-executor-example.py SOCKET DATABASE (inside a private directory).
"""
import json
import os
import socket
import sqlite3
import sys

LIMIT = 1 << 20


def unique_members(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON member")
        result[key] = value
    return result


def execute(db, request):
    fields = {"version", "harness", "session_uid", "call_id", "tool",
              "arguments", "idempotency_key"}
    if (set(request) != fields or type(request["version"]) is not int
            or request["version"] != 1 or request["tool"] != "publish"
            or any(not isinstance(request[k], str) or not request[k]
                   for k in ("harness", "session_uid", "call_id", "idempotency_key"))):
        raise ValueError("unsupported request")
    arguments = request["arguments"]
    if arguments is not None and not isinstance(arguments, dict):
        raise ValueError("arguments must be an object or null")
    # This demo's entire effect is a local SQLite insert: effect and original receipt
    # share one transaction. A remote HTTP call cannot be made atomic this way.
    fingerprint = json.dumps([request["tool"], arguments], sort_keys=True, allow_nan=False)
    identity = (request["session_uid"], request["idempotency_key"])
    with db:
        db.execute("BEGIN IMMEDIATE")
        row = db.execute("SELECT fingerprint, output FROM receipts WHERE uid=? AND key=?",
                         identity).fetchone()
        if row:
            if row[0] != fingerprint:
                raise ValueError("key reused for a different operation")
            return json.loads(row[1])
        output = {"text": "published"}
        db.execute("INSERT INTO publications(uid, key, arguments) VALUES(?, ?, ?)",
                   (*identity, json.dumps(arguments, allow_nan=False)))
        db.execute("INSERT INTO receipts VALUES(?, ?, ?, ?)",
                   (*identity, fingerprint, json.dumps(output)))
    return output


def main():
    os.umask(0o077)
    socket_path, database = sys.argv[1:]
    # Never unlink somebody else's socket; the operator handles stale paths after a crash.
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(socket_path)
        server.listen()
        db = sqlite3.connect(database)
        db.executescript("""
            CREATE TABLE IF NOT EXISTS publications(uid TEXT, key TEXT, arguments TEXT);
            CREATE TABLE IF NOT EXISTS receipts(
                uid TEXT, key TEXT, fingerprint TEXT, output TEXT, PRIMARY KEY(uid, key));
        """)
        print("listening", flush=True)
        while True:
            conn, _ = server.accept()
            with conn:
                conn.settimeout(30)
                request = {}
                try:
                    with conn.makefile("rb") as stream:
                        line = stream.readline(LIMIT + 1)
                    if len(line) > LIMIT or not line.endswith(b"\n"):
                        raise ValueError("invalid frame")
                    request = json.loads(line.decode("utf-8"), object_pairs_hook=unique_members)
                    if not isinstance(request, dict):
                        raise ValueError("request must be an object")
                    reply = {"version": 1, "call_id": request.get("call_id", ""),
                             "is_error": False, "output": execute(db, request)}
                except (ValueError, OSError, sqlite3.Error) as err:
                    if isinstance(err, sqlite3.Error):
                        print("executor storage failure:", type(err).__name__,
                              getattr(err, "sqlite_errorname", "unknown"),
                              file=sys.stderr, flush=True)
                    reply = {"version": 1,
                             "call_id": request.get("call_id", "") if isinstance(request, dict) else "",
                             "is_error": True, "error": "request refused"}
                try:
                    conn.sendall((json.dumps(reply, allow_nan=False) + "\n").encode("utf-8"))
                except OSError:
                    pass  # The effect/receipt may already be committed; recovery reuses the key.


if __name__ == "__main__":
    main()
