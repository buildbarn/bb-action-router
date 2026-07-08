# bb-action-router
The tools in this repository make it possible for a Buildbarn remote execution deployment
to execute actions inside of user-provided containers.

There are two main components: the action router, a service that plugs into 
the [remote execution scheduler](https://raw.githubusercontent.com/buildbarn/bb-remote-execution)
via the [ActionRouter](https://github.com/buildbarn/bb-remote-execution/blob/main/pkg/proto/remoteactionrouter/remoteactionrouter.proto)
API, and a helper executable, which is responsible for running the action in the user-provided
sandbox. At a high level, the action router modifies the incoming action so that it invokes
the helper, with the docker image ref as an argument and, once the action reaches the runner,
the helper emulates running the actions original command inside of the specified container.

# Caveats

Unlike, say, Kubernetes, Buildbarn is not a generic execution environment, which allows us to
make some simplifications. This implementation ignores most attributes of the user-specified
container (entrypoint, filesystem permissions) and assumes that the container is a sort of auxiliary
input to the action. This allows us to provide the functionality in environments where "true"
docker-in-docker is not possible, but it does mean the resulting environment is not exactly
the same as one provided by a full container runtime.

# Overview

There are two main modes of operation: inline and sideloaded. In inline mode the action router
will merge the contents of the container with the action's input root (so the image root is
carried in-band with the inputs) and the helper treats that merged tree as the image root. In
sideloaded mode the action router only rewrites the command line and the container pull is
performed out-of-band by `bb_docker_root_fetcher`, a new service that needs to run alongside
each worker process.

Which approach to pick depends on the specifics of your deployment:
 - in inline mode the action router introduces a bit more overhead as the input root of each action needs to be rewritten,
   and the worker might spend more time materializing the image contents (which can be mitigated by using FUSE),
 - sideloaded is more complex to set up as it requires an extra service and will result in more load on the registry
   as each worker pulls directly (and not via the CAS), but introduces less scheduling overhead and
   the cost of materializing the container contents is amortized across runners.

Both modes can use either helper: `bb_chroot_helper` (unprivileged, user namespaces) or
`bb_chroot_helper_privileged` (real `chroot`, needs CAP_SYS_ADMIN), except that the privileged
helper is not supported in sideloaded mode (its chroot model can't safely reuse the fetcher's
shared, cached roots). So for inline mode use `bb_docker_action_router` with either helper, and
for sideloaded mode use `bb_docker_action_router`, `bb_chroot_helper` and `bb_docker_root_fetcher`.