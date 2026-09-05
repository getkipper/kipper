package controller

import "testing"

// The paths these engines print, taken from what each actually writes when its
// disk stops accepting writes. Postgres and the kernel name no path at all,
// which is the case the fallback exists for, and Postgres is the one the
// incident produced.
func TestFirstAbsolutePathOnRealLines(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`, ""},
		{`write /var/lib/data/000001.log: read-only file system`, "/var/lib/data/000001.log"},
		{`EXT4-fs (sdb): Remounting filesystem read-only`, ""},
		{`[ERROR] failed opening '/data/mysql/ibdata1' (errno: 30 - Read-only file system)`, "/data/mysql/ibdata1"},
		{`Failed to write to file: /var/lib/mongodb/x, errno:30 Read-only file system`, "/var/lib/mongodb/x"},
		{`cannot open /etc/app/settings.yml for writing: read-only file system`, "/etc/app/settings.yml"},
	} {
		if got := firstAbsolutePath(tc.line); got != tc.want {
			t.Errorf("firstAbsolutePath(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
