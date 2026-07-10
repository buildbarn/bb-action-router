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
      // Prepend the (unprivileged, userns) helper. It fetches the docker root
      // out-of-band from the fetcher sidecar over the shared /var/fetcher
      // socket. --build-user=0:0 must match the fetcher's build_user.
      { editCommand: { prependArguments: [
        '/bin/bb_chroot_helper',
        '--docker-image-ref={{.Platform.Get "ContainerBaseImage" | trimPrefix "docker://"}}',
        '--fetcher-socket=/var/fetcher/fetcher.sock',
        '--build-user=0:0',
        '{{if and (eq (.Platform.Get "requires-network") "false") (ne (.Platform.Get "requires-external") "true")}}--network-isolation{{else}}--no-network-isolation{{end}}',
      ] } },
      // Retarget the worker platform queue.
      { editPlatformProperty: {
        remove: ['ContainerBaseImage', 'requires-network', 'requires-external', 'Flavor', 'Version'],
        add: [{ name: 'Flavor', value: 'chroot' }, { name: 'Version', value: 'generic' }],
      } },
    ],
  },
}
