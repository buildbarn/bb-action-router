package integration

import (
	"os/exec"
	"strings"
	"testing"
)

// helper describes a bb_chroot_helper invocation. Empty fields are omitted
// from the command line, so the helper's defaults apply.
type helper struct {
	// Sideloaded image served by the mock fetcher. Empty selects inline mode.
	image          string
	rootMode       string
	isolateNetwork bool
	stagingRoot    string
	config         string
	// Working directory of the helper, which inline mode derives the image
	// root from.
	dir string
}

// run runs a probe through the helper, returning the combined output of the
// helper and the probe.
func (h helper) run(probe string, args ...string) (string, error) {
	// The images' /etc/passwd has a matching entry for this non-default
	// build user.
	argv := []string{"--build-user=1000:1000"}
	if h.image != "" {
		argv = append(argv, "--docker-image-ref="+h.image, "--fetcher-socket="+fetcherSocket)
	}
	if h.rootMode != "" {
		argv = append(argv, "--root-mode="+h.rootMode)
	}
	if h.isolateNetwork {
		argv = append(argv, "--network-isolation")
	} else {
		argv = append(argv, "--no-network-isolation")
	}
	if h.stagingRoot != "" {
		argv = append(argv, "--staging-root="+h.stagingRoot)
	}
	if h.config != "" {
		argv = append(argv, "--config="+h.config)
	}
	argv = append(argv, "--", selfPath, "probe", probe)
	argv = append(argv, args...)

	cmd := exec.Command(helperPath, argv...)
	cmd.Dir = h.dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// probe runs a probe through the helper and fails the test if the helper
// fails.
func (h helper) probe(t *testing.T, probe string, args ...string) string {
	t.Helper()
	out, err := h.run(probe, args...)
	if err != nil {
		t.Fatalf("helper %+v failed: %v\n%s", h, err, out)
	}
	return out
}

func (h helper) expectProbe(t *testing.T, want, probe string, args ...string) {
	t.Helper()
	if got := h.probe(t, probe, args...); got != want {
		t.Errorf("probe %s %q: got %q, want %q", probe, args, got, want)
	}
}

// expectRejected checks that the helper fails with an error containing want,
// before running the action.
func (h helper) expectRejected(t *testing.T, want string) {
	t.Helper()
	out, err := h.run("uid")
	if err == nil || !strings.Contains(out, want) {
		t.Errorf("helper %+v: got %v: %q, want failure containing %q", h, err, out, want)
	}
}
