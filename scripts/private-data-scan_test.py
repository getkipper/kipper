"""Behavioural tests for scripts/private-data-scan.py.

Every test builds a throwaway git repository, plants one kind of private data,
and runs the scanner as a subprocess the way the hook and the workflow do. The
scanner's exit code and output are the contract under test, not its internals.

Run with:  python3 scripts/private-data-scan_test.py
"""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCANNER = os.path.join(HERE, "private-data-scan.py")

ALLOWLIST = """\
# test allow-list
example.com
kipper.run
1.1.1.1
198.18.0.0/15
"""


def git(repo, *args, env=None):
    e = dict(os.environ)
    e.update({
        "GIT_AUTHOR_NAME": "Test Author", "GIT_AUTHOR_EMAIL": "author@example.com",
        "GIT_COMMITTER_NAME": "Test Author", "GIT_COMMITTER_EMAIL": "author@example.com",
        "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
    })
    if env:
        e.update(env)
    return subprocess.run(["git", "-C", repo, *args], check=True, capture_output=True, text=True, env=e).stdout.strip()


class Repo:
    def __init__(self):
        self.dir = tempfile.mkdtemp(prefix="pds-")
        git(self.dir, "init", "-q", "-b", "main")
        git(self.dir, "commit", "-q", "--allow-empty", "-m", "root")
        self.write(".private-data-allowlist", ALLOWLIST)
        self.commit("allow-list")

    def write(self, path, content, binary=False):
        full = os.path.join(self.dir, path)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "wb" if binary else "w") as f:
            f.write(content)
        return full

    def commit(self, message, paths=None):
        git(self.dir, "add", "-A")
        git(self.dir, "commit", "-q", "-m", message)
        return git(self.dir, "rev-parse", "HEAD")

    def head(self):
        return git(self.dir, "rev-parse", "HEAD")

    def cleanup(self):
        shutil.rmtree(self.dir, ignore_errors=True)


def scan(repo, *args, env=None):
    e = dict(os.environ)
    e.pop("PRIVATE_NAME_PATTERN", None)
    if env:
        e.update(env)
    p = subprocess.run([sys.executable, SCANNER, *args], cwd=repo.dir, capture_output=True, text=True, env=e)
    return p.returncode, p.stdout + p.stderr


class ScannerTest(unittest.TestCase):
    def setUp(self):
        self.repo = Repo()
        self.base = self.repo.head()
        self.addCleanup(self.repo.cleanup)

    def plant(self, path, content, message="change", binary=False):
        self.repo.write(path, content, binary=binary)
        return self.repo.commit(message)

    def assertFinding(self, out, kind, needle=None):
        self.assertIn(kind, out, out)
        if needle:
            self.assertIn(needle, out, out)

    # --- addresses -------------------------------------------------------

    def test_public_ipv4_fails(self):
        self.plant("notes.md", "the box is at 93.184.216.34 today\n")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "address", "notes.md:1")

    def test_documentation_private_and_loopback_ipv4_pass(self):
        self.plant("a.go", 'x := []string{"192.0.2.10", "198.51.100.7", "203.0.113.10", "10.42.0.1", "127.0.0.1", "169.254.1.1", "0.0.0.0", "224.0.0.1"}\n')
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)

    def test_allowlisted_ipv4_and_cidr_pass(self):
        self.plant("dns.yaml", "resolvers: [1.1.1.1, 198.18.5.5]\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)

    def test_public_ipv6_fails_and_documentation_ipv6_passes(self):
        self.plant("v6.go", 'ok := "2001:db8::1"; alsoOk := "fe80::1"; bad := "2001:4860:4860::8888"\n')  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "address", "2001:4860:4860::8888")  # private-data-scan:allow
        self.assertNotIn("2001:db8::1", out)

    def test_ipv4_embedded_ipv6_fails(self):
        self.plant("v6.go", 'bad := "2001:4860::192.0.2.1"\n')  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "address", "2001:4860::192.0.2.1")  # private-data-scan:allow

    def test_times_macs_and_digests_are_not_addresses(self):
        self.plant("misc.txt", "at 12:30:45 mac aa:bb:cc:dd:ee:ff sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)

    # --- domains ---------------------------------------------------------

    def test_unknown_domain_fails(self):
        self.plant("docs/x.md", "log in at console.internal-corp.de first\n")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "domain", "console.internal-corp.de")  # private-data-scan:allow

    def test_allowlisted_domain_and_subdomains_pass(self):
        self.plant("docs/y.md", "see app.example.com and demo.kipper.run and console--demo.kipper.run\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)

    def test_uppercase_hostname_and_country_code_hosts_fail(self):
        self.plant("z.md", "reach SECRET.CORP.COM or server.client.fr\n")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "domain", "SECRET.CORP.COM")  # private-data-scan:allow
        self.assertFinding(out, "domain", "server.client.fr")  # private-data-scan:allow

    def test_marker_exempts_a_line_from_the_generic_checks_only(self):
        self.plant("f.go", 'peer := "93.184.216.34" // private-data-scan:allow\n')  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)
        self.plant("g.go", 'ns := "acmecorp" // private-data-scan:allow\n')
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\bacmecorp\b"})
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "name", "g.go")

    def test_code_identifiers_are_not_domains(self):
        self.plant("z.go", "v := ci.Status.Transition.To; w := cfg.route.host; u := foo.bar.baz; x := Foo.Bar.Com; y := main.py; z := args.ai\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)

    # --- names -----------------------------------------------------------

    def test_name_pattern_from_environment_fails(self):
        self.plant("svc.go", "ns := \"acmecorp-test\"\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\bacmecorp\b|\bothername\b"})
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "name", "svc.go:1")

    def test_name_pattern_honours_word_boundaries(self):
        self.plant("svc.go", "func TestAppCannotBeCreated() {}\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\bappcann\b"})
        self.assertEqual(rc, 0, out)

    def test_missing_pattern_is_reported_and_generic_checks_still_run(self):
        self.plant("svc.go", "host := \"93.184.216.34\"\n")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertIn("PRIVATE_NAME_PATTERN", out)

    def test_require_pattern_fails_when_unset(self):
        self.plant("ok.md", "nothing here\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", "--require-pattern")
        self.assertEqual(rc, 2, out)

    def test_malformed_pattern_is_a_scan_error(self):
        self.plant("ok.md", "nothing here\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\b(unclosed"})
        self.assertEqual(rc, 2, out)

    # --- where private data hides ---------------------------------------

    def test_path_name_fails(self):
        self.plant("fixtures/acmecorp/config.yaml", "a: 1\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\bacmecorp\b"})
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "path", "fixtures/acmecorp/config.yaml")

    def test_commit_message_fails(self):
        self.plant("ok.md", "clean\n", message="fix the acmecorp cluster at 93.184.216.34")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\bacmecorp\b"})
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "message")

    def test_author_mail_provider_is_not_a_domain_finding(self):
        self.repo.write("ok.md", "clean\n")
        git(self.repo.dir, "add", "-A")
        git(self.repo.dir, "commit", "-q", "-m", "contribution",
            env={"GIT_AUTHOR_NAME": "Outside Contributor", "GIT_AUTHOR_EMAIL": "someone@gmail.com"})  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)
        git(self.repo.dir, "commit", "-q", "--allow-empty", "-m", "see console.internal-corp.de")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "message")

    def test_value_introduced_then_removed_inside_the_range_fails(self):
        self.plant("f.go", 'ns := "acmecorp-test"\n', message="add fixture")
        self.plant("f.go", 'ns := "payroll-test"\n', message="rename fixture")
        rc, out = scan(self.repo, "--tree", "HEAD", env={"PRIVATE_NAME_PATTERN": r"\bacmecorp\b"})
        self.assertEqual(rc, 0, out)
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", env={"PRIVATE_NAME_PATTERN": r"\bacmecorp\b"})
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "name", "f.go")

    def test_new_branch_scans_only_commits_not_on_the_remote(self):
        remote = tempfile.mkdtemp(prefix="pds-remote-")
        self.addCleanup(shutil.rmtree, remote, True)
        git(remote, "init", "-q", "--bare")
        git(self.repo.dir, "remote", "add", "origin", remote)
        self.plant("old.md", "already on the remote: 93.184.216.34\n", message="old")  # private-data-scan:allow
        git(self.repo.dir, "push", "-q", "origin", "main")
        git(self.repo.dir, "checkout", "-q", "-b", "feature")
        self.plant("new.md", "clean\n", message="new")
        rc, out = scan(self.repo, "--new", "HEAD", "--remote", "origin")
        self.assertEqual(rc, 0, out)
        self.plant("new2.md", "leak 93.184.216.34\n", message="new2")  # private-data-scan:allow
        rc, out = scan(self.repo, "--new", "HEAD", "--remote", "origin")
        self.assertEqual(rc, 1, out)
        self.assertNotIn("old.md", out)

    def test_force_pushed_branch_is_still_scanned(self):
        remote = tempfile.mkdtemp(prefix="pds-remote-")
        self.addCleanup(shutil.rmtree, remote, True)
        git(remote, "init", "-q", "--bare")
        git(self.repo.dir, "remote", "add", "origin", remote)
        git(self.repo.dir, "push", "-q", "origin", "main")
        git(self.repo.dir, "checkout", "-q", "-b", "feature")
        self.plant("new.md", "leak 93.184.216.34\n", message="new")  # private-data-scan:allow
        git(self.repo.dir, "push", "-q", "origin", "feature")
        rc, out = scan(self.repo, "--new", "HEAD", "--remote", "origin")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "address", "new.md")

    def test_range_skips_commits_the_remote_already_has(self):
        remote = tempfile.mkdtemp(prefix="pds-remote-")
        self.addCleanup(shutil.rmtree, remote, True)
        git(remote, "init", "-q", "--bare")
        git(self.repo.dir, "remote", "add", "origin", remote)
        git(self.repo.dir, "push", "-q", "origin", "main")
        git(self.repo.dir, "checkout", "-q", "-b", "develop")
        git(self.repo.dir, "push", "-q", "origin", "develop")
        git(self.repo.dir, "checkout", "-q", "main")
        self.plant("old.md", "already public on main: 93.184.216.34\n", message="on main")  # private-data-scan:allow
        git(self.repo.dir, "push", "-q", "origin", "main")
        git(self.repo.dir, "checkout", "-q", "develop")
        git(self.repo.dir, "merge", "-q", "main")
        self.plant("new.md", "clean\n", message="rebuilt develop")
        rc, out = scan(self.repo, "--range", "origin/develop..HEAD")
        self.assertEqual(rc, 0, out)
        self.plant("new2.md", "leak 93.184.216.34\n", message="really new")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", "origin/develop..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertNotIn("old.md", out)

    def test_tree_scan_covers_the_whole_tree(self):
        self.plant("deep/old.md", "leak 93.184.216.34\n", message="old")  # private-data-scan:allow
        base2 = self.repo.head()
        self.plant("new.md", "clean\n", message="new")
        rc, out = scan(self.repo, "--range", f"{base2}..HEAD")
        self.assertEqual(rc, 0, out)
        rc, out = scan(self.repo, "--tree", "HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "address", "deep/old.md")

    def test_redact_hides_values_but_keeps_locations(self):
        self.plant("notes.md", "the box is at 93.184.216.34\n")  # private-data-scan:allow
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD", "--redact")
        self.assertEqual(rc, 1, out)
        self.assertIn("notes.md:1", out)
        self.assertNotIn("93.184.216.34", out)  # private-data-scan:allow

    def test_clean_range_exits_zero_and_says_so(self):
        self.plant("ok.md", "nothing to see, 203.0.113.10 and app.example.com\n")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)
        self.assertIn("clean", out.lower())

    # --- binaries --------------------------------------------------------

    def test_unscannable_binary_is_named(self):
        self.plant("blob.bin", b"\x00\x01\x02binary", binary=True)
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)
        self.assertIn("blob.bin", out)

    @unittest.skipUnless(shutil.which("tesseract") and shutil.which("magick"), "tesseract and ImageMagick needed")
    def test_image_text_is_read_by_ocr(self):
        img = os.path.join(self.repo.dir, "docs", "shot.png")
        os.makedirs(os.path.dirname(img), exist_ok=True)
        subprocess.run(["magick", "-size", "600x80", "xc:white", "-fill", "black", "-pointsize", "28",
                        "-annotate", "+10+50", "Connecting to 93.184.216.34 ...", img], check=True)  # private-data-scan:allow
        self.repo.commit("add screenshot")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 1, out)
        self.assertFinding(out, "address", "docs/shot.png")

    @unittest.skipUnless(shutil.which("tesseract"), "tesseract needed")
    def test_unreadable_image_is_a_scan_error(self):
        self.plant("docs/broken.png", b"\x89PNG\r\n\x1a\n\x00garbage" * 10, binary=True)
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 2, out)
        self.assertIn("broken.png", out)

    @unittest.skipUnless(shutil.which("tesseract") and shutil.which("magick"), "tesseract and ImageMagick needed")
    def test_clean_image_passes(self):
        img = os.path.join(self.repo.dir, "docs", "clean.png")
        os.makedirs(os.path.dirname(img), exist_ok=True)
        subprocess.run(["magick", "-size", "600x80", "xc:white", "-fill", "black", "-pointsize", "28",
                        "-annotate", "+10+50", "Connecting to 203.0.113.10 ...", img], check=True)
        self.repo.commit("add screenshot")
        rc, out = scan(self.repo, "--range", f"{self.base}..HEAD")
        self.assertEqual(rc, 0, out)


if __name__ == "__main__":
    unittest.main(verbosity=1)
