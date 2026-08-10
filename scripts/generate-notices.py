#!/usr/bin/env python3
"""Generate THIRD_PARTY_NOTICES.md from Kipper's shipped dependencies.

Lists every third-party component compiled into Kipper's distributed artifacts
(the kip binary, the Go service images, and the console/runtime bundles) together
with its licence text, so the distribution carries the attribution and notices
those licences require. The MPL-2.0 MySQL driver is called out explicitly to
satisfy MPL-2.0 section 3.2 (source availability of the covered component).

Run from anywhere: python3 scripts/generate-notices.py
Only shipped dependencies are included (test-only Go deps and npm devDependencies
are excluded, since they are not distributed).
"""

import glob
import json
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GO_MODULES = ["kip", "console-api", "controller", "gateway", "sidecar", "kipper-poll", "authz"]
# npm bundles that ship in a distributed artifact (the docs site is a separate
# distribution and is not part of the product images).
NPM_DIRS = ["console", "kipper-runtime-node"]
LICENSE_CHECKER = "license-checker@25.0.1"
OWN_GO_PREFIX = "github.com/getkipper/kipper"

LICENSE_FILE_NAMES = [
    "LICENSE", "LICENCE", "COPYING", "LICENSE.md", "LICENSE.txt",
    "LICENSE-MIT", "LICENSE.MIT", "LICENSE-APACHE", "COPYRIGHT", "License",
]


def find_licence_file(d):
    if not d or not os.path.isdir(d):
        return None
    for name in LICENSE_FILE_NAMES:
        p = os.path.join(d, name)
        if os.path.isfile(p):
            return p
    for p in sorted(glob.glob(os.path.join(d, "*"))):
        b = os.path.basename(p).lower()
        if (b.startswith("licen") or b.startswith("copying") or b == "unlicense") \
                and os.path.isfile(p):
            return p
    return None


NOTICE_FILE_NAMES = ["NOTICE", "NOTICE.txt", "NOTICE.md"]


def find_notice_file(d):
    """Apache-2.0 section 4(d): a bundled NOTICE file's attributions must ship
    with the distribution, so it is carried alongside the licence text."""
    if not d or not os.path.isdir(d):
        return None
    for name in NOTICE_FILE_NAMES:
        p = os.path.join(d, name)
        if os.path.isfile(p):
            return p
    return None


def read_licence(path):
    if not path:
        return None
    with open(path, "r", errors="ignore") as f:
        return f.read().strip()


def go_components():
    seen = {}
    for m in GO_MODULES:
        # Pin the target platform so the output is reproducible regardless of the
        # host, and reflects what actually ships (Linux container images).
        env = dict(os.environ, GOWORK="off", GOOS="linux", GOARCH="amd64")
        out = subprocess.run(
            ["go", "list", "-deps",
             "-f", "{{if .Module}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}",
             "./..."],
            cwd=os.path.join(REPO, m), env=env, capture_output=True, text=True,
        )
        if out.returncode != 0:
            print(f"error: go list failed in {m}:\n{out.stderr}", file=sys.stderr)
            sys.exit(2)
        for line in out.stdout.splitlines():
            parts = line.split("|")
            if len(parts) < 3 or not parts[0] or parts[0].startswith(OWN_GO_PREFIX):
                continue
            path, version, d = parts
            seen[path] = {"name": path, "version": version,
                          "text": read_licence(find_licence_file(d)),
                          "notice": read_licence(find_notice_file(d))}
    return [seen[k] for k in sorted(seen)]


def own_npm_names():
    names = set()
    for d in NPM_DIRS:
        pj = os.path.join(REPO, d, "package.json")
        if os.path.isfile(pj):
            with open(pj) as f:
                names.add(json.load(f).get("name", ""))
    return {n for n in names if n}


def npm_components():
    own = own_npm_names()
    seen = {}
    for d in NPM_DIRS:
        if not os.path.isdir(os.path.join(REPO, d, "node_modules")):
            print(f"error: {d}/node_modules missing; run npm install first",
                  file=sys.stderr)
            sys.exit(2)
        out = subprocess.run(
            ["npx", "--yes", LICENSE_CHECKER, "--production", "--json"],
            cwd=os.path.join(REPO, d), capture_output=True, text=True,
        )
        if out.returncode != 0:
            print(f"error: license-checker failed in {d}:\n{out.stderr}", file=sys.stderr)
            sys.exit(2)
        for name_version, info in json.loads(out.stdout).items():
            name = name_version.rsplit("@", 1)[0]
            if name in own:
                continue
            version = name_version.rsplit("@", 1)[1] if "@" in name_version else ""
            seen[name] = {"name": name, "version": version,
                          "licence": info.get("licenses", ""),
                          "text": read_licence(info.get("licenseFile"))}
    return [seen[k] for k in sorted(seen)]


def render(go, npm):
    out = []
    out.append("# Third-party notices")
    out.append("")
    out.append("Kipper is licensed under the Apache License 2.0 (see `LICENSE`).")
    out.append("Its distributed binaries and container images include the "
               "third-party open-source components listed below. Each is the "
               "property of its respective authors and is provided under the "
               "licence shown with it. This file ships alongside every GitHub "
               "release and inside the console-api and authz container images; "
               "the copy at the repository root is the canonical one.")
    out.append("")
    # Point the MPL-2.0 source link at the exact version that ships, read from
    # the scanned components so a dependency bump can't leave a stale tag here.
    mysql_version = next(
        (c["version"] for c in go if c["name"] == "github.com/go-sql-driver/mysql"),
        None,
    )
    if mysql_version:
        out.append("## Mozilla Public License 2.0 component")
        out.append("")
        out.append("`github.com/go-sql-driver/mysql` is licensed under the Mozilla "
                   "Public License 2.0 (MPL-2.0) and is included unmodified. Under "
                   "MPL-2.0 section 3.2, its Source Code Form is available at "
                   f"https://github.com/go-sql-driver/mysql/tree/{mysql_version}. "
                   "Kipper's own code is not covered by the MPL-2.0; the licence "
                   "applies only to that component's own files. The full licence "
                   "text is included in the Go components section below.")
        out.append("")
    out.append(f"## Go components ({len(go)})")
    out.append("")
    for c in go:
        out.append(f"### {c['name']} {c['version']}".rstrip())
        out.append("")
        if c["text"]:
            out.append("```")
            out.append(c["text"])
            out.append("```")
        else:
            out.append("_Licence text not bundled; see the component's source "
                       "repository._")
        if c.get("notice"):
            out.append("")
            out.append("NOTICE:")
            out.append("")
            out.append("```")
            out.append(c["notice"])
            out.append("```")
        out.append("")
    out.append(f"## JavaScript components ({len(npm)})")
    out.append("")
    for c in npm:
        header = f"### {c['name']} {c['version']}".rstrip()
        out.append(header)
        out.append("")
        if c.get("licence"):
            out.append(f"Licence: {c['licence']}")
            out.append("")
        if c["text"]:
            out.append("```")
            out.append(c["text"])
            out.append("```")
        else:
            out.append("_Licence text not bundled; see the component's source "
                       "repository._")
        out.append("")
    return "\n".join(out).rstrip() + "\n"


def main():
    go = go_components()
    npm = npm_components()
    content = render(go, npm)
    dest = os.path.join(REPO, "THIRD_PARTY_NOTICES.md")
    with open(dest, "w") as f:
        f.write(content)
    print(f"wrote {dest}: {len(go)} Go + {len(npm)} JavaScript components")


if __name__ == "__main__":
    main()
