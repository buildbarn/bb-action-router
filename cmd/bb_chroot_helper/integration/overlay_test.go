package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOverlayReusesStagingRoot runs sequential actions in the same staging
// directory, with images in which /sbin changes from a symlink to a file and
// back again.
func TestOverlayReusesStagingRoot(t *testing.T) {
	stagingRoot := filepath.Join(testDir, "overlay_root")
	for _, image := range []string{"test-image", "other-image", "test-image"} {
		// No root mode, as overlay is the default.
		h := helper{image: image, stagingRoot: stagingRoot}
		if image == "test-image" {
			h.expectProbe(t, "usr/sbin", "readlink", "/sbin")
		} else {
			h.expectProbe(t, "other-image-file", "read-file", "/sbin")
		}
	}

	// Mount points remain on disk, but must not expose or modify the image.
	if info, err := os.Lstat(filepath.Join(stagingRoot, "top_level_file")); err != nil || info.Size() != 0 {
		t.Errorf("top_level_file mount point is not an empty file: %v, %v", info, err)
	}
	if marker, err := os.ReadFile(filepath.Join(testImageRoot, "usr/lib/lib_marker")); err != nil || string(marker) != "from-usr-lib" {
		t.Errorf("image symlink target modified: %q, %v", marker, err)
	}
}
