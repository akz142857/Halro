#!/usr/bin/env python3
"""Local-only deadman receiver fixture. Not a production notification service."""

import argparse
import datetime as dt
import http.server
import json
import os
import re
import sqlite3
import ssl
import threading
import time
from pathlib import Path


def now_seconds():
    return time.time()


def event_time(value):
    if not isinstance(value, str):
        raise ValueError("observed_at must be a timestamp")
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("observed_at needs a time zone")
    return parsed.timestamp()


def ttl_seconds(value):
    if not isinstance(value, str):
        raise ValueError("heartbeat_ttl must be a duration")
    match = re.fullmatch(r"([1-9][0-9]*)(s|m)", value)
    if not match:
        raise ValueError("heartbeat_ttl must use seconds or minutes")
    return int(match.group(1)) * (60 if match.group(2) == "m" else 1)


def required_text(event, key):
    value = event.get(key)
    if not isinstance(value, str) or not value:
        raise ValueError(f"{key} must be nonempty text")
    return value


class Store:
    def __init__(self, path, expected_cluster):
        self.lock = threading.Lock()
        self.cluster = expected_cluster
        self.db = sqlite3.connect(path, check_same_thread=False, isolation_level="DEFERRED")
        os.chmod(path, 0o600)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA synchronous=FULL")
        self.db.executescript("""
            CREATE TABLE IF NOT EXISTS events (
                event_id TEXT PRIMARY KEY, probe_key TEXT NOT NULL,
                sequence INTEGER NOT NULL, body TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS probes (
                probe_key TEXT PRIMARY KEY, sequence INTEGER NOT NULL,
                deadline REAL, firing INTEGER NOT NULL DEFAULT 0
            );
            CREATE TABLE IF NOT EXISTS notifications (
                id INTEGER PRIMARY KEY AUTOINCREMENT, probe_key TEXT NOT NULL,
                state TEXT NOT NULL, reason TEXT NOT NULL, event_id TEXT,
                created_at REAL NOT NULL
            );
        """)

    def _expire(self, at):
        for key, in self.db.execute(
            "SELECT probe_key FROM probes WHERE deadline IS NOT NULL AND deadline <= ? AND firing = 0", (at,)
        ).fetchall():
            self.db.execute("UPDATE probes SET firing = 1 WHERE probe_key = ?", (key,))
            self.db.execute(
                "INSERT INTO notifications(probe_key,state,reason,created_at) VALUES(?,?,?,?)",
                (key, "firing", "heartbeat_ttl_expired", at),
            )

    def expire(self):
        with self.lock, self.db:
            self._expire(now_seconds())

    def accept(self, event):
        if not isinstance(event, dict) or event.get("schema_version") != "halro.deadman.event/v1":
            raise ValueError("invalid event schema")
        event_id = required_text(event, "event_id")
        identity = [required_text(event, name) for name in ("probe_id", "environment", "region", "cluster")]
        if identity[3] != self.cluster:
            raise ValueError("wrong cluster")
        sequence = event.get("sequence")
        if not isinstance(sequence, int) or isinstance(sequence, bool) or sequence < 1:
            raise ValueError("invalid sequence")
        attempt = event.get("attempt")
        if not isinstance(attempt, int) or isinstance(attempt, bool) or attempt < 1:
            raise ValueError("invalid attempt")
        kind = event.get("kind")
        if kind not in ("heartbeat", "state_transition"):
            raise ValueError("invalid kind")
        observed = event_time(event.get("observed_at"))
        ttl = ttl_seconds(event.get("heartbeat_ttl"))
        if observed > now_seconds() + 5:
            raise ValueError("future observation")
        if kind == "state_transition":
            if required_text(event, "state") not in ("up", "down"):
                raise ValueError("invalid transition")
            required_text(event, "target_id")
            if required_text(event, "target_kind") not in ("halro", "prometheus", "alertmanager"):
                raise ValueError("invalid target kind")
        elif any(name in event for name in ("state", "target_id", "target_kind")):
            raise ValueError("heartbeat has target fields")
        key = json.dumps(identity, separators=(",", ":"))
        canonical = dict(event)
        canonical.pop("attempt", None)
        body = json.dumps(canonical, sort_keys=True, separators=(",", ":"))
        at = now_seconds()
        with self.lock, self.db:
            self._expire(at)
            existing = self.db.execute("SELECT body FROM events WHERE event_id = ?", (event_id,)).fetchone()
            if existing:
                return "duplicate" if existing[0] == body else "replay"
            row = self.db.execute(
                "SELECT sequence,deadline,firing FROM probes WHERE probe_key = ?", (key,)
            ).fetchone()
            if row and sequence <= row[0]:
                return "replay"
            self.db.execute(
                "INSERT INTO events(event_id,probe_key,sequence,body) VALUES(?,?,?,?)",
                (event_id, key, sequence, body),
            )
            deadline, firing = (row[1], row[2]) if row else (None, 0)
            if kind == "heartbeat":
                candidate = observed + ttl
                if deadline is None or candidate > deadline:
                    deadline = candidate
                    if firing and candidate > at:
                        self.db.execute(
                            "INSERT INTO notifications(probe_key,state,reason,event_id,created_at) VALUES(?,?,?,?,?)",
                            (key, "resolved", "heartbeat_restored", event_id, at),
                        )
                        firing = 0
            else:
                self.db.execute(
                    "INSERT INTO notifications(probe_key,state,reason,event_id,created_at) VALUES(?,?,?,?,?)",
                    (key, "firing" if event["state"] == "down" else "resolved", "target_" + event["state"], event_id, at),
                )
            self.db.execute(
                "INSERT INTO probes(probe_key,sequence,deadline,firing) VALUES(?,?,?,?) "
                "ON CONFLICT(probe_key) DO UPDATE SET sequence=excluded.sequence,deadline=excluded.deadline,firing=excluded.firing",
                (key, sequence, deadline, firing),
            )
        return "accepted"

    def snapshot(self):
        with self.lock:
            probes = self.db.execute("SELECT probe_key,sequence,deadline,firing FROM probes ORDER BY probe_key").fetchall()
            notifications = self.db.execute(
                "SELECT state,reason,event_id,created_at FROM notifications ORDER BY id"
            ).fetchall()
            return {"probes": [{"key": key, "sequence": seq, "deadline": deadline, "firing": bool(firing)}
                               for key, seq, deadline, firing in probes],
                    "notifications": [{"state": state, "reason": reason, "event_id": event_id, "created_at": at}
                                      for state, reason, event_id, at in notifications]}


def handler_for(store, token):
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

        def authorized(self):
            return self.headers.get("Authorization") == "Bearer " + token

        def respond(self, status, value=None):
            body = json.dumps(value).encode() if value is not None else b""
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path == "/":
                self.respond(200, {"role": "primary", "cluster_id": store.cluster})
            elif self.path == "/-/ready":
                self.respond(200, {"ready": True})
            elif self.path.startswith("/api/v1/query?"):
                self.respond(200, {"status": "success", "data": {"resultType": "scalar", "result": [now_seconds(), "0"]}})
            elif self.path == "/v1/state" and self.authorized():
                self.respond(200, store.snapshot())
            else:
                self.respond(404)

        def do_POST(self):
            if self.path != "/v1/events" or not self.authorized():
                self.respond(401)
                return
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 65536:
                    raise ValueError("invalid body size")
                event = json.loads(self.rfile.read(size))
                result = store.accept(event)
            except (ValueError, json.JSONDecodeError):
                self.respond(400)
                return
            self.respond(204 if result in ("accepted", "duplicate") else 409)

    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--listen", default="127.0.0.1:19420")
    parser.add_argument("--db", required=True)
    parser.add_argument("--cluster", required=True)
    parser.add_argument("--tls-cert", required=True)
    parser.add_argument("--tls-key", required=True)
    parser.add_argument("--client-ca", required=True)
    parser.add_argument("--token-file", required=True)
    args = parser.parse_args()
    host, port = args.listen.rsplit(":", 1)
    os.umask(0o077)
    store = Store(args.db, args.cluster)
    server = http.server.ThreadingHTTPServer((host, int(port)), handler_for(store, Path(args.token_file).read_text().strip()))
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.tls_cert, args.tls_key)
    context.load_verify_locations(args.client_ca)
    context.verify_mode = ssl.CERT_REQUIRED
    server.socket = context.wrap_socket(server.socket, server_side=True)

    stop = threading.Event()

    def tick():
        while not stop.is_set():
            store.expire()
            stop.wait(0.25)

    worker = threading.Thread(target=tick, daemon=True)
    worker.start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        stop.set()
        worker.join()
        server.server_close()
        store.db.close()


if __name__ == "__main__":
    main()
