{
  global: {
    diagnosticsHttpServer: {
      // Distinct port: shares the executor Pod netns with worker (:80) and
      // runner (:81).
      httpServers: [{
        listenAddresses: [':82'],
        authenticationPolicy: { allow: {} },
      }],
      enablePrometheus: true,
    },
  },
  socketPath: '/var/fetcher/fetcher.sock',
  rootsDirectoryPath: '/var/fetcher/roots',
  fetchParallelism: 4,
  fetchOpenDirectoryLimit: 16,
  maximumMaterializedRoots: 4,
  maximumConcurrentFetches: 2,
  maximumMessageSizeBytes: 2 * 1024 * 1024,
  maximumImageSizeBytes: 10 * 1024 * 1024 * 1024,
  imagePullTimeout: '300s',
  acquireTimeout: '600s',
  registryAuthentication: [
    { anonymous: { registry: 'ghcr.io' } },
    { anonymous: { registry: 'index.docker.io' } },
  ],
}
