local workflows_template = import 'tools/github_workflows/workflows_template.libsonnet';

workflows_template.getWorkflows(
  containers = [
    'bb_chroot_helper_installer',
    'bb_docker_action_router',
    'bb_docker_root_fetcher',
  ],
)
