/*
 * bb_chroot_helper - overlay docker image directories onto host root.
 *
 * Note: This is primarily tested by the integation test, which can currently only be invoked manually.
 */
#include <dirent.h>
#include <fcntl.h>
#include <grp.h>
#include <net/if.h>
#include <poll.h>
#include <sched.h>
#include <signal.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <sys/xattr.h>
#include <unistd.h>

#include <cerrno>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <memory>
#include <set>
#include <string>
#include <thread>
#include <unordered_set>
#include <vector>

#include "mount.h"

namespace fs = std::filesystem;

// We drop down to a non-root user to run actions.
constexpr int kBuildUid = 1000;
constexpr int kBuildGid = 1000;

// These are mounted by the container runtime.
static const std::set<std::string> kKeepList = {"proc", "sys", "dev", "tmp", "runner"};
static const std::vector<std::string> kEtcFiles = {"resolv.conf", "hostname", "hosts"};

// Xattr we set on every top-level entry we create in / so that the next run
// can tell what to delete
// Uses the user.* namespace so no special permissions are required.
static constexpr const char* kOwnedXattr = "user.bb_chroot_helper.owned";

// main is at the bottom so that I don't need to forward-declare all of the functions.
// The bigger functions are laid out in the order in which they're executed, so the
// file can be read top-to-bottom.

static bool should_keep_folder(const std::string& name) {
  return kKeepList.count(name);
}

[[noreturn]] static void die(const std::string& msg) {
  fprintf(stderr, "bb_chroot_helper fatal: %s\n", msg.c_str());
  // If we fail mid-way through an unshare call it's safer to skip some of the
  // cleanup (flushing buffers, atexit). The print above is unbuffered, so it'll
  // show up in stderr either way.
  _exit(1);
}

[[noreturn]] static void die_errno(const std::string& msg) {
  fprintf(stderr, "bb_chroot_helper: %s: %s\n", msg.c_str(), strerror(errno));
  _exit(1);
}

// Connect to the fetcher socket and wait for the server greeting.
// Returns the connected fd, or -1 if the greeting wasn't received
// within timeout_ms. The original error code (rather than the exit
// code from `close()`) is set to out_errno.
static int connect_to_fetcher(const std::string& socket_path, int timeout_ms, int* out_errno) {
  int fd = ::socket(AF_UNIX, SOCK_STREAM, 0);
  if (fd < 0) {
    die_errno("socket(AF_UNIX)");
  }

  struct sockaddr_un addr = {};
  addr.sun_family = AF_UNIX;
  if (socket_path.size() >= sizeof(addr.sun_path)) {
    die("fetcher socket path too long: " + socket_path);
  }
  std::copy(socket_path.begin(), socket_path.end(), addr.sun_path);

  if (connect(fd, reinterpret_cast<struct sockaddr*>(&addr), sizeof(addr)) != 0) {
    if (out_errno) {
      *out_errno = errno;
    }
    close(fd);
    return -1;
  }

  // Wait for the server to send something.
  struct pollfd pfd = {fd, POLLIN, 0};
  if (poll(&pfd, 1, timeout_ms) <= 0) {
    if (out_errno) {
      *out_errno = ETIMEDOUT;
    }
    close(fd);
    return -1;
  }

  // Make sure the first message was "HI\n", otherwise
  // the server is borked.
  char buf[4];
  ssize_t n = read(fd, buf, sizeof(buf));
  if (n < 3 || buf[0] != 'H' || buf[1] != 'I' || buf[2] != '\n') {
    if (out_errno) {
      *out_errno = (n < 0) ? errno : EPROTO;
    }
    close(fd);
    return -1;
  }
  return fd;
}

// Connect to the fetcher. We do some retries to allow for the fetcher coming up after the runner.
static int connect_to_fetcher_with_retry(const std::string& socket_path) {
  int last_errno = 0;
  for (int i = 0; i < 15; ++i) {
    int fd = connect_to_fetcher(socket_path, 500, &last_errno);
    if (fd >= 0) {
      return fd;
    }
    std::this_thread::sleep_for(std::chrono::seconds(1));
  }
  errno = last_errno;
  die_errno("connect to fetcher at " + socket_path + " after 30s");
}

// Acquire a materialized docker root from the docker root fetcher.
static std::string acquire_docker_root(const std::string& socket_path, const std::string& image_ref) {
  int fd = connect_to_fetcher_with_retry(socket_path);

  std::string request = "ACQUIRE " + image_ref + "\n";
  if (write(fd, request.data(), request.size()) != static_cast<ssize_t>(request.size())) {
    die_errno("write to fetcher");
  }

  std::string response;
  char ch;
  while (read(fd, &ch, 1) == 1 && ch != '\n') {
    response += ch;
    if (response.length() > 16 * 1024) {
      die("fetcher malformed response (longer than 16k chars)");
    }
  }

  if (response.rfind("OK ", 0) == 0) {
    int flags = fcntl(fd, F_GETFD);
    // The fd is set CLOEXEC so child processes (the action) don't inherit it.
    if (flags < 0 || fcntl(fd, F_SETFD, flags | FD_CLOEXEC) != 0) {
      die_errno("fcntl FD_CLOEXEC on fetcher socket");
    }
    // We intentionally leak the FD and rely on the kernel closing it.
    // That's how the fetcher knows the action is done.
    return response.substr(3);
  }
  close(fd);
  if (response.rfind("ERROR ", 0) == 0) {
    die("fetcher: " + response.substr(6));
  }
  die("unexpected fetcher response: " + response);
}

// We don't have a cleanup when the process exits, so each time we start
// the helper, we try to remove any toplevel dirs/files that might have been
// created by the previous action.
static void clean_stale_files_from_root() {
  std::error_code ec;
  for (const auto& entry : fs::directory_iterator("/", ec)) {
    auto name = entry.path().filename().string();
    if (should_keep_folder(name)) {
      continue;
    }

    char buf[1];
    // lgetxattr operates on the link itself, not the target.
    if (lgetxattr(entry.path().c_str(), kOwnedXattr, buf, sizeof(buf)) < 0) {
      continue;
    }

    if (entry.is_directory(ec)) {
      if (rmdir(entry.path().c_str())) {
        die_errno("removing stale directory " + entry.path().string());
      }
    } else {
      if (unlink(entry.path().c_str())) {
        die_errno("removing stale file" + entry.path().string());
      }
    }
  }
}

// Resolve a symlink within docker_root to its target's absolute path,
// rewriting absolute targets to stay rooted at docker_root so they
// don't escape to the host filesystem. Only one hop of resolution is
// performed; we die if the target is itself a symlink, since a chain
// could otherwise be used to escape (an absolute symlink in the chain
// would be followed by the kernel against the real filesystem). In
// practice, docker images use at most one hop for entries like
// /bin -> usr/bin.
static fs::path resolve_symlink_within(const fs::path& docker_root, const fs::path& link) {
  auto target = fs::read_symlink(link);
  fs::path resolved;
  if (target.is_absolute()) {
    resolved = docker_root / target.relative_path();
  } else {
    resolved = link.parent_path() / target;
  }
  resolved = resolved.lexically_normal();

  // Reject targets that try to escape docker_root via ".."
  // (for example "../../etc").
  auto root_str = docker_root.lexically_normal().string();
  auto resolved_str = resolved.string();
  if (resolved_str != root_str && resolved_str.rfind(root_str + "/", 0) != 0) {
    die("symlink target escapes docker_root: " + link.string() + " -> " + target.string() + " (resolved to " +
        resolved_str + ")");
  }

  std::error_code ec;
  if (fs::is_symlink(fs::symlink_status(resolved, ec))) {
    die("symlink chain not supported: " + link.string() + " -> " + target.string() + " -> (another symlink)");
  }
  return resolved;
}

// This prepares the root dir for the subsequent `mount --bind` calls.  We need
// empty dirs/files to be in place to act as mount points.  This needs to be
// done while we're still root, hence the separate function.
//
// Every entry we create gets tagged with kOwnedXattr so clean_stale_files_from_root
// on the next run knows what to remove. Entries that already exist (base image)
// are left untagged and untouched.
static void prepare_root(const std::string& docker_root) {
  std::error_code ec;
  for (const auto& entry : fs::directory_iterator(docker_root, ec)) {
    auto name = entry.path().filename().string();
    if (should_keep_folder(name)) {
      continue;
    }

    std::string dst = "/" + name;

    // status() follows symlinks — a docker_root /bin -> usr/bin symlink
    // is treated as a directory and not a file. This is mostly because
    // /bin and /var are directories in the chroot runner image and so it's
    // not possible to replace them with files that are symlinks.
    // We could special case just these two, but well-formed actions outputs
    // shouldn't depend on whether /bin is a "real" directory or a symlink.
    auto status = fs::status(entry.path(), ec);

    if (fs::is_directory(status)) {
      if (!fs::exists(fs::symlink_status(dst))) {
        if (mkdir(dst.c_str(), 0755) != 0 && errno != EEXIST) {
          die_errno("mkdir " + dst);
        }
        if (lsetxattr(dst.c_str(), kOwnedXattr, "1", 1, 0) != 0) {
          die_errno("setxattr " + dst);
        }
      }
    } else if (fs::is_regular_file(status)) {
      // It's possible to `mount --bind` a single file as long as there exists
      // a target file to act as the mount point.
      if (!fs::exists(fs::symlink_status(dst))) {
        int fd = open(dst.c_str(), O_WRONLY | O_CREAT, 0644);
        if (fd < 0) {
          die_errno("create mount point " + dst);
        }
        close(fd);
        if (lsetxattr(dst.c_str(), kOwnedXattr, "1", 1, 0) != 0) {
          die_errno("setxattr " + dst);
        }
      }
    }
  }
}

// We don't want to overwrite /etc/passwd as that would mess with the CAS
// so instead we have the image fetcher manage it and here we just assert
// that there's an entry for the uid that processes inside this user
// namespace see (uid 0).
static void assert_build_user_exists(const std::string& docker_root) {
  std::string passwd_path = docker_root + "/etc/passwd";
  std::ifstream f(passwd_path);
  if (!f) {
    die("cannot read " + passwd_path);
  }
  std::string content((std::istreambuf_iterator<char>(f)), std::istreambuf_iterator<char>());
  if (content.find(":x:0:0:") == std::string::npos) {
    die(passwd_path + " has no uid 0 entry (should be set by the docker root fetcher)");
  }
}

// Bind-mount src at dst, then mark the mount read-only.
static void bind_mount_ro(const std::string& src, const std::string& dst, unsigned long extra_flags) {
  if (mount(src.c_str(), dst.c_str(), nullptr, MS_BIND | extra_flags, nullptr) != 0) {
    die_errno("mount --bind " + dst);
  }
  if (mark_mount_readonly(dst.c_str()) != 0) {
    die_errno("mark readonly " + dst);
  }
}

// Bind-mount host /etc files onto the docker root's /etc before
// overlay. When overlay later mounts docker_root/etc onto /etc,
// these inner bind mounts propagate through, so /etc/resolv.conf
// etc. show the host's versions.
// This is needed for DNS resolution to work correctly.
static void bind_mount_etc_files(const std::string& docker_root) {
  for (const auto& name : kEtcFiles) {
    std::string src = "/etc/" + name;
    std::string dst = docker_root + "/etc/" + name;
    if (!fs::exists(src) || !fs::exists(dst)) {
      continue;
    }
    bind_mount_ro(src, dst, 0);
  }
}

// Overlay docker_root onto / and hide runner top-level dirs that don't
// appear in docker_root.
static void overlay_root_dirs(const std::string& docker_root) {
  struct Overlay {
    std::string src;
    std::string dst;
    unsigned long extra_flags;
  };
  std::unique_ptr<Overlay> deferred_mount;
  std::unordered_set<std::string> bind_mounts;

  // Derive the top-level directory that contains docker_root (e.g. "var"
  // for /var/run/fetcher/...). That one has to be mounted last.
  std::string deferred_name;
  for (const auto& part : fs::path(docker_root)) {
    std::string s = part.string();
    if (s.empty() || s == "/") {
      continue;
    }
    deferred_name = s;
    break;
  }
  if (deferred_name.empty()) {
    die("cannot derive top-level dir from docker_root: " + docker_root);
  }

  std::error_code ec;
  auto it = fs::directory_iterator(docker_root, ec);
  if (ec) {
    die_errno("iterate docker_root " + docker_root);
  }
  for (const auto& entry : it) {
    auto name = entry.path().filename().string();
    if (should_keep_folder(name)) {
      continue;
    }

    fs::path src_path = entry.path();
    if (fs::is_symlink(fs::symlink_status(src_path))) {
      src_path = resolve_symlink_within(docker_root, src_path);
    }
    auto status = fs::status(src_path, ec);
    if (ec) {
      die_errno("iterate within docker_root " + src_path.string());
    }

    auto src = src_path.string();
    auto dst = "/" + name;
    unsigned long extra_flags = fs::is_directory(status) ? MS_REC : 0;
    if (name == deferred_name) {
      // All the other bind-mounts point into here, so it must be bind-mounted over last.
      deferred_mount = std::unique_ptr<Overlay>(new Overlay{src, dst, extra_flags});
    } else {
      bind_mounts.insert(name);
      bind_mount_ro(src, dst, extra_flags);
    }
  }

  if (deferred_mount) {
    auto dst = "/" + deferred_name;
    bind_mount_ro(deferred_mount->src, dst, deferred_mount->extra_flags);
  } else {
    // We expect the fetcher to ensure that this directory exists in the materialized
    // root. It not being there is likely a config error.
    die("docker_root parent '" + deferred_name + "' has no matching entry; expected fetcher to create it");
  }

  // If anything is left over attempt to hide it with a tmpfs mount.
  for (const auto& entry : fs::directory_iterator("/", ec)) {
    auto name = entry.path().filename().string();
    if (should_keep_folder(name)) {
      continue;
    }
    if (bind_mounts.count(name)) {
      continue;
    }
    if (name == deferred_name) {
      continue;
    }
    auto dst = entry.path();
    if (mount("tmpfs", dst.c_str(), "tmpfs", MS_NOSUID | MS_NODEV, "size=0")) {
      die_errno("mount tmpfs " + dst.string());
    }
  }
}

// If we're in network isolated mode, then the loopback interface starts
// as being down and we need to bring it up.
static void bring_up_loopback() {
  int sock = socket(AF_INET, SOCK_DGRAM, 0);
  if (sock < 0) {
    die_errno("socket(AF_INET)");
  }
  struct ifreq ifr = {};
  strncpy(ifr.ifr_name, "lo", IFNAMSIZ);
  ifr.ifr_flags = IFF_UP | IFF_RUNNING;
  if (ioctl(sock, SIOCSIFFLAGS, &ifr) != 0) {
    die_errno("bringing lo up");
  }
  close(sock);
}

static void write_file(const std::string& path, const std::string& data) {
  std::ofstream f(path);
  if (!f) {
    die_errno("open " + path);
  }
  f << data;
  if (!f) {
    die_errno("write " + path);
  }
}

// The signals we forward to the action.
static constexpr int kForwardedSignals[] = {SIGTERM, SIGINT, SIGHUP, SIGQUIT, SIGUSR1, SIGUSR2};

// Set by the parent after fork so the signal handler can forward to the child.
// sig_atomic_t is the type guaranteed by POSIX to be safe for signal handlers.
static volatile sig_atomic_t g_child_pid = 0;

static void forward_signal(int sig) {
  pid_t p = g_child_pid;
  if (p > 0) {
    kill(p, sig);
  }
}

static void install_signal_forwarders() {
  struct sigaction sa = {};
  sa.sa_handler = forward_signal;
  sigemptyset(&sa.sa_mask);
  sa.sa_flags = SA_RESTART;
  for (int sig : kForwardedSignals) {
    sigaction(sig, &sa, nullptr);
  }
}

// Fork, run action in child, wait in parent, exit with matching status while
// forwarding signals to child.
[[noreturn]] static void run_and_fwd_signals(char** argv) {
  // Block the forwarded signals around fork() so that signals don't get lost
  // before we've installed a handler.
  sigset_t block_set, old_set;
  sigemptyset(&block_set);
  for (int sig : kForwardedSignals) {
    sigaddset(&block_set, sig);
  }
  sigprocmask(SIG_BLOCK, &block_set, &old_set);

  pid_t pid = fork();
  if (pid < 0) {
    die_errno("fork");
  }

  if (pid == 0) {
    // Child: restore the signal mask and exec the action.
    sigprocmask(SIG_SETMASK, &old_set, nullptr);
    execvp(argv[0], argv);
    die_errno(std::string("exec ") + argv[0]);
  }

  g_child_pid = pid;
  install_signal_forwarders();
  // Any signals delivered while blocked are queued and handled once we unblock.
  sigprocmask(SIG_SETMASK, &old_set, nullptr);

  int status = 0;
  while (true) {
    pid_t r = waitpid(pid, &status, 0);
    if (r == pid) {
      break;
    }
    if (r < 0 && errno == EINTR) {
      continue;
    }
    die_errno("waitpid");
  }

  if (WIFEXITED(status)) {
    _exit(WEXITSTATUS(status));
  }
  if (WIFSIGNALED(status)) {
    _exit(128 + WTERMSIG(status));
  }
  _exit(1);
}

// Main entrypoint.
int main(int argc, char** argv) {
  std::string docker_image_ref;
  std::string fetcher_socket = "/var/run/fetcher/fetcher.sock";
  bool isolate_network = false;
  int cmd_start = 1;

  for (int i = 1; i < argc; i++) {
    std::string arg(argv[i]);
    std::string prefix;

    prefix = "--docker-image-ref=";
    if (arg.rfind(prefix, 0) == 0) {
      docker_image_ref = arg.substr(prefix.size());
    } else if ((prefix = "--fetcher-socket="), arg.rfind(prefix, 0) == 0) {
      fetcher_socket = arg.substr(prefix.size());
    } else if (arg == "--network-isolation") {
      isolate_network = true;
    } else if (arg == "--no-network-isolation") {
      isolate_network = false;
    } else {
      break;
    }
    cmd_start = i + 1;
  }

  if (docker_image_ref.empty() || cmd_start >= argc) {
    fprintf(stderr,
            "Usage: %s --docker-image-ref=REF "
            "[--fetcher-socket=PATH] "
            "[--network-isolation|--no-network-isolation] "
            "<command> [args...]\n",
            argv[0]);
    return 1;
  }

  if (getuid() != 0) {
    die("Must run as root");
  }

  // Get docker root from fetcher. Keep the socket fd open — the fetcher
  // releases the root when we close it (on exit).
  auto docker_root = acquire_docker_root(fetcher_socket, docker_image_ref);

  // (as root) prepare filesystem
  assert_build_user_exists(docker_root);
  clean_stale_files_from_root();
  prepare_root(docker_root);

  // Drop to build user
  if (setgroups(0, nullptr) != 0) {
    die_errno("setgroups");
  }
  if (setgid(kBuildGid) != 0) {
    die_errno("setgid");
  }
  if (setuid(kBuildUid) != 0) {
    die_errno("setuid");
  }

  // Create mount namespace
  // (we need to clone_newuser, otherwise the syscall fails)
  int unshare_flags = CLONE_NEWUSER | CLONE_NEWNS;
  if (isolate_network) {
    unshare_flags |= CLONE_NEWNET;
  }
  if (unshare(unshare_flags) != 0) {
    die_errno("unshare");
  }

  // What this does is to revert the /proc/self ownership to
  // the processes real uid/gid (see proc_pid(5)).
  // Setting the uid/gid maps below won't work without this.
  prctl(PR_SET_DUMPABLE, 1, 0, 0, 0);

  write_file("/proc/self/uid_map", "0 " + std::to_string(kBuildUid) + " 1\n");
  write_file("/proc/self/setgroups", "deny");
  write_file("/proc/self/gid_map", "0 " + std::to_string(kBuildGid) + " 1\n");

  if (isolate_network) {
    bring_up_loopback();
  }

  if (mount("", "/", nullptr, MS_PRIVATE | MS_REC, nullptr) != 0) {
    die_errno("mount --make-rprivate /");
  }

  // Do the fake chroot.
  bind_mount_etc_files(docker_root);
  overlay_root_dirs(docker_root);

  run_and_fwd_signals(&argv[cmd_start]);
}
