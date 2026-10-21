package integration

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "probe" {
		os.Exit(runProbe(os.Args[2], os.Args[3:]))
	}
	fetcher, err := setUp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "set up: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	fetcher.Close()
	os.RemoveAll(testDir)
	os.RemoveAll(hostOnlyDir)
	os.Exit(code)
}

func setUp() (net.Listener, error) {
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		return nil, err
	}
	// The image only contains the test binaries. /tmp is kept, so the action
	// sees the runner's.
	if err := os.MkdirAll("/tmp", 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod("/tmp", 0o1777); err != nil {
		return nil, err
	}
	// Host versions of the files in etc-files, which must shadow the image's.
	for name, contents := range map[string]string{
		"resolv.conf": "nameserver 1.2.3.4\n",
		"hostname":    "test-host\n",
		"hosts":       "127.0.0.1 localhost\n",
	} {
		if err := os.WriteFile(filepath.Join("/etc", name), []byte(contents), 0o644); err != nil {
			return nil, err
		}
	}
	if err := writeFiles(hostOnlyDir, map[string]string{"marker": "should-be-hidden"}); err != nil {
		return nil, err
	}
	if err := testImage().write(testImageRoot); err != nil {
		return nil, fmt.Errorf("write test image: %w", err)
	}
	if err := otherImage().write(otherImageRoot); err != nil {
		return nil, fmt.Errorf("write other image: %w", err)
	}
	return serveFetcher(fetcherSocket, map[string]string{
		"test-image":  testImageRoot,
		"other-image": otherImageRoot,
	})
}
