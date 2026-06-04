// Minimal single-node e2e cluster.
{
  storageAddress: 'storage:8981',

  blobstore: {
    contentAddressableStorage: {
      grpc: { client: { address: $.storageAddress } },
    },
    actionCache: {
      completenessChecking: {
        backend: {
          grpc: { client: { address: $.storageAddress } },
        },
        maximumTotalTreeSizeBytes: 64 * 1024 * 1024,
      },
    },
  },

  fileSystemAccessCache: {
    grpc: { client: { address: $.storageAddress } },
  },

  browserUrl: 'http://localhost:7984',
  maximumMessageSizeBytes: 2 * 1024 * 1024,

  // Diagnostics HTTP server on a chosen port. Containers that share a Pod
  // network namespace (worker + runner [+ root-fetcher]) must each use a
  // distinct port, otherwise they collide on bind.
  globalDiag(port):: {
    diagnosticsHttpServer: {
      httpServers: [{
        listenAddresses: [':' + std.toString(port)],
        authenticationPolicy: { allow: {} },
      }],
      enablePrometheus: true,
      enablePprof: true,
      enableActiveSpans: true,
    },
  },
  global: self.globalDiag(80),
}
