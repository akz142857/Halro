import datetime as dt
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from local_receiver import Store


class ReceiverFixtureTest(unittest.TestCase):
    def test_ttl_recovery_and_replay_survive_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory) / "receiver.sqlite")
            store = Store(path, "ha")
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
            first = 1_000_000.0

            def heartbeat(sequence, observed, attempt=1):
                return {
                    "schema_version": "halro.deadman.event/v1",
                    "event_id": f"event-{sequence}",
                    "sequence": sequence,
                    "probe_id": "probe-1",
                    "environment": "test",
                    "region": "local",
                    "cluster": "ha",
                    "kind": "heartbeat",
                    "observed_at": dt.datetime.fromtimestamp(observed, dt.timezone.utc).isoformat(),
                    "heartbeat_ttl": "2s",
                    "attempt": attempt,
                }

            with patch("local_receiver.now_seconds", return_value=first):
                self.assertEqual(store.accept(heartbeat(1, first)), "accepted")
                self.assertEqual(store.accept(heartbeat(1, first, attempt=2)), "duplicate")
                stale = heartbeat(1, first)
                stale["event_id"] = "new-id-old-sequence"
                self.assertEqual(store.accept(stale), "replay")
                self.assertEqual(store.snapshot()["notifications"], [])

            with patch("local_receiver.now_seconds", return_value=first + 3):
                store.expire()
                self.assertEqual(store.snapshot()["notifications"][0]["reason"], "heartbeat_ttl_expired")
                self.assertEqual(store.accept(heartbeat(2, first + 3)), "accepted")
                self.assertEqual(
                    [(n["state"], n["reason"]) for n in store.snapshot()["notifications"]],
                    [("firing", "heartbeat_ttl_expired"), ("resolved", "heartbeat_restored")],
                )
                original_deadline = store.snapshot()["probes"][0]["deadline"]
                self.assertEqual(store.accept(heartbeat(3, first + 1)), "accepted")
                self.assertEqual(store.snapshot()["probes"][0]["deadline"], original_deadline)

            store.db.close()
            store = Store(path, "ha")
            with patch("local_receiver.now_seconds", return_value=first + 3):
                self.assertEqual(store.accept(heartbeat(2, first + 3, attempt=3)), "duplicate")
                self.assertEqual(len(store.snapshot()["notifications"]), 2)
                self.assertEqual(store.snapshot()["probes"][0]["sequence"], 3)
            store.db.close()


if __name__ == "__main__":
    unittest.main()
