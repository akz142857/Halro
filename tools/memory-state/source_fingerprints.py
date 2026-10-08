"""Freeze compile inputs, embedded files and the local acceptance tools.

Documentation evidence is excluded so writing a result cannot invalidate its
own source freeze. Embedded configuration/UI bytes are source even without a
Go extension; omitting them can hide a changed binary or template test input.
"""

import hashlib
import shlex


def fingerprints(root):
    paths = {root / "go.mod", root / "go.sum",
             root / "internal/config/default.yaml",
             root / "configs/config.example.yaml",
             root / "docs/contracts/memory-bounded-fault-matrix.json"}
    for directory in ("cmd", "internal", "tools", "configs", "deploy", "tests", "docs"):
        paths.update((root / directory).rglob("*.go"))
    paths.update((root / "tools/memory-state").glob("*.py"))
    paths.update((root / "tools/memory-state").glob("*.sh"))
    paths.update(path for path in (root / "internal/webui/dist").rglob("*")
                 if path.is_file())
    # Embedded runbooks are binary input too. Follow the declarations so a new
    # embedded document cannot silently disappear from a frozen build.
    for source in tuple(paths):
        if source.suffix != ".go":
            continue
        for line in source.read_text().splitlines():
            if not line.startswith("//go:embed "):
                continue
            for pattern in shlex.split(line[len("//go:embed "):]):
                for embedded in source.parent.glob(pattern.removeprefix("all:")):
                    if embedded.is_file():
                        paths.add(embedded)
                    elif embedded.is_dir():
                        paths.update(p for p in embedded.rglob("*") if p.is_file())
    return {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(paths)}
