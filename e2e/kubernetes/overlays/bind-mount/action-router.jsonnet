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
  bindMount: {
    // The socket and helper are on the runner container's filesystem: the
    // fetcher sidecar and runner share the /var/fetcher volume, and the helper
    // is baked into the scratch runner image at /bin.
    fetcherSocketPath: '/var/fetcher/fetcher.sock',
    chrootHelperPath: '/bin/bb_chroot_helper',
  },
}
