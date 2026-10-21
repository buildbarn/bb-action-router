// Package integration tests bb_chroot_helper end to end. The tests must run as
// root inside the container image built by this package, as they modify the
// container's filesystem. Use :run_integration_test to run them.
//
// The test binary doubles as the action command: it is copied into every image
// root, and the helper runs it as "<binary> probe <name> [arg]".
package integration

import (
	"maps"
	"os"
	"path/filepath"
)

const (
	helperPath = "/bin/bb_chroot_helper"
	testDir    = "/var/chroot_integration_test"
	// Directory present on the runner, but not in any image.
	hostOnlyDir = "/host_only_dir"
	// Path of the test binary, both in the runner image and in every image root.
	selfPath = "/bin/bb_chroot_helper_integration_test"
)

var (
	testImageRoot  = filepath.Join(testDir, "test_image")
	otherImageRoot = filepath.Join(testDir, "other_image")
	fetcherSocket  = filepath.Join(testDir, "fetcher.sock")
)

// image describes the contents of an image root. Parent directories of files
// are created as needed.
type image struct {
	dirs     []string
	files    map[string]string
	symlinks map[string]string
}

func testImage() image {
	return image{
		dirs: []string{"bin", "usr/bin", "usr/lib", "usr/lib64", "usr/sbin", "etc", "opt", "home", "root", "var"},
		files: map[string]string{
			// Matches --build-user=1000:1000, which the tests run with.
			"etc/passwd":       "root:x:0:0::/tmp:/bin/sh\nbuild:x:1000:1000::/tmp:/bin/sh\n",
			"etc/group":        "root:x:0:\n",
			"etc/image_marker": "from-docker-image",
			// Shadowed by the host's /etc/resolv.conf.
			"etc/resolv.conf": "nameserver 9.9.9.9",
			"top_level_file":  "top-level-file-content",
			// Lets tests check that /lib is resolved within the action root.
			"usr/lib/lib_marker": "from-usr-lib",
		},
		symlinks: map[string]string{
			"lib":   "/usr/lib",
			"lib64": "usr/lib64",
			"sbin":  "usr/sbin",
			// Climbs above the image root, which must resolve to the image's /etc.
			"escape": "../../etc",
		},
	}
}

// otherImage differs from testImage in the type of a top-level entry, so tests
// can check that reused staging directories don't leak entries between images.
func otherImage() image {
	img := testImage()
	img.files = maps.Clone(img.files)
	img.symlinks = maps.Clone(img.symlinks)
	delete(img.symlinks, "sbin")
	img.files["sbin"] = "other-image-file"
	img.files["etc/image_marker"] = "other-image"
	return img
}

// write creates the image at root, including a copy of the test binary at
// selfPath.
func (img image) write(root string) error {
	for _, dir := range img.dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}
	if err := writeFiles(root, img.files); err != nil {
		return err
	}
	for name, target := range img.symlinks {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, selfPath), data, 0o755)
}

func writeFiles(root string, files map[string]string) error {
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	return nil
}
