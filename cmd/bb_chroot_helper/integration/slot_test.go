package integration

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// slots returns the names of the entries in a staging root, failing the test
// if any of them isn't a slot directory.
func slots(t *testing.T, stagingRoot string) []string {
	t.Helper()
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.TrimLeft(entry.Name(), "0123456789") != "" {
			t.Errorf("unexpected entry %q in staging root", entry.Name())
		}
		names = append(names, entry.Name())
	}
	return names
}

// TestSequentialActionsReuseSlot runs sequential actions in the same staging
// root, with images in which /sbin changes from a symlink to a file and back
// again.
func TestSequentialActionsReuseSlot(t *testing.T) {
	stagingRoot := filepath.Join(testDir, "sequential_root")
	for _, image := range []string{"test-image", "other-image", "test-image"} {
		h := helper{image: image, stagingRoot: stagingRoot}
		if image == "test-image" {
			h.expectProbe(t, "usr/sbin", "readlink", "/sbin")
		} else {
			h.expectProbe(t, "other-image-file", "read-file", "/sbin")
		}
	}
	if got := slots(t, stagingRoot); !slices.Equal(got, []string{"0"}) {
		t.Errorf("got slots %q, want a single reused slot", got)
	}

	// Mount points remain on disk, but must not expose or modify the image.
	if info, err := os.Lstat(filepath.Join(stagingRoot, "0", "top_level_file")); err != nil || info.Size() != 0 {
		t.Errorf("top_level_file mount point is not an empty file: %v, %v", info, err)
	}
	if marker, err := os.ReadFile(filepath.Join(testImageRoot, "usr/lib/lib_marker")); err != nil || string(marker) != "from-usr-lib" {
		t.Errorf("image symlink target modified: %q, %v", marker, err)
	}
}

// TestLockedSlotIsSkipped holds the lock on the only slot, as a running action
// would, so the helper has to create a new one.
func TestLockedSlotIsSkipped(t *testing.T) {
	stagingRoot := filepath.Join(testDir, "locked_root")
	if err := writeFiles(filepath.Join(stagingRoot, "0"), map[string]string{"marker": "in-use"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(stagingRoot, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	helper{image: "test-image", stagingRoot: stagingRoot}.expectProbe(t, "from-docker-image", "read-file", "/etc/image_marker")
	if got := slots(t, stagingRoot); !slices.Equal(got, []string{"0", "1"}) {
		t.Errorf("got slots %q, want a new slot next to the locked one", got)
	}
	if _, err := os.Stat(filepath.Join(stagingRoot, "0", "marker")); err != nil {
		t.Errorf("locked slot was cleared: %v", err)
	}
}

// TestUnclearableSlotIsSkipped puts a non-empty directory in a slot, which the
// helper must not recurse into. The slot is skipped rather than failing every
// action that picks it.
func TestUnclearableSlotIsSkipped(t *testing.T) {
	stagingRoot := filepath.Join(testDir, "unclearable_root")
	if err := writeFiles(filepath.Join(stagingRoot, "0"), map[string]string{"bin/file": "not-a-mount-point"}); err != nil {
		t.Fatal(err)
	}
	h := helper{image: "test-image", stagingRoot: stagingRoot}
	out, err := h.run("read-file", "/etc/image_marker")
	if err != nil || !strings.Contains(out, "skipping slot "+stagingRoot+"/0") || !strings.HasSuffix(out, "from-docker-image") {
		t.Errorf("got %v: %q, want a skipped slot and the image marker", err, out)
	}
	if got := slots(t, stagingRoot); !slices.Equal(got, []string{"0", "1"}) {
		t.Errorf("got slots %q, want a new slot next to the unclearable one", got)
	}
	if _, err := os.Stat(filepath.Join(stagingRoot, "0", "bin", "file")); err != nil {
		t.Errorf("unclearable slot was modified: %v", err)
	}
}
