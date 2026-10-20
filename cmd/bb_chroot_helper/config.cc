#include "config.h"

#include <filesystem>
#include <memory>
#include <string>
#include <string_view>

#include "CLI/CLI.hpp"

namespace {

// Mandatory marker that ends the helper's own flags; everything after it is the
// action's (client-controlled) command line, which must not be able to pass
// flags to the helper.
constexpr std::string_view kEndOfFlags = "--";

// keep-dirs and etc-files entries are matched against, respectively, top-level
// names in / and file names in /etc. Anything else would either silently never
// match or, in the case of "..", reach outside the directory we mean to touch.
const CLI::Validator kPathComponent(
    [](std::string& value) -> std::string {
      if (value.empty() || value == "." || value == ".." || value.find('/') != std::string::npos) {
        return "entries must be a single path component, got \"" + value + "\"";
      }
      return "";
    },
    "");

// Parse a decimal uid/gid. The length cap keeps it within an int.
bool parse_id(const std::string& value, int* out) {
  if (value.empty() || value.size() > 9) {
    return false;
  }
  int result = 0;
  for (char c : value) {
    if (c < '0' || c > '9') {
      return false;
    }
    result = result * 10 + (c - '0');
  }
  *out = result;
  return true;
}

// A "UID:GID" option. Both ids are set together, so that a user can't be half
// specified and silently fall back to id 0 for the other one.
void add_user_option(CLI::App& app, const std::string& name, int* uid, int* gid, const std::string& description) {
  std::string default_value = std::to_string(*uid) + ":" + std::to_string(*gid);
  app.add_option_function<std::string>(
         name,
         [name, uid, gid](const std::string& value) {
           size_t colon = value.find(':');
           if (colon == std::string::npos || !parse_id(value.substr(0, colon), uid) ||
               !parse_id(value.substr(colon + 1), gid)) {
             throw CLI::ValidationError(name, "want UID:GID with non-negative decimal ids, got \"" + value + "\"");
           }
         },
         description)
      ->type_name("UID:GID")
      ->default_str(default_value);
}

// The helper's options. This is the only list of them: the command line, the
// config file and the usage message are all derived from it. Each option is
// spelled "--name" on the command line and "name" in the config file.
std::unique_ptr<CLI::App> make_app(Config* config) {
  auto app = std::make_unique<CLI::App>("Runs an action in a docker image root in an unprivileged container.",
                                        "bb_chroot_helper");
  app->usage("bb_chroot_helper [OPTIONS] " + std::string(kEndOfFlags) + " <command> [args...]");
  app->footer(
      "Options other than --config and --docker-image-ref can also be set in the TOML file named by --config.\n"
      "Options given on the command line take precedence over the file.");
  // The helper is only ever invoked by the runner; the usage message is printed
  // when the command is missing instead.
  app->set_help_flag();
  // A single-valued option given more than one value (a repeated flag, a TOML
  // array or a duplicated key) is an error rather than resolved by picking one
  // of them. Flags still override the config file, as a config value is only
  // applied to an option that the command line left unset.
  app->option_defaults()->multi_option_policy(CLI::MultiOptionPolicy::Throw);
  app->set_config("--config", "", "TOML file with default values for the options below")
      ->type_name("PATH")
      ->expected(1);
  // Fail on anything we don't recognize, so that a typo can't leave a
  // security-relevant setting at its default.
  app->allow_config_extras(CLI::config_extras_mode::error);

  // Per-action, so it isn't accepted in the config file.
  app->add_option("--docker-image-ref", config->docker_image_ref,
                  "Image to acquire from the fetcher; empty selects inline mode")
      ->type_name("REF")
      ->configurable(false);
  app->add_option("--fetcher-socket", config->fetcher_socket, "Path of the bb_docker_root_fetcher socket")
      ->type_name("PATH")
      ->capture_default_str();
  app->add_option_function<std::string>(
         "--root-mode",
         [config](const std::string& value) {
           config->root_mode = value == "tmpfs" ? RootMode::Tmpfs : RootMode::Overlay;
         },
         "overlay reuses the staging directory for sequential actions; tmpfs supports concurrent actions")
      ->type_name("MODE")
      ->check(CLI::IsMember({"overlay", "tmpfs"}))
      ->default_str(config->root_mode == RootMode::Tmpfs ? "tmpfs" : "overlay");
  app->add_option("--staging-root", config->staging_root, "Dedicated staging directory for the action root")
      ->type_name("PATH")
      ->capture_default_str();
  // CLI11 forces TakeLast on flags, overriding the option defaults above.
  app->add_flag("--network-isolation,!--no-network-isolation", config->isolate_network, "Unshare the network namespace")
      ->multi_option_policy(CLI::MultiOptionPolicy::Throw);
  add_user_option(*app, "--build-user", &config->build_uid, &config->build_gid,
                  "In-namespace uid/gid the action runs as");
  add_user_option(*app, "--host-user", &config->host_uid, &config->host_gid,
                  "Host uid/gid the helper drops privileges to before unsharing");
  // The built-in keep list entries are the mounts the container runtime set up,
  // so keep-dirs adds to them rather than replacing them.
  app->add_option_function<std::vector<std::string>>(
         "--keep-dirs",
         [config](const std::vector<std::string>& values) { config->keep_list.insert(values.begin(), values.end()); },
         "Top-level directories of / to bind-mount into the action root, in addition to proc, sys, dev and tmp")
      ->type_name("NAME")
      ->check(kPathComponent)
      ->delimiter(',')
      ->expected(0, CLI::detail::expected_max_vector_size);
  app->add_option("--etc-files", config->etc_files,
                  "Host /etc files to bind-mount into the image's /etc, replacing the default list")
      ->type_name("NAME")
      ->check(kPathComponent)
      ->delimiter(',')
      ->expected(0, CLI::detail::expected_max_vector_size)
      ->capture_default_str();
  return app;
}

}  // namespace

std::string usage() {
  Config config;
  return make_app(&config)->help();
}

bool parse_command_line(int argc, char** argv, Config* config, int* command_start, std::string* error) {
  // Only the arguments before the separator are handed to the parser, so the
  // action's command line can't be mistaken for the helper's flags.
  int separator = 1;
  while (separator < argc && std::string_view(argv[separator]) != kEndOfFlags) {
    separator++;
  }
  if (separator >= argc) {
    *error = "missing \"" + std::string(kEndOfFlags) + "\" separator before the command";
    return false;
  }
  *command_start = separator + 1;

  // CLI11 can't tell "--name=" from a bare "--name", so it would take the next
  // flag as the value. Refuse the form rather than misparse it.
  for (int i = 1; i < separator; i++) {
    std::string_view arg(argv[i]);
    if (arg.size() > 3 && arg.substr(0, 2) == "--" && arg.back() == '=') {
      *error = std::string(arg) + ": an empty value must be a separate argument (\"" +
               std::string(arg.substr(0, arg.size() - 1)) + "\" \"\") or the option left out";
      return false;
    }
  }

  try {
    make_app(config)->parse(separator, argv);
  } catch (const CLI::ParseError& e) {
    *error = e.what();
    return false;
  }
  return true;
}

bool validate_config(const Config& config, std::string* error) {
  std::filesystem::path staging_root(config.staging_root);
  if (!staging_root.is_absolute() || staging_root.parent_path() == "/" || staging_root.filename().empty() ||
      config.staging_root != staging_root.lexically_normal().string()) {
    *error = "staging-root must be a normalized absolute path below a top-level directory";
    return false;
  }
  // The helper drops privileges to the host user before unsharing, so id 0 there
  // would make setuid()/setgid() a no-op. Id 0 is fine for the build user, which
  // is only the in-namespace mapping.
  if (config.host_uid == 0 || config.host_gid == 0) {
    *error = "host-user uid and gid must not be 0";
    return false;
  }
  return true;
}
