#!/usr/bin/env python3
"""Regenerate THIRD_PARTY_NOTICES from the Go module graph of this tree.

Run from the module root (or via `make licenses`). Requires `go` on PATH.
The file is the attribution document for source and binary distributions.
"""

from __future__ import annotations

import hashlib
import os
import subprocess
import sys
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "THIRD_PARTY_NOTICES"
MAIN = "github.com/darwin-node/darwin-node"

LICENSE_PREFIXES = ("LICENSE", "COPYING", "LICENCE")
NOTICE_PREFIXES = ("NOTICE",)
SKIP_LICENSE_SUFFIXES = (".docs",)  # documentation-only siblings


def go_bin() -> str:
    env = os.environ.get("GO")
    if env:
        return env
    local = Path.home() / ".local" / "go" / "bin" / "go"
    if local.is_file():
        return str(local)
    return "go"


def go_list_modules() -> list[tuple[str, str, str]]:
    env = os.environ.copy()
    path = env.get("PATH", "")
    local_bin = str(Path.home() / ".local" / "go" / "bin")
    if local_bin not in path.split(os.pathsep):
        env["PATH"] = local_bin + os.pathsep + path
    proc = subprocess.run(
        [
            go_bin(),
            "list",
            "-deps",
            "-f",
            "{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}{{end}}",
            "./...",
        ],
        cwd=ROOT,
        env=env,
        check=True,
        capture_output=True,
        text=True,
    )
    seen: dict[str, tuple[str, str, str]] = {}
    for line in proc.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        parts = line.split("\t", 2)
        if len(parts) < 3:
            continue
        path, ver, directory = parts
        if path == MAIN or not directory:
            continue
        seen[path] = (path, ver, directory)
    return [seen[k] for k in sorted(seen)]


def list_attr_files(directory: str) -> tuple[list[Path], list[Path]]:
    licenses: list[Path] = []
    notices: list[Path] = []
    try:
        names = os.listdir(directory)
    except OSError:
        return licenses, notices
    for name in names:
        p = Path(directory) / name
        if not p.is_file():
            continue
        up = name.upper()
        if any(up.startswith(pref) for pref in NOTICE_PREFIXES):
            notices.append(p)
            continue
        if any(up.startswith(pref) for pref in LICENSE_PREFIXES):
            if any(up.endswith(suf.upper()) for suf in SKIP_LICENSE_SUFFIXES):
                continue
            licenses.append(p)
    licenses.sort(key=lambda p: p.name.lower())
    notices.sort(key=lambda p: p.name.lower())
    return licenses, notices


def classify(text: str) -> str:
    if "Apache License" in text and "Version 2.0" in text:
        return "Apache-2.0"
    if "Mozilla Public License" in text:
        return "MPL-2.0"
    if "ISC License" in text or (
        "Permission to use, copy, modify, and/or distribute this software for any"
        in text
        and "THE SOFTWARE IS PROVIDED" in text
        and "AS IS" in text
    ):
        return "ISC"
    if "MIT License" in text or (
        "Permission is hereby granted, free of charge" in text
        and "THE SOFTWARE IS PROVIDED" in text
    ):
        return "MIT"
    if "Redistribution and use" in text:
        if "neither the name" in text.lower():
            return "BSD-3-Clause"
        return "BSD-2-Clause"
    return "OTHER"


def read_text(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="replace").replace("\r\n", "\n").strip() + "\n"


def spdx_for_files(files: list[Path]) -> str:
    tags: list[str] = []
    for f in files:
        tags.append(classify(read_text(f)))
    # Preserve order, unique
    out: list[str] = []
    for t in tags:
        if t not in out:
            out.append(t)
    return " AND ".join(out) if out else "UNKNOWN"


def main() -> int:
    mods = go_list_modules()
    if not mods:
        print("no modules found; is go available?", file=sys.stderr)
        return 1

    inventory: list[tuple[str, str, str]] = []
    notice_blocks: list[tuple[str, str, str]] = []
    # hash -> (spdx, text, modules)
    license_groups: dict[str, tuple[str, str, list[str]]] = {}
    missing: list[str] = []

    for path, ver, directory in mods:
        licenses, notices = list_attr_files(directory)
        if not licenses:
            missing.append(f"{path} {ver}")
            inventory.append((path, ver, "UNKNOWN (no LICENSE file in module root)"))
            continue
        spdx = spdx_for_files(licenses)
        inventory.append((path, ver, spdx))
        for n in notices:
            notice_blocks.append((path, ver, read_text(n)))
        for lic in licenses:
            text = read_text(lic)
            digest = hashlib.sha256(text.encode()).hexdigest()
            spdx_one = classify(text)
            if digest not in license_groups:
                license_groups[digest] = (spdx_one, text, [])
            license_groups[digest][2].append(f"{path} {ver} ({lic.name})")

    # Full texts: every non-Apache body, plus Apache bodies that are not the
    # stock Apache 2.0 (rare). Stock Apache 2.0 is already LICENSE in this repo.
    full_texts: list[tuple[str, str, list[str]]] = []
    apache_stock: list[str] = []
    for digest, (spdx, text, modules) in sorted(
        license_groups.items(), key=lambda kv: (kv[1][0], kv[1][2][0] if kv[1][2] else "")
    ):
        modules_sorted = sorted(modules)
        if spdx == "Apache-2.0" and "http://www.apache.org/licenses/LICENSE-2.0" in text:
            apache_stock.extend(modules_sorted)
            continue
        full_texts.append((spdx, text, modules_sorted))

    lines: list[str] = []
    w = lines.append
    w("Third-party notices for darwin-node")
    w("====================================")
    w("")
    w("Generated by hack/gen-third-party-notices.py (`make licenses`) from")
    w("`go list -deps ./...`. Do not edit by hand.")
    w("")
    w("darwin-node itself is Apache License 2.0; see LICENSE and NOTICE.")
    w("This file is the attribution document for source *and* binary")
    w("distributions: MIT and BSD-3-Clause require their copyright and")
    w("permission notices in all copies, including statically linked binaries.")
    w("")
    w("Agoda macOS-vz-kubelet (Apache-2.0) is a lineage dependency of this")
    w("tree, not a Go module. Its attribution is in NOTICE.")
    w("")
    w("Inventory")
    w("---------")
    w("")
    width_path = max(len(p) for p, _, _ in inventory)
    width_ver = max(len(v) for _, v, _ in inventory)
    w(f"{'Module'.ljust(width_path)}  {'Version'.ljust(width_ver)}  License")
    w(f"{'-' * width_path}  {'-' * width_ver}  -------")
    for path, ver, spdx in inventory:
        w(f"{path.ljust(width_path)}  {ver.ljust(width_ver)}  {spdx}")
    w("")
    if missing:
        w("Modules with no LICENSE file in the module root:")
        for m in missing:
            w(f"  - {m}")
        w("")

    w("Apache-2.0 modules")
    w("------------------")
    w("")
    w("The Apache License, Version 2.0 text is the same as LICENSE in this")
    w("repository. Modules whose LICENSE is that text:")
    w("")
    for m in sorted(apache_stock):
        w(f"  - {m}")
    w("")

    w("NOTICE files from Apache-2.0 (and other) dependencies")
    w("-----------------------------------------------------")
    w("")
    w("Apache-2.0 section 4(d) requires reproducing these attribution notices.")
    w("")
    if not notice_blocks:
        w("(none found)")
        w("")
    else:
        for path, ver, text in notice_blocks:
            w(f"### {path} {ver}")
            w("")
            w("```")
            w(text.rstrip("\n"))
            w("```")
            w("")

    w("Other license texts (verbatim)")
    w("------------------------------")
    w("")
    w("Each unique LICENSE body that is not the stock Apache-2.0 text,")
    w("including MIT, BSD, ISC, and dual-licensed files.")
    w("")
    for i, (spdx, text, modules) in enumerate(full_texts, 1):
        w(f"### {spdx} ({i})")
        w("")
        w("Used by:")
        for m in modules:
            w(f"  - {m}")
        w("")
        w("```")
        w(text.rstrip("\n"))
        w("```")
        w("")

    required = (
        "Copyright (c) 2025 codehex",
        "WAKAYAMA Shirou",
        "Copyright 2009 The Go Authors",
    )
    body = "\n".join(lines) + "\n"
    for needle in required:
        if needle not in body:
            print(f"generated notices missing required string: {needle}", file=sys.stderr)
            return 1

    OUT.write_text(body, encoding="utf-8")
    print(f"wrote {OUT} ({len(mods)} modules, {len(full_texts)} unique non-Apache license bodies)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
