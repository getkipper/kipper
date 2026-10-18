package installer

import (
	"errors"
	"fmt"
	"strings"
)

const (
	sshdKeyOnlyDropIn = "/etc/ssh/sshd_config.d/00-kipper-key-only.conf"
	sshdPreviousCopy  = "/var/lib/kipper/sshd-key-only.previous"

	sshdProbeMarker   = "# kipper-sshd-probe"
	sshdApplyMarker   = "# kipper-sshd-apply"
	sshdRestoreMarker = "# kipper-sshd-restore"
	sshdCommitMarker  = "# kipper-sshd-commit"
)

// keyOnlyConfirmer checks the running SSH server through new connections
// for the configured user.
type keyOnlyConfirmer interface {
	// ConfirmFreshKeyOnlyConnection succeeds when a public key alone logs in.
	ConfirmFreshKeyOnlyConnection() error
	// ConfirmPasswordRefused succeeds only when the server rejects the login
	// without offering password or keyboard-interactive authentication.
	ConfirmPasswordRefused() error
}

type sshdProbe struct {
	includesDropIns bool
	otherIncludes   []string
	managedExtras   []string
	overrides       []string
	passwordAuth    string
	kbdAuth         string
	rootLogin       string
}

// sshdProbeScript emits tab-separated records for drop-in inclusion before
// Match blocks, unsupported Includes, password-related Match settings and
// unmanaged settings in kip's drop-in. Effective global values follow in
// sshd -T format. The scanner handles quoted keywords and whitespace or "="
// separators. It follows top-level Includes one level deep to cover files
// such as the crypto-policies file on RHEL-family systems.
const sshdProbeScript = sshdProbeMarker + `
c=/etc/ssh/sshd_config
d=/etc/ssh/sshd_config.d
inc=$(grep -nEi '^[[:space:]]*"?Include"?[[:space:]=]+"?(/etc/ssh/)?sshd_config\.d/\*\.conf"?[[:space:]]*$' "$c" | head -1 | cut -d: -f1)
match=$(grep -nEi '^[[:space:]]*"?Match"?[[:space:]=]' "$c" | head -1 | cut -d: -f1)
if [ -n "$inc" ] && { [ -z "$match" ] || [ "$inc" -lt "$match" ]; }; then printf 'include\tok\n'; else printf 'include\tmissing\n'; fi
scan() {
  awk -v f="$1" -v depth="$2" -v main="$c" -v allowed="$inc" -v managed="` + sshdKeyOnlyDropIn + `" '
    { line = $0; sub(/^[ \t]+/, "", line); if (line == "" || substr(line, 1, 1) == "#") next
      split(line, w, /[ \t=]+/); k = w[1]; gsub(/"/, "", k); k = tolower(k)
      rest = substr(line, length(w[1]) + 1); sub(/^[ \t=]+/, "", rest) }
    f == managed && k !~ /^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin)$/ { print "managed-extra\t" f ":" NR "\t" k; next }
    k == "match" { m = 1; next }
    k == "include" {
      if (f == main && NR == allowed) next
      if (m || depth > 0 || rest ~ /["\047\\]/) print "include-other\t" f ":" NR "\tInclude " rest; else print "follow\t" rest
      next
    }
    m && k ~ /^(passwordauthentication|kbdinteractiveauthentication|challengeresponseauthentication|authenticationmethods|permitemptypasswords)$/ { print "override\t" f ":" NR "\t" k "\t" rest }
  ' "$1"
}
top=$(for f in "$c" "$d"/*.conf; do [ -f "$f" ] && scan "$f" 0; done)
printf '%s\n' "$top" | grep -v '^follow' || true
printf '%s\n' "$top" | grep '^follow' | cut -f2- | while IFS= read -r paths; do
  (cd /etc/ssh && for p in $paths; do case "$p" in /*) ;; *) p="./$p" ;; esac; [ -f "$p" ] && scan "$p" 1; done)
done
sshd -T 2>/dev/null | grep -Ei '^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin) '
`

// parseSSHDProbe reads sshdProbeScript's output. Only Match-block settings
// that can allow a password login are kept as overrides.
func parseSSHDProbe(out string) (sshdProbe, error) {
	var p sshdProbe
	for _, line := range strings.Split(out, "\n") {
		if record := strings.Split(line, "\t"); len(record) >= 2 {
			switch record[0] {
			case "include":
				p.includesDropIns = record[1] == "ok"
			case "include-other":
				p.otherIncludes = append(p.otherIncludes, strings.Join(record[1:], " "))
			case "managed-extra":
				p.managedExtras = append(p.managedExtras, strings.Join(record[1:], " "))
			case "override":
				if len(record) >= 4 && permitsPasswordLogin(strings.ToLower(record[2]), strings.Fields(record[3])) {
					p.overrides = append(p.overrides, strings.Join(record[1:], " "))
				}
			}
			continue
		}
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "passwordauthentication":
			p.passwordAuth = fields[1]
		case "kbdinteractiveauthentication":
			p.kbdAuth = fields[1]
		case "permitrootlogin":
			p.rootLogin = fields[1]
		}
	}
	if p.passwordAuth == "" || p.kbdAuth == "" || p.rootLogin == "" {
		return sshdProbe{}, errors.New("could not read sshd's effective settings with sshd -T")
	}
	return p, nil
}

func permitsPasswordLogin(setting string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	values := make([]string, len(args))
	for i, a := range args {
		values[i] = strings.ToLower(strings.Trim(a, `"'`))
	}
	switch setting {
	case "passwordauthentication", "kbdinteractiveauthentication", "challengeresponseauthentication", "permitemptypasswords":
		return values[0] == "yes"
	case "authenticationmethods":
		for _, list := range values {
			if list == "any" || strings.Contains(list, "password") || strings.Contains(list, "keyboard-interactive") {
				return true
			}
		}
	}
	return false
}

// rootLoginPolicy tightens "yes" to "prohibit-password", normalises its
// "without-password" alias and preserves stricter values. The drop-in must
// specify the policy because it may replace the file that set it.
func rootLoginPolicy(current string) string {
	switch current {
	case "yes", "without-password", "prohibit-password":
		return "prohibit-password"
	}
	return current
}

func keyOnlyDropIn(rootLogin string) string {
	return "# Managed by kip: SSH logins use keys only.\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n" +
		"PermitRootLogin " + rootLoginPolicy(rootLogin) + "\n"
}

// sshdReload reloads the active ssh or sshd service, if either is running.
const sshdReload = `if systemctl is-active --quiet ssh; then systemctl reload ssh; elif systemctl is-active --quiet sshd; then systemctl reload sshd; fi`

// sshdRestore puts back the drop-in as it was before the change, or removes it
// when there was none, and reloads sshd. It keeps the saved copy until both
// steps have succeeded.
const sshdRestore = sshdRestoreMarker + `
set -e
d=` + sshdKeyOnlyDropIn + `
b=` + sshdPreviousCopy + `
if [ -e "$b.absent" ]; then rm -f "$d"; elif [ -e "$b" ]; then cp -p "$b" "$d"; else exit 0; fi
sshd -t
` + sshdReload + `
rm -f "$b" "$b.absent"
`

// sshdApply saves the current drop-in, writes the new one from stdin,
// validates the configuration and reloads sshd. EnsureKeyOnlySSH attempts to
// restore the saved copy if applying or verifying the change fails.
const sshdApply = sshdApplyMarker + `
set -e
d=` + sshdKeyOnlyDropIn + `
b=` + sshdPreviousCopy + `
mkdir -p /var/lib/kipper /etc/ssh/sshd_config.d
rm -f "$b" "$b.absent"
if [ -e "$d" ]; then cp -p "$d" "$b"; else touch "$b.absent"; fi
staged=$(mktemp /etc/ssh/sshd_config.d/.kipper-XXXXXX)
trap 'rm -f "$staged"' EXIT
cat > "$staged"
chmod 0644 "$staged"
mv "$staged" "$d"
trap - EXIT
sshd -t
` + sshdReload + `
`

const sshdCommit = sshdCommitMarker + `
rm -f ` + sshdPreviousCopy + ` ` + sshdPreviousCopy + `.absent
`

const sshdVerify = `echo verify; sshd -T 2>/dev/null | grep -Ei '^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin) '`

// EnsureKeyOnlySSH disables password and keyboard-interactive SSH login with
// a managed drop-in and reloads sshd. The client must support fresh SSH checks.
// Unsupported Includes, permissive Match settings and unmanaged drop-in settings
// cause an error before any change. A fresh key-only login is required before
// writing, and stricter root-login policies are preserved.
//
// A true result means the change passed configuration and fresh-login checks.
// A false result with no error means the configuration already disables both
// login methods and the running server refuses them for the configured user.
// That path leaves the configuration untouched and skips the key-login check
// and reload, so other users may still be subject to older settings.
//
// Apply or verification failures trigger an immediate restore attempt rather
// than a timed rollback. Errors include any restore failure. Backup cleanup
// failures are logged without failing a verified change.
func EnsureKeyOnlySSH(client CommandRunner) (bool, error) {
	confirmer, ok := client.(keyOnlyConfirmer)
	if !ok {
		return false, errors.New("key-only SSH requires verification over new SSH connections. Run kip cluster harden from your workstation")
	}
	out, err := client.Run(sshdProbeScript)
	if err != nil {
		return false, fmt.Errorf("reading the SSH server configuration: %w", err)
	}
	probe, err := parseSSHDProbe(out)
	if err != nil {
		return false, err
	}
	if !probe.includesDropIns {
		return false, errors.New("/etc/ssh/sshd_config does not include /etc/ssh/sshd_config.d/*.conf before its first Match block, so kip cannot set key-only login safely. Add that Include line at the top, or turn PasswordAuthentication and KbdInteractiveAuthentication off yourself")
	}
	if len(probe.otherIncludes) > 0 {
		return false, fmt.Errorf("the SSH server configuration includes files kip does not check (%s). Move their settings into /etc/ssh/sshd_config.d, then run this again", strings.Join(probe.otherIncludes, "; "))
	}
	if len(probe.overrides) > 0 {
		return false, fmt.Errorf("a Match block can still allow password login (%s). Remove those settings, then run this again", strings.Join(probe.overrides, "; "))
	}
	if len(probe.managedExtras) > 0 {
		return false, fmt.Errorf("%s contains settings outside kip's control (%s). Replacing the file would remove them. Move them to another file in /etc/ssh/sshd_config.d, then run this again", sshdKeyOnlyDropIn, strings.Join(probe.managedExtras, "; "))
	}
	if probe.passwordAuth == "no" && probe.kbdAuth == "no" {
		// Check the running server because configuration edits take effect on
		// reload. This probe covers only kip's login, so other users may still
		// be subject to older settings.
		if err := confirmer.ConfirmPasswordRefused(); err != nil {
			return false, fmt.Errorf("sshd's configuration disables password login, but verification against the running server failed. Reload sshd, then run this again: %w", err)
		}
		return false, nil
	}
	if err := confirmer.ConfirmFreshKeyOnlyConnection(); err != nil {
		return false, fmt.Errorf("kip could not log in with a key alone, so turning passwords off could lock you out: %w", err)
	}

	wantRoot := rootLoginPolicy(probe.rootLogin)
	if _, err := client.RunStdin(sshdApply, strings.NewReader(keyOnlyDropIn(probe.rootLogin))); err != nil {
		return false, restoreSSHD(client, fmt.Errorf("setting key-only SSH login: %w", err))
	}
	if err := verifyKeyOnlySSH(client, confirmer, wantRoot); err != nil {
		return false, restoreSSHD(client, err)
	}
	if _, err := client.Run(sshdCommit); err != nil {
		// Keep the verified change even if backup cleanup fails.
		fmt.Printf("       ⚠  Key-only SSH works, but removing %s failed: %v\n", sshdPreviousCopy, err)
	}
	return true, nil
}

func restoreSSHD(client CommandRunner, cause error) error {
	if _, err := client.Run(sshdRestore); err != nil {
		return fmt.Errorf("%w. Restoring the previous SSH configuration also failed (%w). Check the saved state at %s", cause, err, sshdPreviousCopy)
	}
	return fmt.Errorf("%w. The previous SSH configuration has been restored", cause)
}

func verifyKeyOnlySSH(client CommandRunner, confirmer keyOnlyConfirmer, wantRoot string) error {
	out, err := client.Run(sshdVerify)
	if err != nil {
		return fmt.Errorf("reading sshd's effective settings: %w", err)
	}
	want := map[string]string{"passwordauthentication": "no", "kbdinteractiveauthentication": "no", "permitrootlogin": wantRoot}
	for _, line := range strings.Split(strings.ToLower(out), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			if w, ok := want[fields[0]]; ok {
				got := fields[1]
				if got == "without-password" {
					got = "prohibit-password"
				}
				if got != w {
					return fmt.Errorf("sshd reports %s %s after the change", fields[0], fields[1])
				}
				delete(want, fields[0])
			}
		}
	}
	if len(want) > 0 {
		return errors.New("could not confirm sshd's settings after the change")
	}
	if err := confirmer.ConfirmFreshKeyOnlyConnection(); err != nil {
		return fmt.Errorf("a new key-only SSH login failed after the change: %w", err)
	}
	if err := confirmer.ConfirmPasswordRefused(); err != nil {
		return fmt.Errorf("could not confirm that the running SSH server refuses password login: %w", err)
	}
	return nil
}
