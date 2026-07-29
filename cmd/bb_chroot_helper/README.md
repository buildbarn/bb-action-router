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
 - `/runner` where the `bb_worker` build directory (and so the action's input tree) is present,
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

# Configuration

The helper is invoked as `bb_chroot_helper [flags...] -- <command> [args...]`. The `--` is mandatory, so that the
client-controlled command can't be read as helper flags.

Settings come from three places, each overriding the previous one: the built-in defaults, the config file named by
`--config=PATH`, and the remaining flags.

Only the per-action settings are meant to be flags, since those are what the action router templates from platform
properties:

 - `--docker-image-ref=REF` selects sideloaded mode (an empty or absent value means inline mode). It deliberately has no
   config file equivalent,
 - `--network-isolation` / `--no-network-isolation`.

Everything else belongs in the config file, which is TOML. A setting is named the same way as the flag it corresponds
to, minus the leading dashes:

```toml
# Path of the bb_docker_root_fetcher socket. Unused in inline mode.
fetcher-socket = "/var/run/fetcher/fetcher.sock"

# Default when the router passes neither --network-isolation nor
# --no-network-isolation.
network-isolation = false

# Extra top-level entries of / to leave alone (see the warning below). Added to
# the built-in list of container runtime mounts (proc, sys, dev, tmp). The
# top-level directory holding the worker's build directory belongs here.
keep-dirs = ["runner", "nix"]

# Host /etc files bind-mounted into the image's /etc, so the action sees the
# host's version. Replaces the built-in list, which is what's shown here.
etc-files = ["resolv.conf", "hostname", "hosts"]

# uid/gid the action runs as inside the helper's user namespace. Must match the
# build user in the image's /etc/passwd, which in sideloaded mode is written by
# the docker root fetcher (its build_user must therefore agree with this).
[build-user]
uid = 0
gid = 0

# Host-side uid/gid the helper setuid()s to before unsharing. Must be an id the
# runner container is allowed to switch to, and must not be 0.
[host-user]
uid = 1000
gid = 1000
```

Every key is optional, but an unknown key, a wrong type, a partially specified `[build-user]`/`[host-user]` and a
`--config` path that can't be read are all fatal.

`keep-dirs` needs care: entries on the keep list are not cleaned up between actions, not used as overlay mount points
and not hidden behind a tmpfs, so the runner's version of the directory stays visible and writable inside the sandbox.

It is, however, mandatory for one entry: the top-level directory that contains the worker's build directory, since the
action's working directory lives in there. In sideloaded mode the helper would otherwise mount an empty tmpfs over it
and the action would start with no working directory. That directory therefore has to be one the docker image doesn't
also provide — with `buildDirectoryPath: /runner/build` the entry is `runner`, and a build directory under `/var` won't
work at all, since `/var` is where the materialized image roots live.

The path passed to `--config` is resolved against the helper's working directory, which in inline mode is inside the
action's input root. Use an absolute path.
