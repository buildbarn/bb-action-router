#!/usr/bin/env bash
# Build + load the local images and bring up the e2e cluster in a given
# action-router mode.
#
# Usage: up.sh [inline|bind-mount]   (default: inline)
set -euo pipefail

MODE="${1:-inline}"
case "${MODE}" in
  inline | bind-mount) ;;
  *) echo "usage: $0 [inline|bind-mount]" >&2 && exit 2 ;;
esac

CLUSTER="${CLUSTER:-kind}"
K8S_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${K8S_DIR}/../.." && pwd)"

case "$(uname -m)" in
  arm64 | aarch64) PLATFORM="//build/platforms:linux_aarch64_musl" ;;
  x86_64 | amd64) PLATFORM="//build/platforms:linux_x86_64_musl" ;;
  *) echo "unsupported host arch: $(uname -m)" >&2 && exit 1 ;;
esac

# Ensure the cluster exists; `kind load` below needs it.
if ! kind get clusters | grep -qx "${CLUSTER}"; then
  echo ">> Creating KinD cluster '${CLUSTER}'"
  kind create cluster --wait 60s
fi

cd "${REPO_ROOT}"

# Reload all images tagged e2e
TARGETS=()
while IFS= read -r t; do TARGETS+=("$t"); done < <(
  bazel query 'attr(tag, "e2e", kind("image_load", //...))' --output=label 2>/dev/null
)
if [ "${#TARGETS[@]}" -eq 0 ]; then
  echo "no e2e image_load targets found" >&2 && exit 1
fi

echo ">> Building images for ${PLATFORM}"
bazel build --output_groups=tarball --platforms="${PLATFORM}" "${TARGETS[@]}"

# The "docker save" tarballs are in the image_load rule's "tarball" output group
# (not its default outputs). Paths are relative to REPO_ROOT.
TARS=()
while IFS= read -r t; do TARS+=("$t"); done < <(
  bazel cquery "set(${TARGETS[*]})" --platforms="${PLATFORM}" --output=starlark \
    --starlark:expr='"\n".join([f.path for f in providers(target)["OutputGroupInfo"].tarball.to_list()])' \
    2>/dev/null
)

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo ">> Loading ${#TARS[@]} images into KinD cluster '${CLUSTER}'"
for i in "${!TARS[@]}"; do
  layout="${WORK}/layout_${i}"
  mkdir -p "${layout}"
  tar -xf "${TARS[$i]}" -C "${layout}"
  # rules_img emits a hybrid tarball (docker manifest.json + OCI
  # oci-layout/index.json). containerd prefers the OCI index, which rules_img
  # leaves nameless, so the image imports untagged. Strip the OCI layout so the
  # image is named from the docker manifest.json RepoTags.
  rm -f "${layout}/oci-layout" "${layout}/index.json"
  docker_tar="${WORK}/image_${i}.tar"
  tar -cf "${docker_tar}" -C "${layout}" manifest.json blobs
  echo "   - ${TARS[$i]}"
  kind load image-archive "${docker_tar}" --name "${CLUSTER}"
done

echo ">> Applying overlay '${MODE}'"
kubectl apply -k "${K8S_DIR}/overlays/${MODE}"

# Stable ConfigMap names mean a plain re-apply won't restart Pods on config-only
# changes, and the Never-policy images need a restart to pick up a fresh load.
echo ">> Restarting workloads"
kubectl -n bb-e2e rollout restart deploy
kubectl -n bb-e2e rollout status deploy --timeout=240s

cat <<EOF

>> Cluster is up in '${MODE}' mode.
   Point a REv2 client at the frontend via:
     kubectl -n bb-e2e port-forward svc/frontend 8980:8980
     bazel buld --remote_executor=grpc://localhost:8980 --remote_instance_name=""
   Inspect: kubectl -n bb-e2e get pods
   Logs:    kubectl -n bb-e2e logs deploy/action-router
            kubectl -n bb-e2e logs deploy/executor -c runner
EOF
