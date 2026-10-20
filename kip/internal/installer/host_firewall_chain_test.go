package installer

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubbedShell runs commands with the machine's own sh and the stand-ins in bin.
type stubbedShell struct{ bin string }

func (s stubbedShell) Run(command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s stubbedShell) RunStdin(command string, stdin io.Reader) (string, error) {
	return "", nil
}

// iptablesHost writes stand-ins for ufw, timeout and iptables. The iptables
// stub prints table for a bare -S and reports missingChain on stderr for a
// missing named chain, modelling the different errors in versions 1.8.9 and
// 1.8.10. A non-zero tableExit simulates failure to read the whole table.
func iptablesHost(t *testing.T, table, missingChain string, tableExit int) stubbedShell {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "table"), []byte(table), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "missing"), []byte(missingChain+"\n"), 0o600))
	tableFile, missingFile := "'"+filepath.Join(bin, "table")+"'", "'"+filepath.Join(bin, "missing")+"'"
	iptables := "#!/bin/sh\n[ \"$1\" = -w ] && shift 2\n" +
		"echo '# Warning: iptables-legacy tables present, use iptables-legacy to see them' >&2\n" +
		"if [ $# -eq 1 ] && [ \"$1\" = -S ]; then cat " + tableFile + " || exit 9; exit " + strconv.Itoa(tableExit) + "; fi\n" +
		"if grep -qx -- \"-N $2\" " + tableFile + "; then grep -- \"$2\" " + tableFile + "; exit 0; fi\n" +
		"cat " + missingFile + " >&2; exit 1\n"
	stubs := map[string]string{
		"iptables": iptables,
		"ufw":      "#!/bin/sh\nexit 0\n",
		"timeout":  "#!/bin/sh\nshift 3\nexec \"$@\"\n",
	}
	for name, script := range stubs {
		//nolint:gosec // the stand-ins have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755))
	}
	return stubbedShell{bin: bin}
}

const (
	policies      = "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n-P OUTPUT ACCEPT\n-N KUBE-FIREWALL\n-N ufw-before-input\n"
	missingIn1810 = "iptables: No chain/target/match by that name."
	missingIn189  = "iptables v1.8.9 (nf_tables): chain `ufw-user-input' in table `filter' is incompatible, use 'nft' tool."
)

func TestStopUFWReadsWhetherUFWsChainsAreGone(t *testing.T) {
	t.Run("a missing chain is gone on current iptables", func(t *testing.T) {
		assert.NoError(t, stopUFW(iptablesHost(t, policies, missingIn1810, 0)))
	})

	// Debian 12's iptables-nft 1.8.9 reports missing chains as incompatible.
	// Version 1.8.10 fixes this error message.
	t.Run("a missing chain is gone on iptables 1.8.9", func(t *testing.T) {
		assert.NoError(t, stopUFW(iptablesHost(t, policies, missingIn189, 0)))
	})

	t.Run("a chain that is still loaded is reported", func(t *testing.T) {
		host := iptablesHost(t, policies+"-N ufw-user-input\n-A ufw-user-input -p tcp -m tcp --dport 22 -j ACCEPT\n", missingIn189, 0)

		err := stopUFW(host)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "still loaded")
	})

	t.Run("a table iptables cannot read leaves the state unknown", func(t *testing.T) {
		err := stopUFW(iptablesHost(t, policies, missingIn189, 1))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not tell")
	})
}
