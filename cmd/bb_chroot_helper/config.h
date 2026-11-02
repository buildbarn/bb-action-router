#pragma once

#include <set>
#include <string>
#include <vector>

// Settings for bb_chroot_helper.
//
// Fields start out at the built-in defaults, are then overridden by the config
// file (--config=PATH, when given) and finally by the remaining command line
// flags. Every setting is available in both places under the same name, so a
// deployment can keep its static settings in one file while the action router
// only has to template the per-action ones onto the command line.
struct Config {
  // Reference of the image to acquire from the fetcher. An empty value selects
  // inline mode, where the action's merged input root is the image root.
  //
  // The one setting that can't come from the config file, as it's per-action.
  std::string docker_image_ref;

  // Path of the bb_docker_root_fetcher socket. Unused in inline mode.
  std::string fetcher_socket = "/var/run/fetcher/fetcher.sock";

  // Staging directory holding the action roots, one numbered directory per
  // concurrently running action; must be dedicated to the helper.
  std::string staging_root = "/var/action_root";

  // Whether to unshare the network namespace. This is per-action, but the
  // config file can set the default for actions that don't specify it.
  bool isolate_network = false;

  // In-namespace uid/gid the action runs as. Must match the build user the
  // docker root fetcher writes into the image's /etc/passwd.
  int build_uid = 0;
  int build_gid = 0;

  // Host-side uid/gid the helper setuid()s to before unsharing. Must be an id
  // the runner container is allowed to switch to.
  int host_uid = 1000;
  int host_gid = 1000;

  // Top-level directories of / that are bind-mounted into the action's root.
  // The built-in entries are the ones the container runtime mounts; keep-dirs
  // are added to them.
  //
  // A deployment has to add the top-level directory that holds the worker's
  // build directory (the action's working directory lives in there), since
  // where that sits is deployment specific.
  std::set<std::string> keep_list = {"proc", "sys", "dev", "tmp"};

  // Host /etc files bind-mounted into the image's /etc, so that the action sees
  // the host's version. DNS resolution depends on this. etc-files replaces this
  // list.
  std::vector<std::string> etc_files = {"resolv.conf", "hostname", "hosts"};
};

// Usage message, generated from the same option table the parser uses.
std::string usage();

// Apply the command line to *config, including the TOML file named by
// --config. Flags take precedence over the file regardless of their position.
// The flags must be terminated with "--"; *command_start is set to the index
// just past it, which is argc when there is no command.
//
// An unknown option or config file key is an error rather than ignored: a typo
// in a setting that decides where the sandbox boundary sits must not be silently
// dropped.
//
// Returns false and sets *error if a flag or the config file is malformed.
bool parse_command_line(int argc, char** argv, Config* config, int* command_start, std::string* error);

// Check the fully assembled configuration, i.e. the invariants that hold no
// matter which of the sources a setting came from.
//
// Returns false and sets *error on the first violation.
bool validate_config(const Config& config, std::string* error);
