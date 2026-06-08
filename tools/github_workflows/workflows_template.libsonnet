{
  local platforms = [
    {
      name: 'linux_x86_64_musl',
      target: '//build/platforms:linux_x86_64_musl',
      test: true,
    },
  ],

  local bazelInstallStep(bazel_os) = {
    name: 'Installing Bazel',
    run: 'v=$(cat .bazelversion) && curl -L https://github.com/bazelbuild/bazel/releases/download/${v}/bazel-${v}-%s-x86_64 > ~/bazel && chmod +x ~/bazel && echo ~ >> ${GITHUB_PATH}' % bazel_os,
    shell: 'bash',
  },

  local noDefaultTestToolchain = '--@bazel_tools//tools/test:incompatible_use_default_test_toolchain=False',

  local getJobs(setupSteps, extraSteps) = {
    build_and_test: {
      strategy: {
        matrix: {
          host: [
            {
              bazel_os: 'linux',
              cross_compile: true,
              os: 'ubuntu-latest',
              platform_name: 'linux_x86_64_musl',
            },
          ],
        },
      },
      'runs-on': '${{ matrix.host.os }}',
      name: 'build_and_test ${{ matrix.host.os }}',
      steps: [
        {
          name: 'Check out source code',
          uses: 'actions/checkout@v1',
        },
      ] + setupSteps + [
        bazelInstallStep('${{matrix.host.bazel_os}}'),
      ] + std.flattenArrays([
        [
          {
            name: '%s: build' % platform.name,
            run: 'bazel build --platforms=%s %s //...' % [
              platform.target,
              noDefaultTestToolchain,
            ],
            'if': "matrix.host.cross_compile || matrix.host.platform_name == '%s'" % platform.name,
          },
        ] + if std.get(platform, 'test', false) then [
          {
            name: '%s: test' % platform.name,
            run: 'bazel test --test_output=errors $(bazel query \'kind(".*_test rule", //...)\')',
            'if': "matrix.host.platform_name == '%s'" % platform.name,
          },
        ] else []
        for platform in platforms
      ]) + extraSteps,
    },
    lint: {
      'runs-on': 'ubuntu-latest',
      name: 'lint',
      steps: [
        {
          name: 'Check out source code',
          uses: 'actions/checkout@v1',
        },
      ] + setupSteps + [
        bazelInstallStep('linux'),
        {
          name: 'Reformat',
          run: 'bazel run @com_github_buildbarn_bb_storage//tools:reformat',
        },
        {
          name: 'Test style conformance',
          run: 'git add . && git diff --exit-code HEAD --',
        },
        {
          name: 'Golint',
          run: 'bazel run @org_golang_x_lint//golint -- -set_exit_status $(pwd)/...',
        },
      ],
    },
  },

  getWorkflows(setupSteps=[], extraSteps=[]): {
    'main.yaml': {
      name: 'main',
      on: { push: { branches: ['main'] } },
      jobs: getJobs(setupSteps, extraSteps),
    },
    'pull-requests.yaml': {
      name: 'pull-requests',
      on: { pull_request: { branches: ['main'] } },
      jobs: getJobs(setupSteps, extraSteps),
    },
  },
}
