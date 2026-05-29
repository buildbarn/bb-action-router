//go:build linux

package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/buildbarn/bb-action-router/pkg/actionrouter"
)

const workerUser = "worker"

// userCredentials holds UID and GID for a user
type userCredentials struct {
	uid int
	gid int
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <command> [args...]\n", os.Args[0])
		os.Exit(1)
	}

	// Look up worker user credentials before the chroot changes /etc/passwd
	creds, err := lookupUser(workerUser)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bb_chroot_helper: %v\n", err)
		os.Exit(1)
	}

	// CWD is <root>/bazel_exec_root[/<working_dir>] (set by action router)
	// We find bazel_exec_root in the path and:
	// - chroot to everything before it
	// - chdir to /bazel_exec_root[/<working_dir>]
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bb_chroot_helper: failed to get working directory: %v\n", err)
		os.Exit(1)
	}
	// Resolve any symlinks in the path.
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bb_chroot_helper: failed to resolve working directory: %v\n", err)
		os.Exit(1)
	}

	// Find bazel_exec_root in path to split into chroot dir and working dir
	chrootDir, workingDir, err := splitChrootPath(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bb_chroot_helper: %v\n", err)
		os.Exit(1)
	}

	if err := run(chrootDir, workingDir, creds, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "bb_chroot_helper: %v\n", err)
		os.Exit(1)
	}
}

// splitChrootPath splits the path around bazel_exec_root.
func splitChrootPath(cwd string) (chrootDir, workingDir string, err error) {
	parts := strings.Split(cwd, string(filepath.Separator))

	for i, part := range parts {
		if part == actionrouter.BazelInputRootDirectoryName {
			chrootDir = strings.Join(parts[:i], string(filepath.Separator))
			if chrootDir == "" {
				chrootDir = "/"
			}
			workingDir = "/" + strings.Join(parts[i:], string(filepath.Separator))
			return chrootDir, workingDir, nil
		}
	}

	return "", "", fmt.Errorf("could not find %s in path %s", actionrouter.BazelInputRootDirectoryName, cwd)
}

func run(chrootDir, workingDir string, creds userCredentials, args []string) error {
	// Lock this goroutine to the current pthread as we're calling into unix APIs.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Create a new mount namespace (also implies CLONE_FS, which makes the effect of chroot private to this process).
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("failed to unshare mount namespace: %w", err)
	}

	// Recursively make all mounts private to prevent propagation to parent namespace.
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("failed to make mounts private: %w", err)
	}

	// Set up mounts for the chroot environment
	if err := setupMounts(chrootDir); err != nil {
		return err
	}

	// Copy host's resolv.conf into the chroot for DNS resolution.
	if err := copyResolvConf(chrootDir); err != nil {
		return err
	}

	// Create the worker user inside the chroot so that user lookups
	// (e.g. tilde expansion) work. Don't use useradd as it may not
	// exist in minimal container images.
	if err := createWorkerUser(chrootDir, creds); err != nil {
		return err
	}

	// Chroot into the directory
	if err := syscall.Chroot(chrootDir); err != nil {
		return fmt.Errorf("failed to chroot to %s: %w", chrootDir, err)
	}
	// Sandbox setup done, launch process as normal.

	if err := os.Chdir(workingDir); err != nil {
		return fmt.Errorf("failed to chdir to %s: %w", workingDir, err)
	}

	if err := dropPrivileges(creds); err != nil {
		return fmt.Errorf("failed to drop privileges: %w", err)
	}

	if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
		return fmt.Errorf("failed to exec %s: %w", args[0], err)
	}

	return nil
}

type mountPoint struct {
	source string
	target string
	fstype string
	flags  uintptr
	mode   os.FileMode
}

func setupMounts(chrootDir string) error {
	// TODO: this is a very generous set of mounts and not secure.
	mounts := []mountPoint{
		// TODO: at least block /proc/kcore
		{source: "proc", target: "proc", fstype: "proc", flags: unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV, mode: 0o755},
		{source: "sysfs", target: "sys", fstype: "sysfs", flags: unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV, mode: 0o755},
		// TODO: we probably only want null, zero, random, urandom and tty.
		{source: "/dev", target: "dev", fstype: "", flags: unix.MS_BIND | unix.MS_REC, mode: 0o755},
		{source: "tmpfs", target: "tmp", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, mode: 0o777},
	}

	for _, m := range mounts {
		targetPath := filepath.Join(chrootDir, m.target)

		if err := os.MkdirAll(targetPath, m.mode); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to create /%s mount point: %w", m.target, err)
		}

		if err := unix.Mount(m.source, targetPath, m.fstype, m.flags, ""); err != nil {
			return fmt.Errorf("failed to mount /%s: %w", m.target, err)
		}
	}

	return nil
}

func appendToFile(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fileContainsUser checks if a passwd or group file already has an entry
// for the given username (matching the first colon-delimited field).
func fileContainsUser(path, username string) (bool, error) {
	// TODO: the check here only works by name, we should also make sure we're not inserting duplicate UIDs.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	prefix := username + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true, nil
		}
	}
	return false, nil
}

func createWorkerUser(chrootDir string, creds userCredentials) error {
	passwdPath := filepath.Join(chrootDir, "etc/passwd")
	if exists, err := fileContainsUser(passwdPath, workerUser); err != nil {
		return fmt.Errorf("failed to check /etc/passwd for worker user: %w", err)
	} else if !exists {
		passwdLine := fmt.Sprintf("%s:x:%d:%d::/tmp:/bin/sh\n", workerUser, creds.uid, creds.gid)
		if err := appendToFile(passwdPath, passwdLine); err != nil {
			return fmt.Errorf("failed to append worker user to /etc/passwd: %w", err)
		}
	}

	groupPath := filepath.Join(chrootDir, "etc/group")
	if exists, err := fileContainsUser(groupPath, workerUser); err != nil {
		return fmt.Errorf("failed to check /etc/group for worker group: %w", err)
	} else if !exists {
		groupLine := fmt.Sprintf("%s:x:%d:\n", workerUser, creds.gid)
		if err := appendToFile(groupPath, groupLine); err != nil {
			return fmt.Errorf("failed to append worker group to /etc/group: %w", err)
		}
	}

	return nil
}

func copyResolvConf(chrootDir string) error {
	const resolvConf = "etc/resolv.conf"
	src := filepath.Join("/", resolvConf)

	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", src, err)
	}

	dst := filepath.Join(chrootDir, resolvConf)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(dst), err)
	}

	// The destination may be a hardlink into the worker cache (which is not something
	// we should be overwriting) so unlink first.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing %s: %w", dst, err)
	}

	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", dst, err)
	}

	return nil
}

func lookupUser(username string) (userCredentials, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return userCredentials{}, fmt.Errorf("failed to lookup user %s: %w", username, err)
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return userCredentials{}, fmt.Errorf("failed to parse uid: %w", err)
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return userCredentials{}, fmt.Errorf("failed to parse gid: %w", err)
	}

	return userCredentials{uid: uid, gid: gid}, nil
}

func dropPrivileges(creds userCredentials) error {
	// Set groups, gid, then uid (order matters)
	if err := syscall.Setgroups([]int{creds.gid}); err != nil {
		return fmt.Errorf("failed to setgroups: %w", err)
	}

	if err := syscall.Setgid(creds.gid); err != nil {
		return fmt.Errorf("failed to setgid: %w", err)
	}

	if err := syscall.Setuid(creds.uid); err != nil {
		return fmt.Errorf("failed to setuid: %w", err)
	}

	return nil
}
