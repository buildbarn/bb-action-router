package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
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

func startMockFetcher(socketPath, dockerRoot string) (func(), error) {
	self, _ := os.Executable()
	cmd := exec.Command(self, "--mock-fetcher", socketPath, dockerRoot)
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
		"lib":   "usr/lib",
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
	socketPath := filepath.Join(testDir, "fetcher.sock")

	os.MkdirAll(testDir, 0o755)
	defer os.RemoveAll(testDir)

	if err := setupDockerRoot(dockerRoot); err != nil {
		die(fmt.Sprintf("setup docker root: %v", err))
	}

	// Write known host /etc files.
	os.MkdirAll("/etc", 0o755)
	os.WriteFile("/etc/resolv.conf", []byte("nameserver 1.2.3.4\n"), 0o644)
	os.WriteFile("/etc/hostname", []byte("test-host\n"), 0o644)
	os.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost\n"), 0o644)

	// Copy ourselves into the docker root so we're available after overlay to run probes.
	self, _ := os.Executable()
	selfDst := filepath.Join(dockerRoot, "bin/integration_test")
	data, _ := os.ReadFile(self)
	os.WriteFile(selfDst, data, 0o755)

	cleanupFetcher, err := startMockFetcher(socketPath, dockerRoot)
	if err != nil {
		die(fmt.Sprintf("start mock fetcher: %v", err))
	}
	defer cleanupFetcher()

	fmt.Println("\n=== Basic overlay (image marker) ===")
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
	checkOutput("Cannot write to /foo (unprivileged)", "error: mkdir /foo: permission denied", out)

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

	fmt.Println("\n=== Stale host dirs hidden ===")
	os.MkdirAll("/stale_test_dir", 0o755)
	os.WriteFile("/stale_test_dir/marker", []byte("should-be-hidden"), 0o644)
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/stale_test_dir/marker")
	check("Stale host dir hidden", strings.Contains(out, "error") || out == "")
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

	fmt.Println("\n=== Symlinks resolved and bind-mounted ===")
	// docker_root has /lib -> usr/lib. We can't replace the runner's
	// read-only /lib with a symlink, so the helper bind-mounts
	// docker_root/usr/lib onto /lib. Verify by reading a marker that only
	// exists under usr/lib.
	out, _ = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/lib/lib_marker")
	checkOutput("Symlink target bind-mounted at /lib", "from-usr-lib", out)

	fmt.Println("\n=== Test 10: Symlink escaping docker_root is rejected ===")
	// A top-level symlink whose target resolves outside docker_root must
	// be refused by resolve_symlink_within (otherwise an image could
	// trick the helper into bind-mounting host paths over runner /).
	escapePath := filepath.Join(dockerRoot, "escape")
	if err := os.Symlink("../../etc", escapePath); err != nil {
		die(fmt.Sprintf("create escape symlink: %v", err))
	}
	out, err = runHelper(helperPath, socketPath, "test-image",
		[]string{"--no-network-isolation"},
		"/bin/integration_test", "--probe=read-file", "/etc/image_marker")
	check("Helper exits non-zero on escape symlink", err != nil)
	check(fmt.Sprintf("Helper rejects escape with diagnostic (got: %s)", out),
		strings.Contains(out, "escapes docker_root"))
	os.Remove(escapePath)

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
	if len(os.Args) != 4 {
		die("Usage: integration_test --mock-fetcher <socket_path> <docker_root>")
	}
	socketPath, dockerRoot := os.Args[2], os.Args[3]

	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		die(fmt.Sprintf("listen: %v", err))
	}
	defer listener.Close()

	fmt.Fprintf(os.Stderr, "mock_fetcher: listening on %s, serving %s\n", socketPath, dockerRoot)
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
			if len(parts) == 2 && parts[0] == "ACQUIRE" {
				fmt.Fprintf(c, "OK %s\n", dockerRoot)
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
