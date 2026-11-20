package integration

import (
	"testing"
)

// TestActionRoot checks the action's view of a sideloaded image.
func TestActionRoot(t *testing.T) {
	h := helper{image: "test-image"}
	for _, tc := range []struct {
		name, probe, arg, want string
	}{
		{"ImageFile", "read-file", "/etc/image_marker", "from-docker-image"},
		{"HostEtcFile", "read-file", "/etc/resolv.conf", "nameserver 1.2.3.4"},
		{"TopLevelFile", "read-file", "/top_level_file", "top-level-file-content"},
		{"TopLevelSymlink", "readlink", "/lib", "/usr/lib"},
		{"TopLevelSymlinkTarget", "read-file", "/lib/lib_marker", "from-usr-lib"},
		{"SymlinkEscape", "read-file", "/escape/image_marker", "from-docker-image"},
		{"HostDirHidden", "read-file", hostOnlyDir + "/marker", "error: open " + hostOnlyDir + "/marker: no such file or directory"},
		// Needed by ld.so for $ORIGIN and by the JVM; see README.md.
		{"ProcSelfExe", "readlink", "/proc/self/exe", selfPath},
		{"BuildUserHome", "home", "", "/tmp"},
		{"EtcReadOnly", "write-file", "/etc/test_write", "error: open /etc/test_write: read-only file system"},
		{"RootReadOnly", "mkdir", "/foo", "error: mkdir /foo: read-only file system"},
		{"TmpWritable", "write-file", "/tmp/test_write", "write succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.expectProbe(t, tc.want, tc.probe, tc.arg)
		})
	}
}

// TestChrootEscape checks that an action running as root in its user namespace
// can't get out of the action root by escaping a chroot, as the runner's root
// isn't mounted in the action's mount namespace at all.
func TestChrootEscape(t *testing.T) {
	h := helper{image: "test-image", buildUser: "0:0"}
	h.expectProbe(t, "error: open "+hostOnlyDir+"/marker: no such file or directory", "escape-chroot", hostOnlyDir+"/marker")
}
