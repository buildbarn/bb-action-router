package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	passed int
	failed int
)

func check(desc string, ok bool) {
	if ok {
		fmt.Printf("PASS: %s\n", desc)
		passed++
	} else {
		fmt.Printf("FAIL: %s\n", desc)
		failed++
	}
}

func checkOutput(desc, expected, actual string) {
	if expected == actual {
		fmt.Printf("PASS: %s\n", desc)
		passed++
	} else {
		fmt.Printf("FAIL: %s (expected %q, got %q)\n", desc, expected, actual)
		failed++
	}
}

func runHelper(helperPath, fetcherSocket, imageRef string, extraArgs []string, command ...string) (string, error) {
	args := []string{
		"--root-mode=tmpfs",
		"--docker-image-ref=" + imageRef,
		"--fetcher-socket=" + fetcherSocket,
		// Exercise the configurable path with a non-default build user; the
		// seeded /etc/passwd has a matching build:1000:1000 entry. (The
		// default is the in-namespace root user, 0:0.)
		"--build-user=1000:1000",
	}
	args = append(args, extraArgs...)
	// The helper requires its own arguments to be terminated with "--".
	args = append(args, "--")
	args = append(args, command...)

	cmd := exec.Command(helperPath, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func startMockFetcher(socketPath, dockerRoot, otherDockerRoot string) (func(), error) {
	self, _ := os.Executable()
	cmd := exec.Command(self, "--mock-fetcher", socketPath, dockerRoot, otherDockerRoot)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(socketPath); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("fetcher socket did not appear at %s", socketPath)
	}
	return func() { cmd.Process.Kill(); cmd.Wait() }, nil
}

func setupDockerRoot(root string) error {
	dirs := []string{
		"bin", "usr/bin", "usr/lib", "usr/lib64", "usr/sbin", "etc", "opt", "home", "root", "var",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}
	symlinks := map[string]string{
		"lib":   "/usr/lib",
		"lib64": "usr/lib64",
		"sbin":  "usr/sbin",
	}
	for name, target := range symlinks {
		os.Remove(filepath.Join(root, name))
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"),
		[]byte("root:x:0:0::/tmp:/bin/sh\nbuild:x:1000:1000::/tmp:/bin/sh\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"),
		[]byte("root:x:0:\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "etc/image_marker"),
		[]byte("from-docker-image"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "etc/resolv.conf"),
		[]byte("nameserver 9.9.9.9"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "top_level_file"),
		[]byte("top-level-file-content"), 0o644); err != nil {
		return err
	}
	// Marker in the symlink's target so we can verify the symlink is
	// resolved and its target bind-mounted.
	if err := os.WriteFile(filepath.Join(root, "usr/lib/lib_marker"),
		[]byte("from-usr-lib"), 0o644); err != nil {
		return err
	}
	return nil
}

func runTests() {
	helperPath := "/bin/bb_chroot_helper"
	testDir := "/var/chroot_integration_test"
	dockerRoot := filepath.Join(testDir, "docker_root")
	otherDockerRoot := filepath.Join(testDir, "other_docker_root")
	socketPath := filepath.Join(testDir, "fetcher.sock")

	os.MkdirAll(testDir, 0o755)
	os.MkdirAll("/tmp", 0o1777)
	defer os.RemoveAll(testDir)
	if err := os.Symlink("/tmp", filepath.Join(testDir, "tmp_link")); err != nil {
		die(fmt.Sprintf("create staging root symlink: %v", err))
	}

	if err := setupDockerRoot(dockerRoot); err != nil {
		die(fmt.Sprintf("setup docker root: %v", err))
	}
	if err := setupDockerRoot(otherDockerRoot); err != nil {
		die(fmt.Sprintf("setup other docker root: %v", err))
	}
	if err := os.Remove(filepath.Join(otherDockerRoot, "sbin")); err != nil {
		die(fmt.Sprintf("remove other image sbin: %v", err))
	}
	if err := os.WriteFile(filepath.Join(otherDockerRoot, "sbin"), []byte("other-image-file"), 0o644); err != nil {
		die(fmt.Sprintf("create other image sbin: %v", err))
	}
	if err := os.WriteFile(filepath.Join(otherDockerRoot, "etc/image_marker"), []byte("other-image"), 0o644); err != nil {
		die(fmt.Sprintf("create other image marker: %v", err))
	}

	// Write known host /etc files.
	os.MkdirAll("/etc", 0o755)
	os.WriteFile("/etc/resolv.conf", []byte("nameserver 1.2.3.4\n"), 0o644)
	os.WriteFile("/etc/hostname", []byte("test-host\n"), 0o644)
	os.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost\n"), 0o644)

	// Copy ourselves into the docker roots so we're available after chroot to run probes.
	self, _ := os.Executable()
	data, err := os.ReadFile(self)
	if err != nil {
		die(fmt.Sprintf("read test executable: %v", err))
	}
	for _, root := range []string{dockerRoot, otherDockerRoot} {
		if err := os.WriteFile(filepath.Join(root, "bin/integration_test"), data, 0o755); err != nil {
			die(fmt.Sprintf("copy test executable to %s: %v", root, err))
		}
	}

	cleanupFetcher, err := startMockFetcher(socketPath, dockerRoot, otherDockerRoot)
	if err != nil {
		die(fmt.Sprintf("start mock fetcher: %v", err))
	}
	defer cleanupFetcher()

	fmt.Println("\n=== Image root (image marker) ===")
	out, _ := runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/etc/image_marker")
	checkOutput("Docker image /etc/image_marker visible", "from-docker-image", out)

	fmt.Println("\n=== Host resolv.conf bind-mounted ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/etc/resolv.conf")
	checkOutput("Host resolv.conf visible", "nameserver 1.2.3.4", out)

	fmt.Println("\n=== /proc/self/exe accessible ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=readlink", "/proc/self/exe")
	check("/proc/self/exe resolves", out != "" && !strings.Contains(out, "error"))

	fmt.Println("\n=== Writing to /etc as unprivileged user should fail ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=write-test", "/etc/test_write")
	checkOutput("Cannot write to /etc (unprivileged)", "error: open /etc/test_write: read-only file system", out)

	fmt.Println("\n=== Creating a root directory as unprivileged user should fail ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=mkdir", "/foo")
	checkOutput("Cannot write to /foo (read-only root)", "error: mkdir /foo: read-only file system", out)

	fmt.Println("\n=== Test 5: HOME=/tmp ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=user-home")
	checkOutput("HOME is /tmp", "/tmp", out)

	fmt.Println("\n=== Top-level file bind-mount ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/top_level_file")
	checkOutput("Top-level file visible", "top-level-file-content", out)

	fmt.Println("\n=== Host dirs hidden ===")
	os.MkdirAll("/stale_test_dir", 0o755)
	os.WriteFile("/stale_test_dir/marker", []byte("should-be-hidden"), 0o644)
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/stale_test_dir/marker")
	check("Host dir hidden", strings.Contains(out, "error") || out == "")
	os.RemoveAll("/stale_test_dir")

	fmt.Println("\n=== Network isolation ===")
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--network-isolation"},
		"/bin/integration_test", "--probe=interfaces")
	ifaces := strings.TrimSpace(out)
	hasEth := false
	for _, iface := range strings.Split(ifaces, "\n") {
		iface = strings.TrimSpace(iface)
		// In some container runtimes a bunch of extra interfaces related to the
		// network bridge come up in the sandbox, so we need to assert that there
		// is no eth0, etc.. rather than asserting that there is only lo.
		if strings.HasPrefix(iface, "eth") || strings.HasPrefix(iface, "en") ||
			strings.HasPrefix(iface, "wl") || strings.HasPrefix(iface, "veth") {
			hasEth = true
		}
	}
	check(fmt.Sprintf("Network isolated (no external interfaces, got: %s)",
		strings.ReplaceAll(ifaces, "\n", ", ")), !hasEth)

	fmt.Println("\n=== Top-level symlinks preserved ===")
	// docker_root has /lib -> /usr/lib.
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=readlink", "/lib")
	checkOutput("Top-level symlink preserved", "/usr/lib", out)
	// Verify that the symlink resolves within the action root.
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/lib/lib_marker")
	checkOutput("Top-level symlink resolves", "from-usr-lib", out)

	fmt.Println("\n=== Symlink traversal stays inside the action root ===")
	escapePath := filepath.Join(dockerRoot, "escape")
	if err := os.Symlink("../../etc", escapePath); err != nil {
		die(fmt.Sprintf("create escape symlink: %v", err))
	}
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/escape/image_marker")
	checkOutput("Symlink traversal resolves within the action root", "from-docker-image", out)
	os.Remove(escapePath)

	fmt.Println("\n=== Concurrent actions using different image layouts ===")
	const actions = 16
	failures := 0
	var firstFailure string
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < actions; i++ {
		imageRef := "test-image"
		path, expected := "/etc/image_marker", "from-docker-image"
		if i%2 == 1 {
			imageRef = "other-image"
			path, expected = "/sbin", "other-image-file"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := runHelper(helperPath, socketPath, imageRef, []string{"--no-network-isolation"},
				"/bin/integration_test", "--probe=read-file", path)
			if err != nil || out != expected {
				mu.Lock()
				failures++
				if firstFailure == "" {
					firstFailure = fmt.Sprintf("%s: %v: %s", imageRef, err, out)
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	check(fmt.Sprintf("%d concurrent actions succeeded (failures: %d, first: %s)", actions, failures, firstFailure), failures == 0)

	stageEntries, err := os.ReadDir("/var/action_root")
	check(fmt.Sprintf("Staging mount point is empty (got: %v)", stageEntries), err == nil && len(stageEntries) == 0)
	for _, name := range []string{"sbin", "top_level_file"} {
		_, err := os.Lstat("/" + name)
		check(fmt.Sprintf("No image mount point left in runner root at /%s", name), os.IsNotExist(err))
	}

	fmt.Println("\n=== Overlay mode (sequential actions only) ===")
	// Omitting root-mode selects overlay. Reuse the staging directory across
	// images where /sbin changes from a symlink to a file and back again.
	for _, imageRef := range []string{"test-image", "other-image", "test-image"} {
		path, expected := "/sbin", "other-image-file"
		if imageRef == "test-image" {
			path, expected = "/lib/lib_marker", "from-usr-lib"
		}
		overlay := exec.Command(helperPath, "--docker-image-ref="+imageRef, "--fetcher-socket="+socketPath,
			"--build-user=1000:1000", "--", "/bin/integration_test", "--probe=read-file", path)
		overlayOut, err := overlay.CombinedOutput()
		checkOutput("Default overlay image "+imageRef, expected, strings.TrimSpace(string(overlayOut)))
		check("Default overlay command succeeds", err == nil)
	}
	out, err = runHelper(helperPath, socketPath, "test-image", []string{"--root-mode=overlay"},
		"/bin/integration_test", "--probe=mkdir", "/foo")
	checkOutput("Overlay root is read-only", "error: mkdir /foo: read-only file system", out)
	check("Overlay probe succeeds", err == nil)
	// Mount points remain on disk, but must not expose or modify the image.
	info, err := os.Stat("/var/action_root/top_level_file")
	check("Overlay leaves an empty file mount point", err == nil && info.Size() == 0)
	marker, err := os.ReadFile(filepath.Join(dockerRoot, "usr/lib/lib_marker"))
	check("Overlay cleanup preserves symlink targets", err == nil && string(marker) == "from-usr-lib")

	fmt.Println("\n=== Staging roots below kept directories are rejected ===")
	for _, stage := range []string{"/tmp/review_stage", filepath.Join(testDir, "tmp_link/review_stage")} {
		out, err := runHelper(helperPath, socketPath, "test-image",
			[]string{"--staging-root=" + stage},
			"/bin/integration_test", "--probe=uid")
		check("Rejects staging root "+stage,
			err != nil && strings.Contains(out, "staging-root must not be below a kept directory"))
	}

	aliasConfig := filepath.Join(testDir, "alias.toml")
	if err := os.WriteFile(aliasConfig, []byte("keep-dirs = [\"alias\"]\n"), 0o644); err != nil {
		die(fmt.Sprintf("write alias config: %v", err))
	}
	for _, target := range []string{"/var", "/var/action_root"} {
		if err := os.Symlink(target, "/alias"); err != nil {
			die(fmt.Sprintf("create kept directory symlink: %v", err))
		}
		out, err := runHelper(helperPath, socketPath, "test-image",
			[]string{"--root-mode=overlay", "--config=" + aliasConfig},
			"/bin/integration_test", "--probe=uid")
		check("Rejects kept directory resolving to "+target,
			err != nil && strings.Contains(out, "staging-root must not be below a kept directory"))
		if err := os.Remove("/alias"); err != nil {
			die(fmt.Sprintf("remove kept directory symlink: %v", err))
		}
	}

	fmt.Println("\n=== Inline working directory ===")
	inlineRoot := "/runner/image"
	workDir := filepath.Join(inlineRoot, "bazel_exec_root")
	if err := setupDockerRoot(inlineRoot); err != nil {
		die(fmt.Sprintf("setup inline root: %v", err))
	}
	for path, contents := range map[string][]byte{
		filepath.Join(inlineRoot, "bin/integration_test"): data,
		filepath.Join(testDir, "inline.toml"):             []byte("keep-dirs = [\"runner\"]\n"),
	} {
		if err := os.WriteFile(path, contents, 0o755); err != nil {
			die(fmt.Sprintf("write inline fixture: %v", err))
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		die(fmt.Sprintf("create inline working directory: %v", err))
	}
	if err := os.Chown(workDir, 1000, 1000); err != nil {
		die(fmt.Sprintf("chown inline working directory: %v", err))
	}
	if err := os.WriteFile(filepath.Join(workDir, "input"), []byte("inline-input"), 0o644); err != nil {
		die(fmt.Sprintf("write inline input: %v", err))
	}
	for _, mode := range []string{"tmpfs", "overlay"} {
		for _, probe := range []struct{ name, path, expected string }{
			{"read-file", "input", "inline-input"},
			{"write-test", "output", "write succeeded"},
		} {
			cmd := exec.Command(helperPath, "--root-mode="+mode, "--build-user=1000:1000",
				"--config="+filepath.Join(testDir, "inline.toml"), "--",
				"/bin/integration_test", "--probe="+probe.name, probe.path)
			cmd.Dir = workDir
			out, err := cmd.CombinedOutput()
			check(mode+" inline "+probe.name, err == nil && strings.TrimSpace(string(out)) == probe.expected)
		}
	}
	fmt.Printf("\n================================\n")
	fmt.Printf("Results: %d passed, %d failed\n", passed, failed)
	fmt.Printf("================================\n")
	if failed > 0 {
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		runTests()
		return
	}
	switch os.Args[1] {
	case "--mock-fetcher":
		cmdMockFetcher()
	case "--probe=read-file":
		cmdProbeReadFile()
	case "--probe=readlink":
		cmdProbeReadlink()
	case "--probe=uid":
		fmt.Print(syscall.Getuid())
	case "--probe=user-home":
		u, err := user.Current()
		if err != nil {
			fmt.Printf("error: %v", err)
			return
		}
		fmt.Print(u.HomeDir)
	case "--probe=write-test":
		cmdProbeWriteTest()
	case "--probe=mkdir":
		cmdProbeMkdirTest()
	case "--probe=interfaces":
		cmdProbeInterfaces()
	default:
		die("unsupported arg")
	}
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "integration_test: %s\n", msg)
	os.Exit(1)
}

func cmdMockFetcher() {
	if len(os.Args) != 5 {
		die("Usage: integration_test --mock-fetcher <socket_path> <docker_root> <other_docker_root>")
	}
	socketPath := os.Args[2]
	dockerRoots := map[string]string{"test-image": os.Args[3], "other-image": os.Args[4]}

	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		die(fmt.Sprintf("listen: %v", err))
	}
	defer listener.Close()

	fmt.Fprintf(os.Stderr, "mock_fetcher: listening on %s\n", socketPath)
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			fmt.Fprintf(c, "HI\n")
			reader := bufio.NewReader(c)
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
			if len(parts) == 2 && parts[0] == "ACQUIRE" && dockerRoots[parts[1]] != "" {
				fmt.Fprintf(c, "OK %s\n", dockerRoots[parts[1]])
				// Hold the image lease until the helper closes the socket.
				io.Copy(io.Discard, c)
			} else {
				fmt.Fprintf(c, "ERROR bad request\n")
			}
		}(conn)
	}
}

// Probes (they run inside the helper)

func cmdProbeReadFile() {
	if len(os.Args) < 3 {
		fmt.Print("error: missing path")
		return
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Printf("error: %v", err)
		return
	}
	fmt.Print(strings.TrimSpace(string(data)))
}

func cmdProbeReadlink() {
	if len(os.Args) < 3 {
		fmt.Print("error: missing path")
		return
	}
	target, err := os.Readlink(os.Args[2])
	if err != nil {
		fmt.Printf("error: %v", err)
		return
	}
	fmt.Print(target)
}

func cmdProbeWriteTest() {
	if len(os.Args) < 3 {
		fmt.Print("error: missing path")
		return
	}
	err := os.WriteFile(os.Args[2], []byte("test"), 0o644)
	if err != nil {
		fmt.Printf("error: %v", err)
	} else {
		fmt.Print("write succeeded")
		os.Remove(os.Args[2])
	}
}

func cmdProbeMkdirTest() {
	if len(os.Args) < 3 {
		fmt.Print("error: missing path")
		return
	}
	err := os.Mkdir(os.Args[2], 0o644)
	if err != nil {
		fmt.Printf("error: %v", err)
	} else {
		fmt.Print("mkdir succeeded")
		os.Remove(os.Args[2])
	}
}

func cmdProbeInterfaces() {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		fmt.Printf("error: %v", err)
		return
	}
	var ifaces []string
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, ":"); idx > 0 {
			ifaces = append(ifaces, strings.TrimSpace(line[:idx]))
		}
	}
	fmt.Print(strings.Join(ifaces, "\n"))
}
