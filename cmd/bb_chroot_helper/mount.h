#pragma once

// Mark the mount at `path` as read-only (per-mount).
//
// Prefers mount_setattr(2) (Linux 5.12+) Falls back to
// mount(MS_REMOUNT|MS_BIND|MS_RDONLY) when mount_setattr returns ENOSYS (older
// kernels).
//
// Returns 0 on success, -1 on failure with errno set.
int mark_mount_readonly(const char* path);
