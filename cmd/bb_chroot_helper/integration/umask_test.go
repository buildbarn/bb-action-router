package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRestrictiveUmask starts the helper with a umask that strips the
// permissions of every directory it creates. The staging root, the slot and
// the mount points are created by root, but have to be traversable by the host
// user the action runs as.
func TestRestrictiveUmask(t *testing.T) {
	for _, tc := range []struct {
		name  string
		umask int
	}{
		{"Umask077", 0o077},
		{"Umask027", 0o027},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Neither the staging root nor its slots exist yet, so the helper
			// has to create both.
			stagingRoot := filepath.Join(testDir, "umask_root_"+tc.name)
			h := helper{image: "test-image", stagingRoot: stagingRoot, umask: tc.umask}
			h.expectProbe(t, "from-docker-image", "read-file", "/etc/image_marker")
			// A second action reuses the slot, whose mode the helper has to
			// set rather than inherit.
			h.expectProbe(t, "usr/sbin", "readlink", "/sbin")
			h.expectProbe(t, "top-level-file-content", "read-file", "/top_level_file")
		})
	}
}

// TestRestrictiveStagingRootIsFixed starts the helper with a staging root that
// a previous run left behind with a mode the host user can't traverse.
func TestRestrictiveStagingRootIsFixed(t *testing.T) {
	stagingRoot := filepath.Join(testDir, "restrictive_root")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	helper{image: "test-image", stagingRoot: stagingRoot}.expectProbe(t, "from-docker-image", "read-file", "/etc/image_marker")
	if info, err := os.Stat(stagingRoot); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("got staging root %v, %v, want mode 0755", info, err)
	}
}

// TestTraversableStagingRootIsLeftAlone checks that the helper only changes the
// mode of a staging root the host user can't traverse, as it may not be
// allowed to change it otherwise.
func TestTraversableStagingRootIsLeftAlone(t *testing.T) {
	for _, mode := range []os.FileMode{0o711, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			stagingRoot := filepath.Join(testDir, "traversable_root_"+mode.String())
			if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stagingRoot, mode); err != nil {
				t.Fatal(err)
			}
			helper{image: "test-image", stagingRoot: stagingRoot}.expectProbe(t, "from-docker-image", "read-file", "/etc/image_marker")
			if info, err := os.Stat(stagingRoot); err != nil || info.Mode().Perm() != mode {
				t.Errorf("got staging root %v, %v, want mode %v", info, err, mode)
			}
		})
	}
}
