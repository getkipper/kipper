#!/usr/bin/env python3
"""License compliance gate for Kipper's dependencies.

Kipper ships under Apache-2.0 and must stay free of copyleft (GPL/AGPL/LGPL/SSPL)
dependencies. This script audits every imported Go module (including test-only
imports) and every npm package across all manifests, and exits non-zero if a
disallowed licence appears.

Policy:
  - PERMISSIVE (Apache-2.0, MIT, BSD, ISC, CC0, 0BSD, ...): allowed.
  - WEAK-COPYLEFT (MPL-2.0): allowed only for the specific dependencies listed
    in ACCEPTED_WEAK. A new MPL-2.0 dependency fails the gate until it has been
    reviewed and added there.
  - COPYLEFT (GPL/AGPL/LGPL/SSPL) and unrecognised licences: fail the build.

The standard tools (go-licenses, lichen) don't work on Go 1.25 workspaces, so
this classifies licence files directly. GPL-family licences always carry their
distinctive title text, so real copyleft is caught reliably.
"""

import glob
import json
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GO_MODULES = ["kip", "console-api", "controller", "gateway", "sidecar", "kipper-poll", "authz"]
NPM_DIRS = ["console", "docs", "kipper-runtime-node"]

# Pin the scanner so a new release can't turn an unrelated commit red.
LICENSE_CHECKER = "license-checker@25.0.1"

# Weak-copyleft dependencies reviewed and accepted, keyed by module/package name
# to its SPDX id. go-sql-driver/mysql (MPL-2.0) is imported unmodified, so it
# imposes no terms on Kipper's own code.
#
# lightningcss comes in under vite as a build-time CSS transformer. The console's
# Dockerfile builds in one stage and copies only the built assets into the runtime
# image, so none of it is distributed. The platform binaries are listed one by one
# so that a lightningcss-* package nobody has looked at still fails the gate.
ACCEPTED_WEAK = {
    "github.com/go-sql-driver/mysql": "MPL-2.0",
    "lightningcss": "MPL-2.0",
    "lightningcss-android-arm64": "MPL-2.0",
    "lightningcss-darwin-arm64": "MPL-2.0",
    "lightningcss-darwin-x64": "MPL-2.0",
    "lightningcss-freebsd-x64": "MPL-2.0",
    "lightningcss-linux-arm-gnueabihf": "MPL-2.0",
    "lightningcss-linux-arm64-gnu": "MPL-2.0",
    "lightningcss-linux-arm64-musl": "MPL-2.0",
    "lightningcss-linux-x64-gnu": "MPL-2.0",
    "lightningcss-linux-x64-musl": "MPL-2.0",
    "lightningcss-win32-arm64-msvc": "MPL-2.0",
    "lightningcss-win32-x64-msvc": "MPL-2.0",
}

OWN_GO_PREFIX = "github.com/getkipper/kipper"

LICENSE_FILE_NAMES = [
    "LICENSE", "LICENCE", "COPYING", "LICENSE.md", "LICENSE.txt",
    "LICENSE-MIT", "LICENSE.MIT", "LICENSE-APACHE", "COPYRIGHT", "License",
]

# Category severity. "worst" (most restrictive first) combines an SPDX AND -
# you must comply with every licence. "best" (most permissive first) combines an
# SPDX OR - you may choose the least restrictive arm.
SEVERITY = ["COPYLEFT", "UNKNOWN", "WEAK-COPYLEFT", "PERMISSIVE"]


def worst(categories):
    for c in SEVERITY:
        if c in categories:
            return c
    return "PERMISSIVE"


def best(categories):
    for c in reversed(SEVERITY):
        if c in categories:
            return c
    return "UNKNOWN"


def find_licence_file(d):
    if not d or not os.path.isdir(d):
        return None
    for name in LICENSE_FILE_NAMES:
        p = os.path.join(d, name)
        if os.path.isfile(p):
            return p
    for p in glob.glob(os.path.join(d, "*")):
        b = os.path.basename(p).lower()
        if b.startswith("licen") or b.startswith("copying") or b == "unlicense":
            if os.path.isfile(p):
                return p
    return None


def classify_text(text):
    """Classify a licence file's text into (spdx-ish id, category)."""
    t = text.upper()
    # Weak-copyleft licences name the GPL family in their own text (MPL's
    # "Secondary Licenses" clause lists GPL/LGPL/AGPL), so match them first to
    # avoid a false copyleft flag.
    if "MOZILLA PUBLIC LICENSE" in t:
        return "MPL-2.0", "WEAK-COPYLEFT"
    if "ECLIPSE PUBLIC LICENSE" in t:
        return "EPL", "WEAK-COPYLEFT"
    if "COMMON DEVELOPMENT AND DISTRIBUTION" in t:
        return "CDDL", "WEAK-COPYLEFT"
    if "APACHE LICENSE" in t:
        return "Apache-2.0", "PERMISSIVE"
    if "AFFERO GENERAL PUBLIC LICENSE" in t:
        return "AGPL", "COPYLEFT"
    if "LESSER GENERAL PUBLIC LICENSE" in t:
        return "LGPL", "COPYLEFT"
    if "GNU GENERAL PUBLIC LICENSE" in t:
        return "GPL", "COPYLEFT"
    if "SERVER SIDE PUBLIC LICENSE" in t:
        return "SSPL", "COPYLEFT"
    if "MIT LICENSE" in t or "PERMISSION IS HEREBY GRANTED, FREE OF CHARGE" in t:
        return "MIT", "PERMISSIVE"
    if "REDISTRIBUTION AND USE IN SOURCE AND BINARY FORMS" in t:
        return "BSD", "PERMISSIVE"
    if "ISC LICENSE" in t or "PERMISSION TO USE, COPY, MODIFY, AND" in t:
        return "ISC", "PERMISSIVE"
    if "CC0 1.0" in t or "UNENCUMBERED SOFTWARE RELEASED INTO THE PUBLIC DOMAIN" in t:
        return "CC0-1.0", "PERMISSIVE"
    if "BLUE OAK MODEL LICENSE" in t:
        return "BlueOak-1.0.0", "PERMISSIVE"
    if "ZLIB" in t or "ALTERED SOURCE VERSIONS" in t:
        return "Zlib", "PERMISSIVE"
    return "UNKNOWN", "UNKNOWN"


PERMISSIVE_TOKENS = ("MIT", "APACHE", "BSD", "ISC", "0BSD", "CC0",
                     "UNLICENSE", "BLUEOAK", "BLUE-OAK", "PYTHON-2.0", "ZLIB",
                     "WTFPL")


def classify_token(tok):
    u = tok.upper().rstrip("+")
    if not u or u in ("AND", "OR", "WITH", "SEE", "CUSTOM"):
        return None  # operator / non-licence token
    if "AGPL" in u or "LGPL" in u or "SSPL" in u:
        return "COPYLEFT"
    if "GPL" in u:
        return "COPYLEFT"
    if "MPL" in u or "EPL" in u or "CDDL" in u:
        return "WEAK-COPYLEFT"
    if "CC-BY" in u:
        # Attribution-only CC-BY is permissive. Share-alike (SA) is copyleft,
        # and non-commercial (NC) or no-derivatives (ND) are not open source,
        # so none of those may pass as permissive.
        if any(x in u for x in ("-SA", "-NC", "-ND")):
            return "COPYLEFT"
        return "PERMISSIVE"
    if any(p in u for p in PERMISSIVE_TOKENS):
        return "PERMISSIVE"
    return "UNKNOWN"


def classify_spdx(spdx):
    """Classify an npm SPDX expression, respecting AND/OR/WITH precedence.

    OR picks the least restrictive arm (dual-licensed, you may choose); AND and
    WITH require complying with every part, so the most restrictive wins."""
    toks = re.findall(r"\(|\)|[^\s()]+", spdx.replace("/", " OR "))
    if not toks:
        return spdx, "UNKNOWN"
    pos = [0]

    def peek():
        return toks[pos[0]] if pos[0] < len(toks) else None

    def parse_or():
        cats = [parse_and()]
        while peek() and peek().upper() == "OR":
            pos[0] += 1
            cats.append(parse_and())
        return best(cats)

    def parse_and():
        cats = [parse_with()]
        while peek() and peek().upper() == "AND":
            pos[0] += 1
            cats.append(parse_with())
        return worst(cats)

    def parse_with():
        cats = [parse_atom()]
        while peek() and peek().upper() == "WITH":
            pos[0] += 1
            cats.append(parse_atom())
        return worst(cats)

    def parse_atom():
        t = peek()
        if t == "(":
            pos[0] += 1
            cat = parse_or()
            if peek() == ")":
                pos[0] += 1
            return cat
        if t is None:
            return "UNKNOWN"
        pos[0] += 1
        return classify_token(t) or "UNKNOWN"

    try:
        return spdx, parse_or()
    except Exception:
        return spdx, "UNKNOWN"


def licence_of_dir(d):
    lf = find_licence_file(d)
    if not lf:
        return None
    with open(lf, "r", errors="ignore") as f:
        return classify_text(f.read())


def audit_go():
    """Return rows of (name, version, licence, category) for imported Go modules,
    including test-only imports. Each module is classified by its own root licence
    and by the licence at every imported package directory, worst category wins."""
    mods = {}  # path@version -> {"path","version","dirs":set}
    for m in GO_MODULES:
        env = dict(os.environ, GOWORK="off")
        out = subprocess.run(
            ["go", "list", "-deps", "-test",
             "-f", "{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}\t{{.Dir}}{{end}}",
             "./..."],
            cwd=os.path.join(REPO, m), env=env, capture_output=True, text=True,
        )
        if out.returncode != 0:
            print(f"error: go list failed in {m}:\n{out.stderr}", file=sys.stderr)
            sys.exit(2)
        for line in out.stdout.splitlines():
            parts = line.split("\t")
            if len(parts) < 3 or not parts[0]:
                continue
            path, version, mod_dir = parts[0], parts[1], parts[2]
            pkg_dir = parts[3] if len(parts) > 3 else ""
            if path.startswith(OWN_GO_PREFIX):
                continue
            e = mods.setdefault(f"{path}@{version}",
                                {"path": path, "version": version, "dirs": set()})
            if mod_dir:
                e["dirs"].add(mod_dir)
            if pkg_dir:
                e["dirs"].add(pkg_dir)

    rows = []
    for e in mods.values():
        results = [licence_of_dir(d) for d in e["dirs"]]
        results = [r for r in results if r]
        if not results:
            rows.append((e["path"], e["version"], "NO-FILE", "UNKNOWN"))
            continue
        cat = worst([c for _, c in results])
        lic = next((l for l, c in results if c == cat), results[0][0])
        rows.append((e["path"], e["version"], lic, cat))
    return rows


def own_npm_names():
    names = set()
    for d in NPM_DIRS:
        pj = os.path.join(REPO, d, "package.json")
        if os.path.isfile(pj):
            with open(pj) as f:
                names.add(json.load(f).get("name", ""))
    return {n for n in names if n}


def audit_npm():
    """Return rows of (name, version, licence, category) across all npm manifests."""
    own = own_npm_names()
    rows = []
    for d in NPM_DIRS:
        node_modules = os.path.join(REPO, d, "node_modules")
        if not os.path.isdir(node_modules):
            print(f"error: {d}/node_modules missing; run npm install first",
                  file=sys.stderr)
            sys.exit(2)
        out = subprocess.run(
            ["npx", "--yes", LICENSE_CHECKER, "--json"],
            cwd=os.path.join(REPO, d), capture_output=True, text=True,
        )
        if out.returncode != 0:
            print(f"error: license-checker failed in {d}:\n{out.stderr}", file=sys.stderr)
            sys.exit(2)
        for name_version, info in json.loads(out.stdout).items():
            name = name_version.rsplit("@", 1)[0]
            version = name_version.rsplit("@", 1)[1] if "@" in name_version else ""
            if name in own:
                continue
            spdx = info.get("licenses", "")
            if isinstance(spdx, list):
                spdx = " AND ".join(spdx)
            lic, cat = classify_spdx(str(spdx))
            rows.append((name, version, lic, cat))
    return rows


def report(title, rows, violations):
    from collections import Counter
    cats = Counter(r[3] for r in rows)
    print(f"\n{title}: {len(rows)} dependencies")
    for c, n in cats.most_common():
        print(f"  {c:15} {n}")
    for name, version, lic, cat in rows:
        if cat == "COPYLEFT":
            violations.append(f"[{title}] {name} {version} -> {lic} (copyleft, not allowed)")
        elif cat == "WEAK-COPYLEFT" and ACCEPTED_WEAK.get(name) != lic:
            violations.append(f"[{title}] {name} {version} -> {lic} (weak copyleft, not in ACCEPTED_WEAK)")
        elif cat == "UNKNOWN":
            violations.append(f"[{title}] {name} {version} -> {lic} (unrecognised licence, verify manually)")


def main():
    violations = []
    go_rows = audit_go()
    report("Go", go_rows, violations)
    npm_rows = audit_npm()
    report("npm", npm_rows, violations)

    accepted = sorted({f"{n} {v} ({l})" for n, v, l, c in go_rows + npm_rows
                       if c == "WEAK-COPYLEFT" and ACCEPTED_WEAK.get(n) == l})
    if accepted:
        print("\nAccepted weak-copyleft dependencies:")
        for a in accepted:
            print(f"  {a}")

    if violations:
        print(f"\nFAIL: {len(violations)} licence violation(s):")
        for v in violations:
            print(f"  {v}")
        print("\nAllowed: permissive licences, plus the specific MPL-2.0 "
              "dependencies listed in ACCEPTED_WEAK. To accept a new weak-copyleft "
              "dependency, review it and add its name to ACCEPTED_WEAK in this script.")
        sys.exit(1)

    print("\nOK: all dependencies are permissively licensed.")


if __name__ == "__main__":
    main()
