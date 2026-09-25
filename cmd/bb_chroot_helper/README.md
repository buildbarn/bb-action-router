# bb_chroot_helper
This tool runs actions in docker image roots in unprivileged containers. With `root-mode = "tmpfs"`, it supports concurrent
actions by mounting a separate root for each action. The staging mount point defaults to `/var/action_root`.
Sideloaded mode requires a `bb_docker_root_fetcher` socket to be available.

The default `root-mode = "overlay"` clears and reuses the staging directory, requiring one action at a time per runner.

# Keeping `/proc` available
It may be surprising, but most processes will need `/proc/self/exe` to function correctly. This is for a variety of
reasons, `ld.so` uses `/proc/self/exe` to resolve `$ORIGIN` to find binary-relative .so files (`cc_test` targets will,
by default, link dynamically and use `$ORIGIN` to find all of their `deps`, for example), the JVM also relies on this
mechanism when searching for native libraries to load.

In an unprivileged container, a plain bind mount of `/proc` into a new root can fail because of
[locked mounts](https://kinvolk.io/blog/2018/04/towards-unprivileged-container-builds/#what-are-locked-mounts)
under `/proc`. A recursive bind copies these mounts as well, allowing `/proc` to remain available after `chroot`.

# Overview

Before the helper is invoked, the runner's view of the filesystem is roughly as follows:
 - `/bin`, where the runner and `bb_chroot_helper` are present,
 - `/var/fetcher` where the `bb_docker_root_fetcher` domain socket and caches are present,
 - `/runner` where the `bb_worker` build directory (and so the action's input tree) is present,
 - `/proc`, `/sys`, `/dev` and `/tmp` are created by the container runtime.
The runner and helper are static binaries and don't need any libraries in `/lib`.

When invoked, the helper does the following:
 - obtains the image root, from the fetcher in sideloaded mode,
 - creates the staging mount point at `staging-root` if needed,
 - unshares the user and mount namespaces, so that subsequent mounts are private to the action,
 - if needed, isolates the network by unsharing the network devices,
 - mounts tmpfs or clears and self-bind-mounts the staging directory, depending on `root-mode`,
 - bind-mounts the keep directories and image's top-level entries into it,
 - enters the new root with `chroot` and runs the original action command line.

The bind mounts are cleaned up by the kernel when the helper's mount namespace exits.

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

# Filesystem setup: overlay (default, serialized actions) or tmpfs (concurrent actions).
root-mode = "overlay"

# Dedicated staging directory, cleared in overlay mode. Its parent must exist and be
# writable if the mount point does not. The path must be normalized, absolute,
# and below a top-level directory.
staging-root = "/var/action_root"

# Default when the router passes neither --network-isolation nor
# --no-network-isolation.
network-isolation = false

# Extra top-level directories of / to bind into the action root (see the warning below). Added to
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

`keep-dirs` needs care: entries on the keep list are bind-mounted from the runner, so the runner's version of the
directory stays visible and writable inside the sandbox.
`staging-root` must not be inside or equal to a kept directory after resolving symlinks, as this would expose
the staging directory inside the action.

It is, however, mandatory for one entry: the top-level directory that contains the worker's build directory, since the
action's working directory lives in there. In sideloaded mode the helper would otherwise omit it from the new root
and the action would start with no working directory. That directory therefore has to be one the docker image doesn't
also provide — with `buildDirectoryPath: /runner/build` the entry is `runner`, and a build directory under `/var` won't
work at all, since `/var` is where the materialized image roots live.

The path passed to `--config` is resolved against the helper's working directory, which in inline mode is inside the
action's input root. Use an absolute path.
