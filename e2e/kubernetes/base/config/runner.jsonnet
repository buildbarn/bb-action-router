local common = import 'common.libsonnet';

{
  buildDirectoryPath: '/runner/build',
  // Distinct diagnostics port: the runner shares the executor Pod's network
  // namespace with the worker (which uses :80).
  global: common.globalDiag(81),
  grpcServers: [{
    listenPaths: ['/runner/runner.sock'],
    authenticationPolicy: { allow: {} },
  }],
}
