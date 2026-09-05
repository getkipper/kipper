package controller

import "testing"

// The paths these engines print, taken from what each actually writes when its
// disk stops accepting writes. Postgres and the kernel name no path at all,
// which is the case the fallback exists for, and Postgres is the one the
// incident produced.
func TestAbsolutePathsOnRealLines(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`, nil},
		{`write /var/lib/data/000001.log: read-only file system`, []string{"/var/lib/data/000001.log"}},
		{`EXT4-fs (sdb): Remounting filesystem read-only`, nil},
		{`[ERROR] failed opening '/data/mysql/ibdata1' (errno: 30 - Read-only file system)`, []string{"/data/mysql/ibdata1"}},
		{`Failed to write to file: /var/lib/mongodb/x, errno:30 Read-only file system`, []string{"/var/lib/mongodb/x"}},
		{`cannot open /etc/app/settings.yml for writing: read-only file system`, []string{"/etc/app/settings.yml"}},
		// Both paths, because the destination is the one that refused the write.
		{`mv /tmp/backup /var/lib/wal/000001: Read-only file system`, []string{"/tmp/backup", "/var/lib/wal/000001"}},
		// Glued to a prefix, which is how Java and Node print one.
		{`java.io.FileNotFoundException:/var/lib/pg/global (Read-only file system)`, []string{"/var/lib/pg/global"}},
	} {
		got := absolutePaths(tc.line)
		if len(got) != len(tc.want) {
			t.Errorf("absolutePaths(%q) = %v, want %v", tc.line, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("absolutePaths(%q) = %v, want %v", tc.line, got, tc.want)
				break
			}
		}
	}
}
