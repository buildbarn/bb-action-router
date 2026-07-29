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
      // Prepend the (unprivileged, userns) helper. With no --docker-image-ref
      // it treats the merged input root as the image root. Its static settings
      // come from the config file mounted into the runner container from the
      // bb-chroot-helper-config ConfigMap. The '--' terminates the helper's own
      // flags, so the action's command line can't be read as helper flags.
      { editCommand: { prependArguments: [
        '/bin/bb_chroot_helper',
        '--config=/etc/bb_chroot_helper/config.toml',
        '--',
      ] } },
      // Retarget the worker platform queue.
      { editPlatformProperty: {
        remove: ['ContainerBaseImage', 'requires-network', 'requires-external', 'Flavor', 'Version'],
        add: [{ name: 'Flavor', value: 'chroot' }, { name: 'Version', value: 'generic' }],
      } },
    ],
  },
}
