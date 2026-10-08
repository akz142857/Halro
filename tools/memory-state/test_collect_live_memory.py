import importlib.util
import json
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("live_capture", pathlib.Path(__file__).with_name("collect_live_memory.py"))
capture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capture)


class MemoryEvidenceTests(unittest.TestCase):
    def test_cgroup_v2_subtree_and_escaped_mount(self):
        version, path = capture.cgroup_location("0::/tenant/docker/abc\n", "36 25 0:32 /tenant /sys/cgroup\\040memory rw - cgroup2 cgroup rw\n")
        self.assertEqual((version, str(path)), (2, "/sys/cgroup memory/docker/abc"))

    def test_v1_memory_and_hybrid_prefers_unified_membership(self):
        mounts = ("36 25 0:32 / /sys/v1 rw - cgroup cgroup rw,memory\n"
                  "37 25 0:33 / /sys/v2 rw - cgroup2 cgroup rw\n")
        self.assertEqual(capture.cgroup_location("7:cpu:/cpu\n8:memory:/docker/a\n", mounts), (1, pathlib.Path("/sys/v1/docker/a")))
        self.assertEqual(capture.cgroup_location("8:memory:/old\n0::/new\n", mounts), (2, pathlib.Path("/sys/v2/new")))

    def test_unmatched_mount_and_traversal_refused(self):
        for membership in ["0::/elsewhere\n", "0::/tenant/../outside\n"]:
            with self.assertRaises(ValueError):
                capture.cgroup_location(membership, "36 25 0:32 /tenant /sys/v2 rw - cgroup2 cgroup rw\n")

    def test_stat_comm_parentheses_do_not_shift_starttime(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "41").mkdir()
            suffix = ["S"] + ["0"] * 18 + ["12345"] + ["0"] * 5
            (root / "41/stat").write_text("41 (halro (worker)) " + " ".join(suffix))
            self.assertEqual(capture.process_token(41, root), 12345)

    def test_multiple_swarm_tasks_are_not_collapsed(self):
        with mock.patch.object(capture, "run", return_value=("a" * 64 + "\n" + "b" * 64 + "\n").encode()):
            with self.assertRaises(ValueError):
                capture.select_container("service", None)

    def test_command_overflow_and_timeout_join_child(self):
        with self.assertRaises(ValueError):
            capture.run([sys.executable, "-c", "import sys;sys.stdout.write('x'*8192)"], maximum=10)
        with self.assertRaises(TimeoutError):
            capture.run([sys.executable, "-c", "import time;time.sleep(10)"], timeout=0.1)
        self.assertEqual(capture.run([sys.executable, "-c", "print('ok')"]), b"ok\n")

    def test_selected_docker_fields_exclude_credentials_and_application_logs(self):
        for forbidden in [".Config.Env", ".Config.Cmd", ".Config.Entrypoint", ".Mounts", ".State.Health", ".Log"]:
            self.assertNotIn(forbidden, capture.INSPECT)

    def fixture(self, directory, change_identity=False, profile_failure=False):
        state = {"id": "a" * 64, "image_id": "sha256:" + "b" * 64, "configured_image": "halro@test",
                 "pid": 41, "running": True, "paused": False, "oom_killed": False, "started_at": "start",
                 "memory_limit": 2 << 30, "memory_swap": 2 << 30, "task_id": "task", "service": "service"}
        states = [state, dict(state, pid=42)] if change_identity else [state] * 5
        def source(path):
            path = str(path)
            if path.endswith("/cgroup"):
                return "0::/docker/abc\n"
            if path.endswith("/comm"):
                return "halro\n"
            if path.endswith("mountinfo"):
                return "36 25 0:32 / /sys/v2 rw - cgroup2 cgroup rw\n"
            if path.endswith("memory.events"):
                return "oom 0\noom_kill 0\n"
            return "fixture-source\n"
        def command(argv, **kwargs):
            if profile_failure:
                raise RuntimeError("no existing diagnostic listener")
            return b"{}"
        args = ["--service", "service", "--samples", "1", "--output", str(pathlib.Path(directory) / "evidence")]
        if profile_failure:
            args += ["--pprof-port", "6060"]
        patches = [mock.patch.object(capture.sys, "platform", "linux"),
                   mock.patch.object(capture, "select_container", return_value="a" * 64),
                   mock.patch.object(capture, "inspect", side_effect=states),
                   mock.patch.object(capture, "read_file", side_effect=source),
                   mock.patch.object(capture, "process_token", return_value=12345),
                   mock.patch.object(capture, "hash_executable", return_value=([1, 2, 3, 4, 5], "c" * 64)),
                   mock.patch.object(capture, "executable_identity", return_value=[1, 2, 3, 4, 5]),
                   mock.patch.object(capture, "run", side_effect=command)]
        for patch in patches:
            patch.start()
        try:
            code = capture.main(args)
        finally:
            for patch in reversed(patches):
                patch.stop()
        output = pathlib.Path(directory) / "evidence"
        report = json.loads((output / "report.json").read_text())
        return code, report, output

    def test_process_replacement_keeps_incomplete_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            code, report, output = self.fixture(directory, change_identity=True)
            self.assertEqual(code, 1)
            self.assertFalse(report["complete"])
            self.assertEqual(report["samples"], [])
            self.assertTrue((output / "sha256.json").exists())

    def test_proc_capture_is_private_and_not_full_production_acceptance(self):
        with tempfile.TemporaryDirectory() as directory:
            code, report, output = self.fixture(directory)
            self.assertEqual(code, 0)
            self.assertTrue(report["complete"])
            self.assertFalse(report["profile_complete"])
            self.assertFalse(report["production_acceptance"])
            self.assertEqual(output.stat().st_mode & 0o777, 0o700)
            self.assertEqual((output / "report.json").stat().st_mode & 0o777, 0o600)

    def test_missing_diagnostic_listener_is_not_successful_profile(self):
        with tempfile.TemporaryDirectory() as directory:
            code, report, output = self.fixture(directory, profile_failure=True)
            self.assertEqual(code, 0)
            self.assertTrue(report["complete"])
            self.assertFalse(report["profile_complete"])
            self.assertFalse(any(item["captured"] for item in report["profiles"].values()))
            self.assertFalse((output / "heap.pb.gz").exists())

class ProfileIdentityTests(unittest.TestCase):
    def test_listener_belongs_to_target_pid_not_merely_namespace(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "41/net").mkdir(parents=True)
            (root / "41/fd").mkdir()
            (root / "41/net/tcp").write_text("header\n 0: 0100007F:17AC 00000000:0000 0A 0:0 0:0 0 0 0 1234\n")
            with self.assertRaises(ValueError):
                capture.owns_profile_listener(41, 6060, root)
            (root / "41/fd/9").symlink_to("socket:[1234]")
            capture.owns_profile_listener(41, 6060, root)
            with self.assertRaises(ValueError):
                capture.owns_profile_listener(41, 6061, root)

    def test_runtime_shape_and_complete_gzip(self):
        import gzip
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "profile"
            for body in ("{}", "[]"):
                path.write_text(body)
                with self.assertRaises(ValueError):
                    capture.validate_profile(path, runtime=True)
            path.write_text(json.dumps({"memory": {"HeapAlloc": 1, "HeapSys": 2, "Sys": 3, "NumGC": 4}, "goroutines": 1}))
            capture.validate_profile(path, runtime=True)
            encoded = gzip.compress(b"nonempty protobuf bytes still require pprof analysis")
            path.write_bytes(encoded)
            capture.validate_profile(path)
            path.write_bytes(encoded[:-3])
            with self.assertRaises((EOFError, OSError)):
                capture.validate_profile(path)


if __name__ == "__main__":
    unittest.main()
