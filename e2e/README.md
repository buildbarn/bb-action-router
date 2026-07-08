# Kubernetes (KinD) e2e environment

A minimal Buildbarn cluster for exercising `bb_docker_action_router` and the
chroot helpers end to end, across the supported router-mode / helper
combinations.

## Combination selection

The combination is selected entirely by the kustomize overlay, which swaps the
router config (inline vs sideloaded, and which helper binary the router points
`chrootHelperPath` at) and, for sideloaded, patches a `root-fetcher` sidecar
into the executor Pod. The base manifests are identical across combinations.

Supported combinations (overlay = argument to `up.sh`):

| overlay                   | router mode | helper                        | notes                                   |
| ------------------------- | ----------- | ----------------------------- | --------------------------------------- |
| `inline-unprivileged`     | inline      | `bb_chroot_helper` (userns)   | image root = action's merged input root |
| `inline-privileged`       | inline      | `bb_chroot_helper_privileged` | real `chroot()` into the input root     |
| `sideloaded-unprivileged` | sideloaded  | `bb_chroot_helper` (userns)   | root materialized by the fetcher sidecar |

`sideloaded` + the privileged helper is intentionally unsupported: the
privileged helper `chroot()`s into the root, which can't safely reuse the
fetcher's shared, ref-counted cached roots (a stray bind-mount into a cached
root could be clobbered when the worker cleans up the build dir).

## Images

The published images (`bb-storage`, `bb-scheduler`, `bb-worker`, `busybox`) are
pulled from registries by the node. The three locally-developed images
(`bb_docker_action_router`, `bb_docker_root_fetcher`, `bb_runner_scratch`) are
built with `rules_img` for the host arch and loaded into the node with
`kind load image-archive`.

`up.sh` spins up a KinD cluster (if needed), builds the image and deploys the
manifests.

## Usage

```bash
cd e2e/kubernetes

# Create the cluster (first run), build + load the local images, apply the
# overlay, and wait for the rollout — all in one. The argument is the
# combination (default: inline-unprivileged):
./scripts/up.sh inline-unprivileged    # or: inline-privileged | sideloaded-unprivileged

# Start a port forward (here it's in the background but you can use a separate
# terminal).
kubectl -n bb-e2e port-forward svc/frontend 8980:8980 &

cd ../workspace
bazel test //... --remote_executor=grpc://localhost:8980 --remote_instance_name=""
```

## Prerequisites

- `kind`, `kubectl` (its built-in `-k` kustomize is used; standalone `kustomize`
  is not required), `bazel`.
- A working KinD cluster. KinD needs Docker or Podman; Apple `container` is not
  a supported KinD provider, so point KinD at whatever Linux container provider
  you use on this machine. Set `CLUSTER` if your cluster isn't named `kind`.
- Single arch only: images are built for the host arch (= the KinD node arch).