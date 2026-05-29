// Mark a mount read-only, preferring mount_setattr(2) (Linux 5.12+) and
// falling back to mount(MS_REMOUNT|MS_BIND|MS_RDONLY) when the syscall is
// unavailable.
#include "mount.h"

#include <errno.h>
#include <fcntl.h>  // AT_FDCWD
#include <stdint.h>
#include <sys/mount.h>    // MS_BIND, MS_REMOUNT, MS_RDONLY, ...
#include <sys/statvfs.h>  // statvfs, ST_*
#include <sys/syscall.h>  // SYS_mount_setattr
#include <unistd.h>       // syscall

#ifndef MOUNT_ATTR_RDONLY
// The kernel UAPI symbols we replicate
// below (struct mount_attr, MOUNT_ATTR_RDONLY) come from
// https://github.com/torvalds/linux/blob/v5.14/include/uapi/linux/mount.h
// — syscall ABI, stable. musl's <sys/mount.h> does not define them.
#define MOUNT_ATTR_RDONLY 0x00000001
struct mount_attr {
  uint64_t attr_set;
  uint64_t attr_clr;
  uint64_t propagation;
  uint64_t userns_fd;
};
#endif

#ifndef SYS_mount_setattr
#define SYS_mount_setattr 442
#endif

static int try_mount_setattr_ro(const char* path) {
  struct mount_attr attr = {};
  attr.attr_set = MOUNT_ATTR_RDONLY;
  return syscall(SYS_mount_setattr, AT_FDCWD, path, 0, &attr, sizeof(attr));
}

// Legacy MS_REMOUNT path.
static int remount_ro(const char* path) {
  struct statvfs sv;
  if (statvfs(path, &sv) != 0) {
    return -1;
  }
  // We need to preserve the flags, otherwise we might get a permission error
  // (we're allowed to remount as read-only but we're not allowed to change flags).
  // Unfortunately statvfs doesn't use the same values for the flags as mount, so we
  // need to translate them back.
  unsigned long preserved = 0;
  static constexpr struct {
    unsigned long st;
    unsigned long ms;
  } kFlagMap[] = {
      {ST_NOSUID, MS_NOSUID},   {ST_NODEV, MS_NODEV},           {ST_NOEXEC, MS_NOEXEC},
      {ST_NOATIME, MS_NOATIME}, {ST_NODIRATIME, MS_NODIRATIME}, {ST_RELATIME, MS_RELATIME},
  };
  for (const auto& m : kFlagMap) {
    if (sv.f_flag & m.st) {
      preserved |= m.ms;
    }
  }
  return mount("", path, nullptr, MS_REMOUNT | MS_BIND | MS_RDONLY | preserved, nullptr);
}

int mark_mount_readonly(const char* path) {
  if (try_mount_setattr_ro(path) == 0) {
    return 0;
  }
  if (errno != ENOSYS) {
    return -1;
  }
  return remount_ro(path);
}
