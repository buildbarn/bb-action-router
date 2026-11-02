local common = import 'common.libsonnet';

// Single storage node. The block devices are backed by files on
// an emptyDir volume mounted at /storage.
{
  grpcServers: [{
    listenAddresses: [':8981'],
    authenticationPolicy: { allow: {} },
  }],
  maximumMessageSizeBytes: common.maximumMessageSizeBytes,
  global: common.global,
  contentAddressableStorageServer: {
    contentAddressableStorage: {
      chunkStorage: {
        'local': {
          keyLocationMap: {
            onBlockDevice: {
              file: { path: '/storage/cs_klm', sizeBytes: 16 * 1024 * 1024 },
            },
            maximumGetAttempts: 16,
            maximumPutAttempts: 64,
          },
          oldBlocks: 8,
          currentBlocks: 24,
          newBlocks: 3,
          blocksOnBlockDevice: {
            source: {
              file: { path: '/storage/cs_blocks', sizeBytes: 4 * 1024 * 1024 * 1024 },
            },
            spareBlocks: 3,
          },
          chunkingParameters: {
            minChunkSizeBytes: 64 * 1024,
            horizonSizeBytes: 8 * 64 * 1024,
          },
        },
      },
      chunkMappingStorage: {
        chunkMappingValidating: {
          backend: {
            'local': {
              keyLocationMap: {
                onBlockDevice: {
                  file: { path: '/storage/cms_klm', sizeBytes: 4 * 1024 * 1024 },
                },
                maximumGetAttempts: 16,
                maximumPutAttempts: 64,
              },
              oldBlocks: 8,
              currentBlocks: 24,
              newBlocks: 1,
              blocksOnBlockDevice: {
                source: {
                  file: { path: '/storage/cms_blocks', sizeBytes: 128 * 1024 * 1024 },
                },
                spareBlocks: 3,
              },
            },
          },
        },
      },
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
    findMissingAuthorizer: { allow: {} },
    maximumChunkCount: 100000,
  },
  actionCache: {
    backend: {
      'local': {
        keyLocationMap: {
          onBlockDevice: {
            file: { path: '/storage/ac_klm', sizeBytes: 1024 * 1024 },
          },
          maximumGetAttempts: 16,
          maximumPutAttempts: 64,
        },
        oldBlocks: 8,
        currentBlocks: 24,
        newBlocks: 1,
        blocksOnBlockDevice: {
          source: {
            file: { path: '/storage/ac_blocks', sizeBytes: 64 * 1024 * 1024 },
          },
          spareBlocks: 3,
        },
      },
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
  },
  fileSystemAccessCache: {
    backend: {
      'local': {
        keyLocationMap: {
          onBlockDevice: {
            file: { path: '/storage/fsac_klm', sizeBytes: 1024 * 1024 },
          },
          maximumGetAttempts: 16,
          maximumPutAttempts: 64,
        },
        oldBlocks: 8,
        currentBlocks: 24,
        newBlocks: 1,
        blocksOnBlockDevice: {
          source: {
            file: { path: '/storage/fsac_blocks', sizeBytes: 64 * 1024 * 1024 },
          },
          spareBlocks: 3,
        },
      },
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
  },
}
