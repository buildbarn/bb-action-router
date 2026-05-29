# bb_chroot_helper
This tool is intended to replace `chroot` in unpriviledged containers. It assumes it's the only process it's running
and that it has write access to the container's root. It's recommended to run it in a distroless image with as few
top level directories as possible (for example just `/bin` and `/var`).
It requires a `bb_docker_root_fetcher` socket to be available. It assumes that the docker images don't have symlink
chains longer than 2 hops.

# Why can't we "just" `chroot`?
It may be surprising, but most processes will need `/proc/self/exe` to function correctly. This is for a variety of
reasons, `ld.so` uses `/proc/self/exe` to resolve `$ORIGIN` to find binary-relative .so files (`cc_test` targets will,
by default, link dynamically and use `$ORIGIN` to find all of their `deps`, for example), the JVM also relies on this
mechanism when searching for native libraries to load.

In an unprivileged container, it's possible to `chroot` but it's not possible to "move" `/proc` or `/sys` due to
[locked mounts](https://kinvolk.io/blog/2018/04/towards-unprivileged-container-builds/#what-are-locked-mounts) which
means that using `chroot` will prevent many types of actions from running successfully.

To work around this `bb_chroot_helper` does the oposite - instead of `chroot`-ing and moving `/proc`, we use
`mount --bind` to move all the other folders while `/proc`, `/sys`, etc.. stay in place.

# Overview

Before the helper is invoked, the runner's view of the filesystem is roughly as follows:
 - `/bin`, where the runner and `bb_chroot_helper` are present,
 - `/var/fetcher` where the `bb_docker_root_fetcher` domain socket and caches are present,
 - `/var/runner` where the `bb_worker` intput tree is present,
 - `/proc`, `/sys`, `/dev` and `/tmp` are created by the container runtime.
The runner and helper are static binaries and don't need any libraries in `/lib`.

When invoked, the helper does the following:
 - creates top-level directories to serve as mount points for the docker image (it'll also remove any stale mount points
   that may have been created by a previous action),
 - unshares the mount namespace, so that any subsequent `mount --bind` calls are private to the action,
 - if needed, isolates the network by unsharing the network devices,
 - obtains the directory that corresponds to the docker ref it was passed by the action router,
 - calls a `mount --bind` equivalent syscall to mount the top-level folders from that directory and to place the input
   root in a well-known location,
 - exec's the original action command line.

Since the mount namespace is unshared and private to the process, all bind mounts are cleaned up by the kernel when the
action's process exits.
