#!/usr/bin/env python3
"""Read-only memory evidence on the Linux Docker host; never updates a service.

Writes only a new private evidence directory. Docker fields deliberately exclude
Env, command lines, mounts and health/application logs. Profiles are optional,
through the target network namespace's loopback only, without forced GC.
"""
import argparse
import datetime
import hashlib
import gzip
import json
import os
import pathlib
import posixpath
import re
import selectors
import subprocess
import sys
import time

MAX_FILE = 1 << 20
INSPECT = ('{"id":{{json .Id}},"image_id":{{json .Image}},'
           '"configured_image":{{json .Config.Image}},"pid":{{.State.Pid}},'
           '"running":{{.State.Running}},"paused":{{.State.Paused}},'
           '"oom_killed":{{.State.OOMKilled}},"started_at":{{json .State.StartedAt}},'
           '"memory_limit":{{.HostConfig.Memory}},"memory_swap":{{.HostConfig.MemorySwap}},'
           '"task_id":{{json (index .Config.Labels "com.docker.swarm.task.id")}},'
           '"service":{{json (index .Config.Labels "com.docker.swarm.service.name")}}}')


def run(argv, timeout=20, output=None, maximum=MAX_FILE):
    """Bound both streams; kill/join this child on overflow or timeout."""
    p = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    sel = selectors.DefaultSelector()
    captured = {"out": bytearray(), "err": bytearray()}
    used = {"out": 0, "err": 0}
    for pipe, label in ((p.stdout, "out"), (p.stderr, "err")):
        sel.register(pipe, selectors.EVENT_READ, label)
    deadline = time.monotonic() + timeout
    try:
        while sel.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("read-only command exceeded deadline")
            for key, _ in sel.select(min(remaining, 0.2)):
                chunk = os.read(key.fd, 65536)
                if not chunk:
                    sel.unregister(key.fileobj)
                    continue
                label = key.data
                used[label] += len(chunk)
                if used[label] > (maximum if label == "out" else 65536):
                    raise ValueError("read-only command output exceeded limit")
                if label == "out" and output is not None:
                    output.write(chunk)
                else:
                    captured[label].extend(chunk)
        code = p.wait(timeout=max(0.01, deadline - time.monotonic()))
        if code:
            raise RuntimeError("read-only command failed: " + captured["err"].decode(errors="replace"))
        return bytes(captured["out"])
    finally:
        if p.poll() is None:
            p.kill()
        p.wait()
        sel.close()
        p.stdout.close()
        p.stderr.close()


def read_file(path):
    with pathlib.Path(path).open("rb") as f:
        data = f.read(MAX_FILE + 1)
    if len(data) > MAX_FILE:
        raise ValueError("source file exceeded evidence limit")
    return data.decode("utf-8", errors="strict")


def unescape_mount(value):
    return re.sub(r"\\([0-7]{3})", lambda m: chr(int(m.group(1), 8)), value)


def cgroup_location(cgroup_text, mountinfo):
    """Resolve the process cgroup through mount root, never guess a Docker path."""
    memberships = []
    for line in cgroup_text.splitlines():
        _, controllers, path = line.split(":", 2)
        if not path.startswith("/") or ".." in path.split("/"):
            raise ValueError("invalid process cgroup path")
        memberships.append((controllers.split(","), path))
    choices = []
    for line in mountinfo.splitlines():
        before, after = line.split(" - ", 1)
        fields, fs = before.split(), after.split()
        if len(fields) < 6 or len(fs) < 3:
            raise ValueError("invalid mountinfo")
        root, mount = map(unescape_mount, (fields[3], fields[4]))
        version = 2 if fs[0] == "cgroup2" else 1 if fs[0] == "cgroup" and "memory" in fs[2].split(",") else 0
        for controllers, path in memberships:
            if not version or version == 2 and controllers != [""] or version == 1 and "memory" not in controllers:
                continue
            # A mount can expose only a subtree; subtract its authenticated
            # kernel mount root rather than blindly appending the whole path.
            if path != root and not path.startswith(root.rstrip("/") + "/"):
                continue
            relative = posixpath.relpath(path, root)
            choices.append((len(root), version, pathlib.Path(mount) / relative))
    if not choices:
        raise ValueError("process memory cgroup has no matching host mount")
    _, version, path = max(choices, key=lambda x: (x[1], x[0]))
    return version, path


def process_token(pid, proc=pathlib.Path("/proc")):
    stat = read_file(proc / str(pid) / "stat")
    # comm can contain spaces and parentheses. Fields following the last ')'
    # start at field 3; starttime is field 22 (index 19 of that suffix).
    end = stat.rfind(")")
    suffix = stat[end + 2:].split()
    if end < 0 or len(suffix) < 20 or stat.split(" ", 1)[0] != str(pid):
        raise ValueError("invalid process identity")
    return int(suffix[19])


def owns_profile_listener(pid, port, proc=pathlib.Path("/proc")):
    rows = read_file(proc / str(pid) / "net/tcp").splitlines()[1:]
    sockets = set()
    expected = "0100007F:%04X" % port
    for row in rows:
        fields = row.split()
        if len(fields) >= 10 and fields[1] == expected and fields[3] == "0A":
            sockets.add(fields[9])
    if len(sockets) != 1:
        raise ValueError("diagnostic port has no unique IPv4 loopback listener")
    with os.scandir(proc / str(pid) / "fd") as entries:
        for count, entry in enumerate(entries):
            if count >= 4096:
                raise ValueError("process FD ownership scan exceeded limit")
            try:
                target = os.readlink(entry.path)
            except FileNotFoundError:
                continue
            if target == "socket:[%s]" % next(iter(sockets)):
                return
    raise ValueError("diagnostic listener is not owned by the selected Halro PID")


def validate_profile(path, runtime=False):
    if runtime:
        value = json.loads(read_file(path))
        if not isinstance(value, dict):
            raise ValueError("runtime response is not an object")
        memory = value.get("memory", {})
        if type(value.get("goroutines")) is not int or value["goroutines"] < 1 or not isinstance(memory, dict):
            raise ValueError("runtime response is not the diagnostic MemStats shape")
        for key in ("HeapAlloc", "HeapSys", "Sys", "NumGC"):
            if type(memory.get(key)) is not int or memory[key] < 0:
                raise ValueError("runtime response has invalid MemStats")
    else:
        # Verify complete gzip framing/CRC with a bounded streaming decoder.
        # This does not replace subsequent protobuf/symbol analysis by Go pprof.
        total = 0
        with gzip.open(path, "rb") as f:
            for data in iter(lambda: f.read(65536), b""):
                total += len(data)
                if total > 128 << 20:
                    raise ValueError("profile decoded body exceeded evidence limit")
        if total == 0:
            raise ValueError("profile decoded body is empty")


def executable_identity(pid):
    stat = (pathlib.Path("/proc") / str(pid) / "exe").stat()
    return [stat.st_dev, stat.st_ino, stat.st_size, stat.st_mtime_ns, stat.st_ctime_ns]


def hash_executable(pid):
    path = pathlib.Path("/proc") / str(pid) / "exe"
    identity = executable_identity(pid)
    if identity[2] > 256 << 20:
        raise ValueError("executable exceeds bounded fingerprint limit")
    digest = hashlib.sha256()
    with path.open("rb") as f:
        total = 0
        for data in iter(lambda: f.read(65536), b""):
            total += len(data)
            if total > 256 << 20:
                raise ValueError("executable grew beyond fingerprint limit")
            digest.update(data)
    if executable_identity(pid) != identity:
        raise ValueError("executable changed while fingerprinting")
    return identity, digest.hexdigest()


def select_container(service, container):
    if container:
        if not re.fullmatch(r"[a-f0-9]{12,64}", container):
            raise ValueError("container must be a Docker ID")
        return container
    rows = run(["docker", "ps", "--filter", "label=com.docker.swarm.service.name=" + service,
                "--format", "{{.ID}}", "--no-trunc"]).decode().splitlines()
    if len(rows) != 1 or not re.fullmatch(r"[a-f0-9]{64}", rows[0]):
        raise ValueError("service must match exactly one local Running task; select an explicit container ID otherwise")
    return rows[0]


def inspect(container):
    state = json.loads(run(["docker", "inspect", "--format", INSPECT, container]))
    if not state["running"] or state["paused"] or not isinstance(state["pid"], int) or state["pid"] < 1:
        raise ValueError("selected container is not an unpaused Running task")
    return state


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument("--service")
    target.add_argument("--container")
    parser.add_argument("--pid", type=int, help="actual host Halro PID if container init is a wrapper")
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--samples", type=int, default=6)
    parser.add_argument("--interval", type=float, default=5)
    parser.add_argument("--pprof-port", type=int, help="optional existing container-loopback diagnostic listener")
    args = parser.parse_args(argv)
    if sys.platform != "linux":
        parser.error("run on the Linux Docker host, not Docker Desktop's macOS host")
    if not 1 <= args.samples <= 120 or not 0.1 <= args.interval <= 10 or args.pid is not None and args.pid < 1 or args.pprof_port is not None and not 1 <= args.pprof_port <= 65535:
        parser.error("samples 1..120, interval 0.1..10, positive PID and port 1..65535 required")
    output = args.output.resolve()
    os.umask(0o077)
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    report = {"complete": False, "production_acceptance": False, "samples": [], "profiles": {},
              "scope": "read-only current task; no forced GC, traffic, service update or restart"}
    def save_report():
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    try:
        container = select_container(args.service, args.container)
        initial = inspect(container)
        if args.service and initial["service"] != args.service:
            raise ValueError("container service identity differs")
        pid = args.pid or initial["pid"]
        base = pathlib.Path("/proc") / str(pid)
        init_group = read_file(pathlib.Path("/proc") / str(initial["pid"]) / "cgroup")
        group = read_file(base / "cgroup")
        if group != init_group or read_file(base / "comm").strip() != "halro":
            raise ValueError("PID is not the Halro process in the selected container; pass its exact host --pid")
        token = process_token(pid)
        exe_identity, exe_hash = hash_executable(pid)
        version, cg = cgroup_location(group, read_file("/proc/self/mountinfo"))
        report.update(container=initial, host_pid=pid, process_start_ticks=token,
                      executable_identity=exe_identity, executable_sha256=exe_hash,
                      boot_id=read_file("/proc/sys/kernel/random/boot_id").strip(),
                      cgroup_version=version, cgroup_path=str(cg),
                      kernel=read_file("/proc/sys/kernel/osrelease").strip())
        files = (["memory.current", "memory.max", "memory.events", "memory.stat"] if version == 2 else
                 ["memory.usage_in_bytes", "memory.limit_in_bytes", "memory.failcnt", "memory.oom_control", "memory.stat"])
        optional = ["memory.peak", "memory.swap.current", "memory.swap.max"] if version == 2 else ["memory.max_usage_in_bytes"]
        for i in range(args.samples):
            if i:
                time.sleep(args.interval)
            current = inspect(initial["id"])
            if current != initial or process_token(pid) != token or read_file(base / "cgroup") != group or executable_identity(pid) != exe_identity:
                raise ValueError("task/process identity changed during capture; no mixed curve accepted")
            sample = output / ("sample-%03d" % i)
            sample.mkdir(mode=0o700)
            stamp = datetime.datetime.now(datetime.timezone.utc).isoformat()
            for name in ["status", "smaps_rollup", "cgroup"]:
                (sample / ("proc-" + name + ".txt")).write_text(read_file(base / name))
            for name in files:
                (sample / ("cgroup-" + name + ".txt")).write_text(read_file(cg / name))
            for name in optional:
                if (cg / name).exists():
                    (sample / ("cgroup-" + name + ".txt")).write_text(read_file(cg / name))
            if process_token(pid) != token or read_file(base / "cgroup") != group or executable_identity(pid) != exe_identity:
                raise ValueError("process changed while reading sample")
            report["samples"].append({"ordinal": i, "utc": stamp, "directory": sample.name})
            save_report()
        if args.pprof_port:
            for name, route in [("runtime.json", "runtime"), ("heap.pb.gz", "pprof/heap"), ("allocs.pb.gz", "pprof/allocs")]:
                path = output / name
                try:
                    owns_profile_listener(pid, args.pprof_port)
                    with path.open("xb") as f:
                        run(["nsenter", "--target", str(initial["pid"]), "--net", "curl", "--fail", "--silent",
                             "--show-error", "--max-time", "15", "http://127.0.0.1:%d/debug/%s" % (args.pprof_port, route)],
                            timeout=20, output=f, maximum=64 << 20)
                    validate_profile(path, runtime=name == "runtime.json")
                    owns_profile_listener(pid, args.pprof_port)
                    report["profiles"][name] = {"captured": True, "validation": "diagnostic MemStats" if name == "runtime.json" else "complete bounded gzip/CRC; protobuf and symbols still require Go pprof"}
                except (OSError, EOFError, RuntimeError, TimeoutError, ValueError) as error:
                    path.unlink(missing_ok=True)
                    report["profiles"][name] = {"captured": False, "error": str(error)}
            if inspect(initial["id"]) != initial or process_token(pid) != token or executable_identity(pid) != exe_identity:
                raise ValueError("task changed while reading optional profiles")
        report["complete"] = True
        report["profile_requested"] = bool(args.pprof_port)
        report["profile_complete"] = bool(args.pprof_port) and all(x["captured"] for x in report["profiles"].values())
        report["oom_boundary"] = "Current task OOMKilled/events only; does not correlate earlier task 137 or kernel history"
        return 0
    except (OSError, EOFError, RuntimeError, TimeoutError, ValueError) as error:
        report["error"] = str(error)
        return 1
    finally:
        save_report()
        hashes = []
        for path in sorted(output.rglob("*")):
            if path.is_file():
                h = hashlib.sha256()
                with path.open("rb") as f:
                    for data in iter(lambda: f.read(65536), b""):
                        h.update(data)
                hashes.append({"path": str(path.relative_to(output)), "sha256": h.hexdigest()})
        (output / "sha256.json").write_text(json.dumps(hashes, indent=2) + "\n")


if __name__ == "__main__":
    raise SystemExit(main())
