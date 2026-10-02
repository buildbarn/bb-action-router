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
  contentAddressableStorage: {
    backend: {
      'local': {
        keyLocationMap: {
          onBlockDevice: {
            file: { path: '/storage/cas_klm', sizeBytes: 16 * 1024 * 1024 },
          },
          maximumGetAttempts: 16,
          maximumPutAttempts: 64,
        },
        oldBlocks: 8,
        currentBlocks: 24,
        newBlocks: 3,
        blocksOnBlockDevice: {
          source: {
            file: { path: '/storage/cas_blocks', sizeBytes: 4 * 1024 * 1024 * 1024 },
          },
          spareBlocks: 3,
        },
      },
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
    findMissingAuthorizer: { allow: {} },
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
