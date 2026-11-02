package integration

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
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

func runProbe(name string, args []string) int {
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
