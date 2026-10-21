#!/usr/bin/env bash
# Loads the bb_chroot_helper integration test image and runs it. The first
# argument is the image tarball; the rest are passed to the test binary.
#
# CONTAINER_RUNTIME selects the runtime: docker (default), podman or container.
set -euo pipefail

tarball="$1"
shift
runtime="${CONTAINER_RUNTIME:-docker}"
image=bb_chroot_helper_integration_test:latest

case "${runtime}" in
docker | podman)
  "${runtime}" load -i "${tarball}"
  # Docker's containerd image store lists the loaded image but fails to resolve
  # it by tag in "run", so run it by ID.
  image="$("${runtime}" images -q --no-trunc "${image}" | head -n1)"
  # The helper unshares user, mount and network namespaces and mounts
  # filesystems, which the default seccomp and AppArmor profiles deny.
  exec "${runtime}" run --rm \
    --security-opt seccomp=unconfined \
    --security-opt apparmor=unconfined \
    "${image}" -test.v "$@"
  ;;
container)
  container image load -i "${tarball}"
  # Each container runs in its own VM, without seccomp or AppArmor profiles.
  exec container run --rm "${image}" -test.v "$@"
  ;;
*)
  echo "unsupported CONTAINER_RUNTIME: ${runtime}" >&2
  exit 1
  ;;
esac
