package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/pflag"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var (
	knownDistros   = map[string]bool{"almalinux": true, "centos": true, "debian": true, "rhel": true, "rocky": true, "ubuntu": true}
	numericVersion = regexp.MustCompile(`^\d{1,4}(\.\d{1,4}){0,2}$`)
	sizingProfiles = map[string]bool{platform.ProfileNano: true, platform.ProfileSmall: true, platform.ProfileMedium: true,
		platform.ProfileLarge: true, platform.ProfileXLarge: true}
)

// serverDescription limits os-release details to a known distribution or
// family and a numeric version because custom images can include private data.
func serverDescription(host installer.HostOS) string {
	name := "other"
	switch {
	case knownDistros[host.ID]:
		name = host.ID
	case host.Family != installer.FamilyUnknown:
		name = string(host.Family) + "-like"
	}
	version := "other"
	if numericVersion.MatchString(host.VersionID) {
		version = host.VersionID
	}
	return name + " " + version
}

func plainProfile(profile string) string {
	if sizingProfiles[profile] {
		return profile
	}
	return "custom"
}

// saveIssueReport writes report to a new file in dir named after prefix and
// returns its path, preserving existing files and symlinks. It creates dir
// if needed and uses owner-only permissions where the platform supports them.
func saveIssueReport(dir, prefix, report string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, prefix+"-*.txt")
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(report + "\n")
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(f.Name())
		return "", writeErr
	}
	return f.Name(), nil
}

// printIssueReport shows intro, the issue link and the report to paste into
// it, or prints nothing when link is empty. An empty savedPath omits the
// saved-file message.
func printIssueReport(out io.Writer, intro, link, report, savedPath string) {
	if link == "" {
		return
	}
	_, _ = fmt.Fprintf(out, "  %s\n\n", intro)
	_, _ = fmt.Fprintf(out, "  %s\n\n", link)
	if report == "" {
		return
	}
	_, _ = fmt.Fprintf(out, "  Paste only the text between the lines below into the form's Logs field.\n")
	_, _ = fmt.Fprintf(out, "  kip replaced the addresses, hostnames and credentials it recognized with placeholders. Read the text before pasting it.\n\n")
	_, _ = fmt.Fprintf(out, "  ----\n%s\n  ----\n", indent(report))
	if savedPath != "" {
		_, _ = fmt.Fprintf(out, "  Also saved to %s\n", savedPath)
	}
	_, _ = fmt.Fprintln(out)
}

func indent(text string) string {
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}

// reportFailure prints intro and the issue link, and saves and prints the
// report under a file name starting with prefix in the kip config directory.
func reportFailure(out io.Writer, intro, prefix, link, report string) {
	var savedPath string
	var saveErr error
	if report != "" {
		var dir string
		if dir, saveErr = config.Dir(); saveErr == nil {
			savedPath, saveErr = saveIssueReport(dir, prefix, report)
		}
	}
	printIssueReport(out, intro, link, report, savedPath)
	if link != "" && saveErr != nil {
		_, _ = fmt.Fprintf(out, "  Could not save the error to a file: %v\n\n", saveErr)
	}
}

func setFlagNames(flags *pflag.FlagSet) []string {
	var names []string
	flags.Visit(func(f *pflag.Flag) { names = append(names, "--"+f.Name) })
	return names
}
