package integration

import (
	"os"
	"path/filepath"
	"testing"
)

const keptDirError = "staging-root must not be below a kept directory"

func TestStagingRootBelowKeptDirectory(t *testing.T) {
	tmpLink := filepath.Join(testDir, "tmp_link")
	if err := os.Symlink("/tmp", tmpLink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(tmpLink) })

	for _, tc := range []struct{ name, stagingRoot string }{
		{"Direct", "/tmp/review_stage"},
		{"ViaSymlink", filepath.Join(tmpLink, "review_stage")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helper{image: "test-image", stagingRoot: tc.stagingRoot}.expectRejected(t, keptDirError)
		})
	}
}

func TestKeptDirectoryResolvingAboveStagingRoot(t *testing.T) {
	config := filepath.Join(testDir, "alias.toml")
	if err := os.WriteFile(config, []byte("keep-dirs = [\"alias\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The default staging root is /var/action_root.
	for _, tc := range []struct{ name, target string }{
		{"Var", "/var"},
		{"StagingRoot", "/var/action_root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Symlink(tc.target, "/alias"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove("/alias") })
			helper{image: "test-image", config: config}.expectRejected(t, keptDirError)
		})
	}
}
