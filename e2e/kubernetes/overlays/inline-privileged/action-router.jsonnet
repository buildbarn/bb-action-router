{
  grpcServers: [{
    listenAddresses: [':8982'],
    authenticationPolicy: { allow: {} },
  }],
  maximumMessageSizeBytes: 2 * 1024 * 1024,
  global: {
    diagnosticsHttpServer: {
      httpServers: [{
        listenAddresses: [':80'],
        authenticationPolicy: { allow: {} },
      }],
      enablePrometheus: true,
    },
  },
  blobstore: {
    contentAddressableStorage: { grpc: { client: { address: 'storage:8981' } } },
    actionCache: { grpc: { client: { address: 'storage:8981' } } },
  },
  pipeline: {
    // Only rewrite actions that carry a docker base image.
    condition: '{{ne (.Platform.Get "ContainerBaseImage") ""}}',
    operations: [
      // Require a docker:// prefix and a pinned sha256 digest.
      { assertPlatformProperty: {
        property: 'ContainerBaseImage',
        regex: '^docker://.+@sha256:[0-9a-f]{64}$',
      } },
      // Pull the image, upload its tree to the CAS, and merge it into the
      // action's input root (original inputs nested under bazel_exec_root;
      // working directory rewritten accordingly).
      { mergeDockerRoot: {
        imageRef: '{{.Platform.Get "ContainerBaseImage" | trimPrefix "docker://"}}',
        maximumImageSizeBytes: 10 * 1024 * 1024 * 1024,
        imagePullTimeout: '300s',
        registryAuthentication: [
          { anonymous: { registry: 'ghcr.io' } },
          { anonymous: { registry: 'index.docker.io' } },
        ],
      } },
      // Prepend the privileged helper, which performs a real chroot into the
      // merged input root.
      { editCommand: { prependArguments: ['/bin/bb_chroot_helper_privileged'] } },
      // Retarget the worker platform queue.
      { editPlatformProperty: {
        remove: ['ContainerBaseImage', 'requires-network', 'requires-external', 'Flavor', 'Version'],
        add: [{ name: 'Flavor', value: 'chroot' }, { name: 'Version', value: 'generic' }],
      } },
    ],
  },
}
