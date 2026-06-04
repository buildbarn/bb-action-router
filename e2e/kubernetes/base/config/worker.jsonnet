local common = import 'common.libsonnet';

{
  blobstore: common.blobstore,
  browserUrl: common.browserUrl,
  maximumMessageSizeBytes: common.maximumMessageSizeBytes,
  scheduler: { address: 'scheduler:8983' },
  global: common.global {
    setUmask: { umask: 0 },
  },
  buildDirectories: [{
    native: {
      buildDirectoryPath: '/runner/build',
      cacheDirectoryPath: '/runner/cache',
      maximumCacheFileCount: 10000,
      maximumCacheSizeBytes: 1024 * 1024 * 1024,
      cacheReplacementPolicy: 'LEAST_RECENTLY_USED',
    },
    runners: [{
      endpoint: { address: 'unix:///runner/runner.sock' },
      concurrency: 4,
      instanceNamePrefix: '',
      // This matches the platform the action router emits after rewriting an
      // action.
      platform: {
        properties: [
          { name: 'Flavor', value: 'chroot' },
          { name: 'OSFamily', value: 'linux' },
          { name: 'Version', value: 'generic' },
        ],
      },
      workerId: {
        hostname: 'e2e-worker',
      },
    }],
  }],
  inputDownloadConcurrency: 10,
  outputUploadConcurrency: 11,
  directoryCache: {
    maximumCount: 1000,
    maximumSizeBytes: 1000 * 1024,
    cacheReplacementPolicy: 'LEAST_RECENTLY_USED',
  },
}
