package integration

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentActions(t *testing.T) {
	const actions = 16
	stagingRoot := filepath.Join(testDir, "concurrent_root")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < actions; i++ {
		// Alternate between images whose /sbin is a symlink and a file.
		h := helper{image: "test-image", stagingRoot: stagingRoot}
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

	// Slots are only created when every existing one is in use.
	created := len(slots(t, stagingRoot))
	if created < 1 || created > actions {
		t.Errorf("got %d slots for %d concurrent actions", created, actions)
	}
	helper{image: "test-image", stagingRoot: stagingRoot}.expectProbe(t, "from-docker-image", "read-file", "/etc/image_marker")
	if got := len(slots(t, stagingRoot)); got != created {
		t.Errorf("sequential action after concurrent ones created a slot: got %d slots, want %d", got, created)
	}

	// Each action mounts its root in its own mount namespace, so nothing may
	// be left behind in the runner's root.
	for _, name := range []string{"/sbin", "/top_level_file"} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Errorf("image mount point left in runner root at %s: %v", name, err)
		}
	}
}
