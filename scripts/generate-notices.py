#!/usr/bin/env python3
"""Generate THIRD_PARTY_NOTICES.md from Kipper's shipped dependencies.

Lists every third-party component compiled into Kipper's distributed artifacts
(the kip binary, the Go service images, and the console/runtime bundles) together
with its license text, so the distribution carries the attribution and notices
those licenses require. The MPL-2.0 MySQL driver is called out explicitly to
satisfy MPL-2.0 section 3.2 (source availability of the covered component).

Run from anywhere, e.g. from the repository root: python3 scripts/generate-notices.py
Only shipped dependencies are included (test-only Go deps and npm devDependencies
are excluded, since they are not distributed).
"""

import glob
import json
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GO_MODULES = ["kip", "console-api", "controller", "gateway", "sidecar", "kipper-poll", "authz"]
# npm bundles that ship in a distributed artifact (the docs site is a separate
# distribution and is not part of the product images).
NPM_DIRS = ["console", "kipper-runtime-node"]
LICENSE_CHECKER = "license-checker@25.0.1"
OWN_GO_PREFIX = "github.com/getkipper/kipper"

MIRROR_DOCKERFILE = os.path.join("images", "kipper-mirror", "Dockerfile")

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
    """Return the first recognized NOTICE file path, or None if absent."""
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


def rclone_version():
    """Read the mirror image's rclone release from its Dockerfile.

    Exit with status 2 if RCLONE_VERSION is missing.
    """
    with open(os.path.join(REPO, MIRROR_DOCKERFILE)) as f:
        m = re.search(r"^ARG RCLONE_VERSION=(\S+)$", f.read(), re.MULTILINE)
    if not m:
        print(f"error: no RCLONE_VERSION in {MIRROR_DOCKERFILE}", file=sys.stderr)
        sys.exit(2)
    return m.group(1)


def render(go, npm, rclone):
    out = []
    out.append("# Third-party notices")
    out.append("")
    out.append("Kipper is licensed under the Apache License 2.0 (see `LICENSE`).")
    out.append("Its distributed binaries and container images include the "
               "third-party open-source components listed below. Each is the "
               "property of its respective authors and is provided under the "
               "license shown with it. This file ships alongside every GitHub "
               "release and inside the console-api, authz and kipper-mirror container images. "
               "The copy at the repository root is the canonical one.")
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
                   "Kipper's own code is not covered by the MPL-2.0; the license "
                   "applies only to that component's own files. The full license "
                   "text is included in the Go components section below.")
        out.append("")
    out.append("## GNU Affero General Public License 3.0 components")
    out.append("")
    out.append("Kipper's registry serves container images of the MinIO server "
               "(`ghcr.io/getkipper/minio`, release RELEASE.2025-09-07T16-13-09Z) "
               "and the MinIO client (`ghcr.io/getkipper/mc`, release "
               "RELEASE.2025-08-13T08-35-41Z). Both are licensed under the GNU "
               "Affero General Public License 3.0 (AGPL-3.0) and are built "
               "unmodified from their upstream source. Under AGPL-3.0, the "
               "Corresponding Source is available at "
               "https://github.com/minio/minio/tree/RELEASE.2025-09-07T16-13-09Z "
               "and https://github.com/minio/mc/tree/RELEASE.2025-08-13T08-35-41Z, "
               "and the build recipe is `images/minio` in this repository. Each "
               "image carries, under `/licenses`, the full license text, the "
               "attributions and licenses of the dependencies built into it "
               "(upstream's `CREDITS`), and these source directions. Kipper's "
               "own code is not covered by the AGPL-3.0; it runs these images "
               "as separate programs.")
    out.append("")
    out.append("## Component of the backup mirror image")
    out.append("")
    out.append("The backup mirror image (`ghcr.io/getkipper/kipper-mirror`) includes "
               f"rclone {rclone}, licensed under the MIT License and built "
               "unmodified from https://github.com/rclone/rclone/tree/"
               f"{rclone}. The build recipe is `{MIRROR_DOCKERFILE}` in this "
               "repository. The image carries rclone's license, and the license "
               "and notice files of every module compiled into rclone, under "
               "`/licenses/rclone`.")
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
            out.append("_License text not bundled; see the component's source "
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
            out.append(f"License: {c['licence']}")
            out.append("")
        if c["text"]:
            out.append("```")
            out.append(c["text"])
            out.append("```")
        else:
            out.append("_License text not bundled; see the component's source "
                       "repository._")
        out.append("")
    return "\n".join(out).rstrip() + "\n"


def main():
    go = go_components()
    npm = npm_components()
    content = render(go, npm, rclone_version())
    dest = os.path.join(REPO, "THIRD_PARTY_NOTICES.md")
    with open(dest, "w") as f:
        f.write(content)
    print(f"wrote {dest}: {len(go)} Go + {len(npm)} JavaScript components")


if __name__ == "__main__":
    main()
