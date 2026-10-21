package integration

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentTmpfsActions(t *testing.T) {
	stagingRoot := filepath.Join(testDir, "concurrent_root")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		// Alternate between images whose /sbin is a symlink and a file.
		h := helper{image: "test-image", rootMode: "tmpfs", stagingRoot: stagingRoot}
		path, want := "/etc/image_marker", "from-docker-image"
		if i%2 == 1 {
			h.image = "other-image"
			path, want = "/sbin", "other-image-file"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := h.run("read-file", path)
			if err != nil || out != want {
				t.Errorf("%s: got %v: %q, want %q", h.image, err, out, want)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Each action mounts its root in its own mount namespace, so nothing may
	// be left behind on the runner.
	entries, err := os.ReadDir(stagingRoot)
	if err != nil || len(entries) != 0 {
		t.Errorf("staging root not empty: %v, %v", entries, err)
	}
	for _, name := range []string{"/sbin", "/top_level_file"} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Errorf("image mount point left in runner root at %s: %v", name, err)
		}
	}
}
