package installer

import (
	"strings"
	"testing"
)

func TestPrepareWorkerHostInstallsStorageAndRaisesInotify(t *testing.T) {
	worker := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=rocky\nVERSION_ID=\"10.2\"\nID_LIKE=\"rhel centos fedora\"\n"},
		{contains: "rpm -q", out: "missing\n"},
	}}
	master := &routedRunner{routes: []route{{contains: "cat " + k3sResolvConfPath, out: "nameserver 1.1.1.1\n"}}}

	if err := prepareWorkerHost(master, worker); err != nil {
		t.Fatalf("prepareWorkerHost: %v\n%s", err, strings.Join(worker.commands, "\n"))
	}
	for _, want := range []string{
		"fs.inotify.max_user_instances=512",
		"iscsi-initiator-utils",
		"systemctl enable --now iscsid",
		kubeletSysctlPath,
		"kipper-kubelet.yaml",
		"kipper-dns.yaml",
		"kipper-selinux.yaml",
	} {
		if !worker.ran(want) {
			t.Errorf("worker preparation never ran %q\n%s", want, strings.Join(worker.commands, "\n"))
		}
	}
}

// Installing the NFS client can enable rpcbind, so hardening must follow it.
func TestPrepareWorkerHostHardensAfterTheStoragePackages(t *testing.T) {
	worker := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=debian\nVERSION_ID=\"13\"\n"},
		{contains: "list-unit-files rpcbind.socket", out: "rpcbind.socket\n"},
	}}
	master := &routedRunner{}
	if err := prepareWorkerHost(master, worker); err != nil {
		t.Fatal(err)
	}
	install, mask := -1, -1
	for i, c := range worker.commands {
		if strings.Contains(c, "nfs-common") && !strings.Contains(c, "dpkg-query") && install < 0 {
			install = i
		}
		if strings.Contains(c, "systemctl mask rpcbind.socket") {
			mask = i
		}
	}
	if install < 0 || mask < install {
		t.Fatalf("rpcbind masked at %d, storage packages at %d; want the mask after the packages\n%s", mask, install, strings.Join(worker.commands, "\n"))
	}
}

func TestHostRoleK3sUnit(t *testing.T) {
	for role, want := range map[HostRole]string{"": "k3s", RoleServer: "k3s", RoleWorker: "k3s-agent"} {
		if got := role.k3sUnit(); got != want {
			t.Errorf("%q.k3sUnit() = %q, want %q", role, got, want)
		}
	}
}

func TestPreflightWorkerAsksOnlyAboutTheKubeletPort(t *testing.T) {
	worker := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=debian\nVERSION_ID=\"13\"\n"},
		{contains: "MemTotal", out: "4000000\n"},
		{contains: "df ", out: "40000000\n"},
		{contains: "nproc", out: "2\n"},
		{contains: "ss -tlnpH", out: "LISTEN 0 4096 *:10250 *:* users:((\"kubelet\",pid=1,fd=3))\n"},
	}}
	_, _, err := PreflightWorker(worker)
	if err == nil || !strings.Contains(err.Error(), "10250 (kubelet)") {
		t.Fatalf("err = %v, want the foreign kubelet listener refused", err)
	}
	for _, c := range worker.commands {
		if strings.Contains(c, "ss -tlnpH") && (strings.Contains(c, ":80 ") || strings.Contains(c, ":6443")) {
			t.Errorf("worker preflight asked about server ports: %s", c)
		}
	}
}
