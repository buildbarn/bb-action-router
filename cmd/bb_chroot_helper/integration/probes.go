package integration

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Probes run inside the action root. Errors are printed as "error: ..." with a
// zero exit code, so tests can tell a failing probe from a failing helper.
var probes = map[string]func(arg string) (string, error){
	"read-file": func(path string) (string, error) {
		data, err := os.ReadFile(path)
		return strings.TrimSpace(string(data)), err
	},
	"readlink": os.Readlink,
	"write-file": func(path string) (string, error) {
		if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
			return "", err
		}
		return "write succeeded", os.Remove(path)
	},
	"mkdir": func(path string) (string, error) {
		if err := os.Mkdir(path, 0o755); err != nil {
			return "", err
		}
		return "mkdir succeeded", os.Remove(path)
	},
	// Escapes a chroot the classic way, by chrooting into a subdirectory
	// while the working directory stays outside of it, then reads path.
	// Needs CAP_SYS_CHROOT, i.e. an action running as root.
	"escape-chroot": func(path string) (string, error) {
		dir, err := os.MkdirTemp("/tmp", "escape")
		if err != nil {
			return "", err
		}
		if err := syscall.Chroot(dir); err != nil {
			return "", err
		}
		for i := 0; i < 64; i++ {
			if err := syscall.Chdir(".."); err != nil {
				return "", err
			}
		}
		if err := syscall.Chroot("."); err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		return strings.TrimSpace(string(data)), err
	},
	// Starts a tamper probe in a new session, as a daemon would, and returns
	// once it runs.
	"spawn-tamperer": func(dir string) (string, error) {
		cmd := exec.Command(selfPath, "probe", "tamper", dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return "", err
		}
		if err := cmd.Start(); err != nil {
			return "", err
		}
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			return "", err
		}
		if line != "ready\n" {
			return "", fmt.Errorf("tamperer: %s", strings.TrimSpace(line))
		}
		return "tamperer started", nil
	},
	// Signals the tamper probe through dir, and reads /lib once it is done.
	"await-tamperer": func(dir string) (string, error) {
		if err := os.WriteFile(filepath.Join(dir, "next_action_running"), nil, 0o644); err != nil {
			return "", err
		}
		if err := waitForFile(filepath.Join(dir, "tamper_result")); err != nil {
			return "", err
		}
		return os.Readlink("/lib")
	},
	"uid": func(string) (string, error) {
		return strconv.Itoa(os.Getuid()), nil
	},
	"home": func(string) (string, error) {
		u, err := user.Current()
		if err != nil {
			return "", err
		}
		return u.HomeDir, nil
	},
	"interfaces": func(string) (string, error) {
		data, err := os.ReadFile("/proc/net/dev")
		if err != nil {
			return "", err
		}
		var ifaces []string
		for _, line := range strings.Split(string(data), "\n") {
			if idx := strings.Index(line, ":"); idx > 0 {
				ifaces = append(ifaces, strings.TrimSpace(line[:idx]))
			}
		}
		return strings.Join(ifaces, "\n"), nil
	},
}

// tamper outlives the action that started it, and waits for the next action
// to run in the same slot. Then, as root in the user namespace, it remounts
// its root read-write and replaces /lib, which is the next action's /lib if
// they share the slot directory. The outcome goes to tamper_result in dir.
func tamper(dir string) int {
	// The working directory keeps dir reachable after the next helper clears
	// the slot, which detaches the mounts in it, /tmp included.
	if err := os.Chdir(dir); err != nil {
		fmt.Printf("error: %v\n", err)
		return 1
	}
	fmt.Println("ready")
	os.Stdout.Close()
	if err := waitForFile("next_action_running"); err != nil {
		return 1
	}
	result := "tampered"
	err := unix.MountSetattr(unix.AT_FDCWD, "/", 0, &unix.MountAttr{Attr_clr: unix.MOUNT_ATTR_RDONLY})
	if err == nil {
		err = os.Remove("/lib")
	}
	if err == nil {
		err = os.Symlink("/etc", "/lib")
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	os.WriteFile("tamper_result", []byte(result), 0o644)
	return 0
}

func waitForFile(path string) error {
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil || time.Now().After(deadline) {
			return err
		}
	}
}

func runProbe(name string, args []string) int {
	if name == "tamper" && len(args) == 1 {
		return tamper(args[0])
	}
	probe, ok := probes[name]
	if !ok || len(args) > 1 {
		fmt.Fprintf(os.Stderr, "usage: probe <name> [arg], got %s %q\n", name, args)
		return 2
	}
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	out, err := probe(arg)
	if err != nil {
		fmt.Printf("error: %v", err)
		return 0
	}
	fmt.Print(out)
	return 0
}
