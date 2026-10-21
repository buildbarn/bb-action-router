package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInlineMode runs actions whose image root is derived from the working
// directory, as set up by the merge_docker_root pipeline operation.
func TestInlineMode(t *testing.T) {
	const imageRoot = "/runner/image"
	if err := testImage().write(imageRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll("/runner") })
	workDir := filepath.Join(imageRoot, "bazel_exec_root")
	if err := writeFiles(workDir, map[string]string{"input": "inline-input"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(workDir, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	// The working directory is only visible in the action root if its
	// top-level directory is kept.
	config := filepath.Join(testDir, "inline.toml")
	if err := os.WriteFile(config, []byte("keep-dirs = [\"runner\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range rootModes {
		t.Run(mode, func(t *testing.T) {
			h := helper{rootMode: mode, config: config, dir: workDir}
			h.expectProbe(t, "inline-input", "read-file", "input")
			h.expectProbe(t, "write succeeded", "write-file", "output")
			h.expectProbe(t, "from-docker-image", "read-file", "/etc/image_marker")
		})
	}
}
