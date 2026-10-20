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

Settings come from three places, each overriding the previous one: the built-in defaults, the TOML config file named by
`--config=PATH`, and the remaining flags. A flag overrides the file regardless of where it appears relative to
`--config`. `bb_chroot_helper --` with no command prints the full list of options.

Every option is available in both places under the same name: `--name=VALUE` on the command line is `name = VALUE` in
the config file. List options take a comma separated or repeated flag (`--keep-dirs=runner,nix`) or a TOML array, and a
list given as a flag replaces the file's list. There are two exceptions: `--config` itself, and `--docker-image-ref`,
which selects sideloaded mode (an empty or absent value means inline mode) and is per-action, so it's rejected in the
config file.

An empty value must be passed as a separate argument (`--docker-image-ref ""`). The `--name=` form is rejected, because
the parser would otherwise take the next flag as the value.

Only the per-action settings are meant to be flags, since those are what the action router templates from platform
properties: `--docker-image-ref` and `--network-isolation` / `--no-network-isolation`. The rest normally lives in the
config file:

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

# UID:GID the action runs as inside the helper's user namespace. Must match the
# build user in the image's /etc/passwd, which in sideloaded mode is written by
# the docker root fetcher (its build_user must therefore agree with this).
build-user = "0:0"

# Host-side UID:GID the helper setuid()s to before unsharing. Must be an id the
# runner container is allowed to switch to, and must not be 0.
host-user = "1000:1000"
```

Every key is optional, but an unknown key, an invalid value and a `--config` path that can't be read are all fatal.
The file is parsed by [CLI11](https://github.com/CLIUtils/CLI11), whose TOML support is a subset: values are read as
text and checked by the option they're assigned to, so for instance `fetcher-socket = 7` is accepted as the path `7`.
Likewise, a scalar and a one-element array are indistinguishable: `keep-dirs = "nix"` is the list `["nix"]`, and
`fetcher-socket = ["/a.sock"]` is the path `/a.sock`. Giving a single-valued option more than one value, be it a
longer array, a repeated key or a repeated flag, is an error.

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
