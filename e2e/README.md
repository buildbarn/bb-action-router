# Kubernetes (KinD) e2e environment

A minimal Buildbarn cluster for exercising `bb_docker_action_router` and the
chroot helpers end to end, in either **inline** or **bind-mount** mode.

## inline / bind-mount mode selection

The mode is selected entirely by the kustomize overlay, which swaps the
router config and (for bind-mount) patches a `root-fetcher` sidecar into the
executor Pod. The base manifests are identical for both.

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
# overlay, and wait for the rollout — all in one:
./scripts/up.sh inline        # or: ./scripts/up.sh bind-mount

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