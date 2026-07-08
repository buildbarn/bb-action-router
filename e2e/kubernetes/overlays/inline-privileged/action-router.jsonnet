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
  inline: {
    chrootHelperPath: '/bin/bb_chroot_helper_privileged',
    maximumImageSizeBytes: 10 * 1024 * 1024 * 1024,
    imagePullTimeout: '300s',
    registryAuthentication: [
      { anonymous: { registry: 'ghcr.io' } },
      { anonymous: { registry: 'index.docker.io' } },
    ],
  },
}
