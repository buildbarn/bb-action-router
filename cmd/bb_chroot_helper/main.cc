/*
 * bb_chroot_helper - run an action in a docker image root.
 *
 * Note: This is primarily tested by the integation test, which can currently only be invoked manually.
 */
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
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <climits>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <thread>

#include "config.h"
#include "mount.h"

namespace fs = std::filesystem;

// In inline mode the action's original input root is nested under this
// directory inside the merged tree (see the merge_docker_root pipeline
// operation in pkg/actionrouter/op_merge.go). We derive the image root
// (everything above it) by splitting the working directory here.
static constexpr const char* kBazelInputRootDir = "bazel_exec_root";

// main is at the bottom so that I don't need to forward-declare all of the functions.
// The bigger functions are laid out in the order in which they're executed, so the
// file can be read top-to-bottom.

static bool should_keep_folder(const Config& config, const std::string& name) {
  return config.keep_list.count(name);
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

// In inline mode there is no fetcher: the action's merged input root is the
// helper's current working directory tree. Derive the image root by splitting
// the CWD at the first kBazelInputRootDir component.
static std::string derive_inline_docker_root() {
  char buf[PATH_MAX];
  if (getcwd(buf, sizeof(buf)) == nullptr) {
    die_errno("getcwd");
  }
  fs::path cwd(buf);
  fs::path acc("/");
  for (const auto& part : cwd.relative_path()) {
    if (part.string() == kBazelInputRootDir) {
      return acc.string();
    }
    acc /= part;
  }
  die(std::string("could not find ") + kBazelInputRootDir + " in working directory " + cwd.string());
}

// We don't want to overwrite /etc/passwd as that would mess with the CAS
// so instead we have the image fetcher manage it and here we just assert
// that there's an entry for the uid/gid that processes inside this user
// namespace.
static void assert_build_user_exists(const std::string& docker_root, int build_uid, int build_gid) {
  std::string passwd_path = docker_root + "/etc/passwd";
  std::ifstream f(passwd_path);
  if (!f) {
    die("cannot read " + passwd_path);
  }
  std::string content((std::istreambuf_iterator<char>(f)), std::istreambuf_iterator<char>());
  std::string needle = ":x:" + std::to_string(build_uid) + ":" + std::to_string(build_gid) + ":";
  if (content.find(needle) == std::string::npos) {
    die(passwd_path + " has no uid " + std::to_string(build_uid) + " gid " + std::to_string(build_gid) +
        " entry (written by the docker root fetcher in sideloaded mode, by merge_docker_root.build_user in inline "
        "mode)");
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

// Bind-mount host /etc files onto the docker root's /etc before mounting
// the image's /etc into the private root. This makes the host's versions
// of /etc/resolv.conf etc. visible to the action.
// This is needed for DNS resolution to work correctly.
static void bind_mount_etc_files(const Config& config, const std::string& docker_root) {
  for (const auto& name : config.etc_files) {
    std::string src = "/etc/" + name;
    std::string dst = docker_root + "/etc/" + name;
    if (!fs::exists(src) || !fs::exists(dst)) {
      continue;
    }
    bind_mount_ro(src, dst, 0);
  }
}

// Bind the image's top-level entries into a private root.
static void mount_image_root(const Config& config, const std::string& docker_root, const std::string& root) {
  std::error_code ec;
  auto it = fs::directory_iterator(docker_root, ec);
  if (ec) {
    die_errno("iterate docker_root " + docker_root);
  }
  for (const auto& entry : it) {
    auto name = entry.path().filename().string();
    if (should_keep_folder(config, name)) {
      continue;
    }

    fs::path src_path = entry.path();
    auto dst = root + "/" + name;
    auto status = fs::symlink_status(src_path, ec);
    if (ec) {
      die("stat " + src_path.string() + ": " + ec.message());
    }
    if (fs::is_symlink(status)) {
      auto target = fs::read_symlink(src_path);
      if (symlink(target.c_str(), dst.c_str()) != 0) {
        die_errno("symlink " + dst);
      }
      continue;
    }

    auto src = src_path.string();
    if (fs::is_directory(status)) {
      if (mkdir(dst.c_str(), 0755) != 0) {
        die_errno("mkdir " + dst);
      }
    } else if (fs::is_regular_file(status)) {
      int fd = open(dst.c_str(), O_WRONLY | O_CREAT | O_EXCL, 0644);
      if (fd < 0) {
        die_errno("create mount point " + dst);
      }
      close(fd);
    } else {
      die("unsupported top-level image entry: " + src);
    }
    bind_mount_ro(src, dst, fs::is_directory(status) ? MS_REC : 0);
  }
}

static void mount_keep_dirs(const Config& config, const std::string& root) {
  for (const auto& name : config.keep_list) {
    std::string src = "/" + name;
    if (!fs::exists(src)) {
      continue;
    }
    std::string dst = root + src;
    if (mkdir(dst.c_str(), 0755) != 0) {
      die_errno("mkdir " + dst);
    }
    if (mount(src.c_str(), dst.c_str(), nullptr, MS_BIND | MS_REC, nullptr) != 0) {
      die_errno("mount --bind " + dst);
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
  Config config;
  int cmd_start = 0;
  std::string error;
  if (!parse_command_line(argc, argv, &config, &cmd_start, &error)) {
    die(error);
  }
  if (!validate_config(config, &error)) {
    die(error);
  }

  if (cmd_start >= argc) {
    fprintf(stderr, "Usage: %s %s\n", argv[0], usage().c_str());
    return 1;
  }

  if (getuid() != 0) {
    die("Must run as root");
  }

  bool inline_mode = config.docker_image_ref.empty();
  std::string docker_root;
  if (inline_mode) {
    docker_root = derive_inline_docker_root();
  } else {
    // Keep the socket fd open — the fetcher releases the root when we close
    // it (on exit).
    docker_root = acquire_docker_root(config.fetcher_socket, config.docker_image_ref);
  }

  assert_build_user_exists(docker_root, config.build_uid, config.build_gid);

  if (mkdir(config.staging_root.c_str(), 0755) != 0 && errno != EEXIST) {
    die_errno("mkdir " + config.staging_root);
  }
  std::error_code ec;
  auto staging_root = fs::canonical(config.staging_root, ec);
  if (ec || staging_root.parent_path() == "/" || !fs::is_directory(staging_root)) {
    die("invalid staging root " + config.staging_root);
  }
  auto root = staging_root.string();
  for (const auto& name : config.keep_list) {
    auto src = "/" + name;
    if (!fs::exists(src)) {
      continue;
    }
    auto kept_root = fs::canonical(src, ec);
    if (ec) {
      die("resolve kept directory " + src + ": " + ec.message());
    }
    // Compare path components so symlinked keep directories cannot expose
    // the staging root, without confusing siblings such as /var/a and /var/ab.
    if (std::mismatch(kept_root.begin(), kept_root.end(), staging_root.begin(), staging_root.end()).first ==
        kept_root.end()) {
      die("staging-root must not be below a kept directory: " + root);
    }
  }
  if (config.root_mode == RootMode::Overlay && chown(root.c_str(), config.host_uid, config.host_gid) != 0) {
    die_errno("chown " + root);
  }

  // Drop to the host user, which is what the build user is mapped to inside the
  // user namespace created below.
  if (setgroups(0, nullptr) != 0) {
    die_errno("setgroups");
  }
  if (setgid(config.host_gid) != 0) {
    die_errno("setgid");
  }
  if (setuid(config.host_uid) != 0) {
    die_errno("setuid");
  }

  // Create mount namespace
  // (we need to clone_newuser, otherwise the syscall fails)
  int unshare_flags = CLONE_NEWUSER | CLONE_NEWNS;
  if (config.isolate_network) {
    unshare_flags |= CLONE_NEWNET;
  }
  if (unshare(unshare_flags) != 0) {
    die_errno("unshare");
  }

  // What this does is to revert the /proc/self ownership to
  // the processes real uid/gid (see proc_pid(5)).
  // Setting the uid/gid maps below won't work without this.
  prctl(PR_SET_DUMPABLE, 1, 0, 0, 0);

  write_file("/proc/self/uid_map", std::to_string(config.build_uid) + " " + std::to_string(config.host_uid) + " 1\n");
  write_file("/proc/self/setgroups", "deny");
  write_file("/proc/self/gid_map", std::to_string(config.build_gid) + " " + std::to_string(config.host_gid) + " 1\n");

  if (config.isolate_network) {
    bring_up_loopback();
  }

  if (mount("", "/", nullptr, MS_PRIVATE | MS_REC, nullptr) != 0) {
    die_errno("mount --make-rprivate /");
  }

  if (config.root_mode == RootMode::Tmpfs) {
    // Each mount namespace gets its own root at the same mount point.
    if (mount("tmpfs", root.c_str(), "tmpfs", MS_NOSUID | MS_NODEV, "mode=0755") != 0) {
      die_errno("mount tmpfs " + root);
    }
  } else {
    // Sequential actions reuse only the empty mount points and symlinks left
    // by the previous action. Never recurse into directories or mounts.
    for (const auto& entry : fs::directory_iterator(root)) {
      if (!fs::remove(entry.path(), ec) || ec) {
        die("remove stale mount point " + entry.path().string() + ": " + ec.message());
      }
    }
    // Give the staging directory its own mount so making it read-only does
    // not change the mount containing the runner or image cache.
    if (mount(root.c_str(), root.c_str(), nullptr, MS_BIND, nullptr) != 0) {
      die_errno("mount --bind " + root);
    }
  }
  mount_keep_dirs(config, root);
  bind_mount_etc_files(config, docker_root);
  mount_image_root(config, docker_root, root);
  if (mark_mount_readonly(root.c_str()) != 0) {
    die_errno("mark readonly " + root);
  }

  char cwd[PATH_MAX];
  if (getcwd(cwd, sizeof(cwd)) == nullptr) {
    die_errno("getcwd");
  }
  if (chroot(root.c_str()) != 0) {
    die_errno("chroot " + root);
  }
  if (chdir(cwd) != 0) {
    die_errno(std::string("chdir ") + cwd);
  }

  run_and_fwd_signals(&argv[cmd_start]);
}
