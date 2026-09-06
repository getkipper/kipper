#!/usr/bin/env python3
"""Find private data before it reaches the public repository.

The scan walks git objects rather than the working tree, so a value that was
committed and then removed inside the same push is still found. Four checks
run over every changed blob, path name and commit message:

  name     a word from PRIVATE_NAME_PATTERN (client and internal names)
  address  a public IPv4 or IPv6 literal that is not on the allow-list
  domain   a hostname whose suffix is not on the allow-list

Images and recordings are read with tesseract when it is installed, because
a terminal recording once carried a server address no text search could see.

Exit codes: 0 clean, 1 findings, 2 the scan itself could not run.

Usage:
  private-data-scan.py --range OLD..NEW [--redact]
  private-data-scan.py --new SHA --remote NAME [--redact]
  private-data-scan.py --tree REV [--redact]
"""
import argparse
import ipaddress
import os
import re
import shutil
import subprocess
import sys
import tempfile

ALLOWLIST_FILE = ".private-data-allowlist"
# A line carrying this marker is exempt from the address and domain checks, for
# a test fixture or a comment that needs a public value on purpose. The marker
# shows in every diff, so a reviewer sees each exemption. Names are never exempt.
ALLOW_MARKER = "private-data-scan:allow"
IMAGE_SUFFIXES = (".png", ".jpg", ".jpeg", ".webp", ".bmp", ".tif", ".tiff")
RECORDING_SUFFIXES = (".gif", ".mp4", ".webm", ".mov")
PDF_SUFFIXES = (".pdf",)

# Generated from upstream metadata, so full of package authors' domains. The
# name and address checks still run over them; only the domain check is skipped.
GENERATED_FILES = ("package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "go.work.sum", "THIRD_PARTY_NOTICES.md")

# Top-level domains under which real infrastructure turns up. The generic ones
# count on their own, with a single label in front. A country code or a
# word-like TLD (md, sh, py, app, info) doubles as a file suffix or an
# identifier, so it only counts with two labels in front of it, which is the
# shape a real host has. An apex under a country code is therefore left to the
# name pattern.
TLDS_ANY = ("com", "net", "org", "io", "dev", "xyz", "eu", "de", "biz")
COUNTRY_CODES = (
    "ac ad ae af ag ai al am ao aq ar as at au aw ax az ba bb bd be bf bg bh bi bj bm bn bo br bs bt bw by bz "
    "ca cc cd cf cg ch ci ck cl cm cn co cr cu cv cw cx cy cz dj dk dm do dz ec ee eg er es et fi fj fk fm fo fr "
    "ga gd ge gf gg gh gi gl gm gn gp gq gr gs gt gu gw gy hk hm hn hr ht hu id ie il im in iq ir is it je jm jo "
    "jp ke kg kh ki km kn kp kr kw ky kz la lb lc li lk lr ls lt lu lv ly ma mc md me mg mh mk ml mm mn mo mp mq "
    "mr ms mt mu mv mw mx my mz na nc ne nf ng ni nl no np nr nu nz om pa pe pf pg ph pk pl pm pn pr ps pt pw py "
    "qa re ro rs ru rw sa sb sc sd se sg sh si sk sl sm sn so sr ss st sv sx sy sz tc td tf tg th tj tk tl tm tn "
    "to tr tt tv tw tz ua ug uk us uy uz va vc ve vg vi vn vu wf ws ye yt za zm zw").split()
TLDS_WITH_TWO_LABELS = tuple(COUNTRY_CODES) + (
    "run", "app", "email", "pro", "info", "cloud", "tools", "tech", "online", "site", "ai",
    "consulting", "digital", "services", "systems")

LABEL = r"[a-z0-9](?:[a-z0-9\-]{0,61}[a-z0-9])?"
DOMAIN_RE = re.compile(
    r"(?<![\w.\-])("
    r"(?:" + LABEL + r"\.)+(?:" + "|".join(TLDS_ANY) + r")"
    r"|(?:" + LABEL + r"\.){2,}(?:" + "|".join(TLDS_WITH_TWO_LABELS) + r")"
    r")(?![\w\-]|\.[a-z])", re.IGNORECASE)
IPV4_RE = re.compile(r"(?<![\w.:])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])")
# Hex groups with at least two colons, optionally ending in a dotted IPv4 tail
# (2001:db8::192.0.2.1). ipaddress decides what is really an address.
IPV6_RE = re.compile(r"(?<![\w:.])(?=[0-9a-fA-F:]*:[0-9a-fA-F:]*:)[0-9a-fA-F]{0,4}(?::[0-9a-fA-F]{0,4}){2,7}(?:(?:\d{1,3}\.){3}\d{1,3})?(?![\w:.])")


class ScanError(Exception):
    pass


def run(args, cwd=None, binary=False, check=True):
    p = subprocess.run(args, cwd=cwd, capture_output=True, text=not binary)
    if check and p.returncode != 0:
        raise ScanError(f"{' '.join(args)}: {p.stderr.strip() if not binary else p.stderr.decode(errors='replace').strip()}")
    return p.stdout


def git(*args, binary=False, check=True):
    return run(["git", *args], binary=binary, check=check)


class Allowlist:
    def __init__(self, text):
        self.domains = []
        self.networks = []
        for raw in text.splitlines():
            line = raw.split("#", 1)[0].strip().lower()
            if not line:
                continue
            try:
                self.networks.append(ipaddress.ip_network(line, strict=False))
                continue
            except ValueError:
                pass
            self.domains.append(line.lstrip("."))

    @classmethod
    def load(cls, path):
        if not os.path.exists(path):
            return cls("")
        with open(path, encoding="utf-8") as f:
            return cls(f.read())

    def allows_domain(self, host):
        host = host.lower()
        return any(host == d or host.endswith("." + d) for d in self.domains)

    def allows_address(self, addr):
        return any(addr in n for n in self.networks)


class Finding:
    def __init__(self, kind, where, value, note=""):
        self.kind = kind
        self.where = where
        self.value = value
        self.note = note

    def render(self, redact):
        shown = "(redacted)" if redact else self.value
        note = f"  {self.note}" if self.note and not redact else ""
        return f"{self.kind:8s} {self.where}  {shown}{note}"


class Scanner:
    def __init__(self, allowlist, name_re, redact):
        self.allowlist = allowlist
        self.name_re = name_re
        self.redact = redact
        self.findings = []
        self.unscanned = []
        self.seen_blobs = set()

    # --- text checks ------------------------------------------------------

    def check_text(self, text, where_of, domains=True):
        for lineno, line in enumerate(text.splitlines(), 1):
            where = where_of(lineno)
            if self.name_re:
                for m in self.name_re.finditer(line):
                    self.findings.append(Finding("name", where, m.group(0)))
            if ALLOW_MARKER in line:
                continue
            for m in IPV4_RE.finditer(line):
                self._check_address(m.group(0), where)
            for m in IPV6_RE.finditer(line):
                self._check_address(m.group(0), where)
            for m in DOMAIN_RE.finditer(line) if domains else ():
                host = m.group(1)
                # DNS is case-insensitive, so an all-caps host counts. Mixed case
                # (ci.Status.Transition.To) is an identifier chain, never a host.
                if host != host.lower() and host != host.upper():
                    continue
                if not self.allowlist.allows_domain(host):
                    self.findings.append(Finding("domain", where, host, "not on the allow-list"))

    def _check_address(self, token, where):
        try:
            addr = ipaddress.ip_address(token)
        except ValueError:
            return
        if (not addr.is_global or addr.is_multicast or addr.is_reserved or addr.is_private
                or addr.is_loopback or addr.is_link_local or addr.is_unspecified
                or self.allowlist.allows_address(addr)):
            return
        self.findings.append(Finding("address", where, token, "public address"))

    # --- blobs ------------------------------------------------------------

    def check_blob(self, sha, path):
        if sha in self.seen_blobs:
            return
        self.seen_blobs.add(sha)
        data = git("cat-file", "-p", sha, binary=True)
        if b"\0" not in data[:8192]:
            generated = os.path.basename(path) in GENERATED_FILES
            self.check_text(data.decode("utf-8", errors="replace"), lambda n: f"{path}:{n}", domains=not generated)
            return
        lower = path.lower()
        if lower.endswith(IMAGE_SUFFIXES + RECORDING_SUFFIXES + PDF_SUFFIXES):
            text = ocr(data, lower)
            if text is None:
                self.unscanned.append(path)
                return
            self.check_text(text, lambda n: f"{path} (frame or page {n})")
            return
        self.unscanned.append(path)

    def check_path(self, path):
        self.check_text(path, lambda n: path)
        for f in self.findings:
            if f.where == path and f.kind != "path":
                f.kind = "path"

    def check_commit(self, sha):
        where = f"commit {sha[:10]}"
        message = git("log", "-1", "--format=%B", sha)
        self.check_text(message, lambda n: where)
        # An author's mail provider is their own public choice, so identities
        # get the name and address checks but not the domain check.
        identities = git("log", "-1", "--format=%an <%ae>%n%cn <%ce>", sha)
        self.check_text(identities, lambda n: where, domains=False)
        for f in self.findings:
            if f.where == where:
                f.kind = "message"


# --- OCR --------------------------------------------------------------------

def ocr(data, lower):
    """Return the text visible in an image, recording or PDF, or None when the tools are missing."""
    tesseract = shutil.which("tesseract")
    if not tesseract:
        return None
    with tempfile.TemporaryDirectory(prefix="private-data-scan-") as tmp:
        src = os.path.join(tmp, "asset" + os.path.splitext(lower)[1])
        with open(src, "wb") as f:
            f.write(data)
        frames = []
        if lower.endswith(RECORDING_SUFFIXES):
            ffmpeg = shutil.which("ffmpeg")
            if not ffmpeg:
                return None
            run([ffmpeg, "-v", "error", "-i", src, "-vf", "fps=1", os.path.join(tmp, "f%05d.png")])
            frames = sorted(os.path.join(tmp, n) for n in os.listdir(tmp) if n.startswith("f") and n.endswith(".png"))
        elif lower.endswith(PDF_SUFFIXES):
            pdftoppm = shutil.which("pdftoppm")
            if not pdftoppm:
                return None
            run([pdftoppm, "-r", "120", "-png", src, os.path.join(tmp, "p")])
            frames = sorted(os.path.join(tmp, n) for n in os.listdir(tmp) if n.startswith("p") and n.endswith(".png"))
        else:
            frames = [src]
        pages = []
        for frame in frames:
            p = subprocess.run([tesseract, frame, "-", "--psm", "6"], capture_output=True, text=True)
            if p.returncode != 0:
                raise ScanError(f"tesseract could not read {lower}: {p.stderr.strip()[:200]}")
            pages.append(" ".join(p.stdout.split()))
        return "\n".join(pages)


# --- what to scan ------------------------------------------------------------

def commits_in_range(old, new):
    return git("rev-list", "--reverse", f"{old}..{new}").split()


def commits_new_to_remote(new, remote):
    """Commits reachable from new that no ref of the remote has, ignoring a ref that already sits at new itself (a force push)."""
    tip = git("rev-parse", new).strip()
    refs = [line.split()[1] for line in git("for-each-ref", "--format=%(objectname) %(refname)", f"refs/remotes/{remote}/").splitlines()
            if line.split()[0] != tip]
    return git("rev-list", "--reverse", new, "--not", *refs).split()


def changed_blobs(commit):
    """(path, blob) for every file a commit adds or modifies, against each parent."""
    out = git("diff-tree", "-r", "-m", "--root", "--no-commit-id", "-z", commit)
    items = out.split("\0")
    result = []
    i = 0
    while i < len(items) and items[i]:
        meta = items[i]
        parts = meta.lstrip(":").split(" ")
        status = parts[4][0]
        if status in ("R", "C"):
            path = items[i + 2]
            i += 3
        else:
            path = items[i + 1]
            i += 2
        if status != "D":
            result.append((path, parts[3]))
    return result


def tree_blobs(rev):
    out = git("ls-tree", "-r", "-z", rev)
    result = []
    for entry in out.split("\0"):
        if not entry:
            continue
        meta, path = entry.split("\t", 1)
        mode, kind, sha = meta.split(" ")
        if kind == "blob":
            result.append((path, sha))
    return result


# --- main -------------------------------------------------------------------

def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    what = p.add_mutually_exclusive_group(required=True)
    what.add_argument("--range", metavar="OLD..NEW", help="scan the commits in OLD..NEW")
    what.add_argument("--new", metavar="SHA", help="scan the commits reachable from SHA that no ref of --remote has")
    what.add_argument("--tree", metavar="REV", help="scan every file in the tree at REV")
    p.add_argument("--remote", default="origin", help="remote used with --new (default origin)")
    p.add_argument("--redact", action="store_true", help="report locations only, for logs that are public")
    p.add_argument("--require-pattern", action="store_true", help="fail when PRIVATE_NAME_PATTERN is unset")
    p.add_argument("--allowlist", default=None, help=f"allow-list path (default {ALLOWLIST_FILE} at the repository root)")
    return p.parse_args(argv)


def main(argv):
    args = parse_args(argv)
    try:
        top = git("rev-parse", "--show-toplevel").strip()
    except ScanError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    os.chdir(top)

    pattern = os.environ.get("PRIVATE_NAME_PATTERN", "")
    name_re = None
    if pattern:
        try:
            name_re = re.compile(pattern, re.IGNORECASE)
        except re.error as e:
            print(f"error: PRIVATE_NAME_PATTERN is not a valid pattern: {e}", file=sys.stderr)
            return 2
    elif args.require_pattern:
        print("error: PRIVATE_NAME_PATTERN is unset, so the name check cannot run.", file=sys.stderr)
        return 2
    else:
        print("note: PRIVATE_NAME_PATTERN is unset, so only the generic checks (addresses, domains) run.")

    allowlist_path = args.allowlist or os.path.join(top, ALLOWLIST_FILE)
    allowlist = Allowlist.load(allowlist_path)
    allowlist_text = ""
    if os.path.exists(allowlist_path):
        with open(allowlist_path, encoding="utf-8") as f:
            allowlist_text = f.read()

    scanner = Scanner(allowlist, name_re, args.redact)
    commits = []
    try:
        if args.tree:
            for path, sha in tree_blobs(args.tree):
                scanner.check_path(path)
                scanner.check_blob(sha, path)
        else:
            if args.range:
                old, new = args.range.split("..", 1)
                commits = commits_in_range(old, new)
            else:
                commits = commits_new_to_remote(args.new, args.remote)
            for c in commits:
                scanner.check_commit(c)
                for path, sha in changed_blobs(c):
                    scanner.check_path(path)
                    scanner.check_blob(sha, path)
    except ScanError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    scope = f"tree {args.tree}" if args.tree else f"{len(commits)} commit(s)"
    for path in scanner.unscanned:
        print(f"note: {path} is binary and was not scanned; check it by eye.")
    if scanner.findings:
        print(f"Private data found in {scope}:")
        for f in scanner.findings:
            print("  " + f.render(args.redact))
        print("")
        print("Replace real addresses with RFC 5737 / 3849 values, real domains with example.*,")
        print(f"and client names with placeholders. Known public values go in {ALLOWLIST_FILE}.")
        return 1
    print(f"Clean: no private data in {scope}.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
