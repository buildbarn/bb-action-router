#include "config.h"

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <fstream>
#include <string>
#include <string_view>

#include "toml.hpp"

namespace {

// Setting names. A setting that exists in both places is spelled the same way
// in each: the config file key is the name verbatim, the flag is "--" plus the
// name. TOML bare keys allow dashes, so nothing has to be translated.
constexpr std::string_view kFetcherSocket = "fetcher-socket";
constexpr std::string_view kNetworkIsolation = "network-isolation";
constexpr std::string_view kBuildUser = "build-user";
constexpr std::string_view kHostUser = "host-user";
constexpr std::string_view kKeepDirs = "keep-dirs";
constexpr std::string_view kEtcFiles = "etc-files";
// Nested in the [build-user] and [host-user] tables.
constexpr std::string_view kUid = "uid";
constexpr std::string_view kGid = "gid";
// The image ref is per-action, so this one is a flag only; as a config file key
// it's rejected with a message pointing at the flag.
constexpr std::string_view kDockerImageRef = "docker-image-ref";
// Flags with no config file equivalent: --config names the file itself, and
// --no-network-isolation is the negated form of network-isolation.
constexpr std::string_view kConfig = "config";
constexpr std::string_view kNoNetworkIsolation = "no-network-isolation";

// Mandatory marker that ends the helper's own flags; everything after it is the
// action's (client-controlled) command line, which must not be able to pass
// flags to the helper.
constexpr std::string_view kEndOfFlags = "--";

// Every key the config file may contain. A key that isn't listed here is
// rejected, so adding a setting above means adding it to one of these too.
constexpr std::string_view kTopLevelKeys[] = {
    kFetcherSocket, kNetworkIsolation, kKeepDirs, kEtcFiles, kBuildUser, kHostUser,
};
constexpr std::string_view kUserTableKeys[] = {kUid, kGid};

// Real uids/gids are far below this; the cap keeps them within an int.
constexpr int64_t kMaxId = 999999999;

// The command line spelling of a setting.
std::string flag_name(std::string_view name) { return "--" + std::string(name); }

// Prefix for an error message that points at a node in the config file.
std::string at(const std::string& path, const toml::node& node) {
  return path + ":" + std::to_string(node.source().begin.line) + ": ";
}

// keep-dirs and etc-files entries are matched against, respectively, top-level
// names in / and file names in /etc. Anything else would either silently never
// match or, in the case of "..", reach outside the directory we mean to touch.
bool is_single_path_component(const std::string& value) {
  return !value.empty() && value != "." && value != ".." && value.find('/') == std::string::npos;
}

// Fail on anything we don't recognize, so that a typo can't leave a
// security-relevant setting at its default. `prefix` names the enclosing table
// (empty at the top level) and is only used for the error message.
template <std::size_t N>
bool reject_unknown_keys(const toml::table& table, const std::string_view (&known)[N], const std::string& path,
                         const std::string& prefix, std::string* error) {
  for (const auto& [key, value] : table) {
    if (std::find(std::begin(known), std::end(known), key.str()) == std::end(known)) {
      *error = at(path, value) + "unknown key \"" + prefix + std::string(key.str()) + "\"";
      return false;
    }
  }
  return true;
}

// The get_* helpers below leave *out alone when the key is absent, so that
// unset keys keep the built-in default.

bool get_string(const toml::table& table, std::string_view key, std::string* out, const std::string& path,
                std::string* error) {
  const toml::node* node = table.get(key);
  if (node == nullptr) {
    return true;
  }
  const auto value = node->value_exact<std::string>();
  if (!value) {
    *error = at(path, *node) + std::string(key) + " must be a string";
    return false;
  }
  *out = *value;
  return true;
}

bool get_bool(const toml::table& table, std::string_view key, bool* out, const std::string& path, std::string* error) {
  const toml::node* node = table.get(key);
  if (node == nullptr) {
    return true;
  }
  const auto value = node->value_exact<bool>();
  if (!value) {
    *error = at(path, *node) + std::string(key) + " must be true or false";
    return false;
  }
  *out = *value;
  return true;
}

// Unlike the other getters this one requires the key to be present: a
// [build-user] that only sets uid would otherwise silently run the action as
// gid 0.
bool get_required_id(const toml::table& table, std::string_view key, int* out, const std::string& path,
                     const std::string& prefix, const toml::node& table_node, std::string* error) {
  const toml::node* node = table.get(key);
  if (node == nullptr) {
    *error = at(path, table_node) + prefix + std::string(key) + " is required";
    return false;
  }
  const auto value = node->value_exact<int64_t>();
  if (!value || *value < 0 || *value > kMaxId) {
    *error =
        at(path, *node) + prefix + std::string(key) + " must be an integer between 0 and " + std::to_string(kMaxId);
    return false;
  }
  *out = static_cast<int>(*value);
  return true;
}

bool get_string_array(const toml::table& table, std::string_view key, std::vector<std::string>* out,
                      const std::string& path, std::string* error) {
  const toml::node* node = table.get(key);
  if (node == nullptr) {
    return true;
  }
  const toml::array* array = node->as_array();
  if (array == nullptr) {
    *error = at(path, *node) + std::string(key) + " must be an array of strings";
    return false;
  }
  std::vector<std::string> values;
  for (const auto& element : *array) {
    const auto value = element.value_exact<std::string>();
    if (!value) {
      *error = at(path, element) + std::string(key) + " must be an array of strings";
      return false;
    }
    if (!is_single_path_component(*value)) {
      *error = at(path, element) + std::string(key) + " entries must be a single path component, got \"" + *value +
               "\"";
      return false;
    }
    values.push_back(*value);
  }
  *out = std::move(values);
  return true;
}

// Parse a [build-user]/[host-user] table. Absent tables keep the defaults.
bool get_user_table(const toml::table& root, std::string_view key, int* uid, int* gid, const std::string& path,
                    std::string* error) {
  const toml::node* node = root.get(key);
  if (node == nullptr) {
    return true;
  }
  const toml::table* table = node->as_table();
  if (table == nullptr) {
    *error = at(path, *node) + std::string(key) + " must be a table with " + std::string(kUid) + " and " +
             std::string(kGid) + " keys";
    return false;
  }
  std::string prefix = std::string(key) + ".";
  return reject_unknown_keys(*table, kUserTableKeys, path, prefix, error) &&
         get_required_id(*table, kUid, uid, path, prefix, *node, error) &&
         get_required_id(*table, kGid, gid, path, prefix, *node, error);
}

// Parse a decimal uid/gid as passed in a "UID:GID" flag value.
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

bool parse_user_flag(std::string_view name, const std::string& value, int* uid, int* gid, std::string* error) {
  size_t colon = value.find(':');
  if (colon == std::string::npos || !parse_id(value.substr(0, colon), uid) ||
      !parse_id(value.substr(colon + 1), gid)) {
    *error =
        "invalid " + flag_name(name) + ": want UID:GID with non-negative decimal ids, got \"" + value + "\"";
    return false;
  }
  return true;
}

// A command line flag, spelled "--<name>" (see flag_name). `value_name` is the
// placeholder shown in the usage message and is null for flags that take no
// value. `apply` writes the flag's effect to *config, and is null for the one
// flag (--config) that has to be handled before the others so that they can
// override the file it names.
struct Flag {
  std::string_view name;
  const char* value_name;
  bool (*apply)(const std::string& value, Config* config, std::string* error);
};

// The flags the helper accepts. This is the only list of them: parsing, the
// flag/command boundary and the usage message are all derived from it.
constexpr Flag kFlags[] = {
    {kConfig, "PATH", nullptr},
    {kDockerImageRef, "REF",
     [](const std::string& value, Config* config, std::string*) {
       config->docker_image_ref = value;
       return true;
     }},
    {kFetcherSocket, "PATH",
     [](const std::string& value, Config* config, std::string*) {
       config->fetcher_socket = value;
       return true;
     }},
    {kBuildUser, "UID:GID",
     [](const std::string& value, Config* config, std::string* error) {
       return parse_user_flag(kBuildUser, value, &config->build_uid, &config->build_gid, error);
     }},
    {kNetworkIsolation, nullptr,
     [](const std::string&, Config* config, std::string*) {
       config->isolate_network = true;
       return true;
     }},
    {kNoNetworkIsolation, nullptr,
     [](const std::string&, Config* config, std::string*) {
       config->isolate_network = false;
       return true;
     }},
};

// Match `arg` against the flag table. Returns null if it isn't one of our
// flags, which is an error (the helper's flags have to be terminated with
// kEndOfFlags). Value flags are spelled "--name=VALUE"; *value is cleared for
// the others.
const Flag* match_flag(const std::string& arg, std::string* value) {
  for (const Flag& flag : kFlags) {
    std::string name = flag_name(flag.name);
    if (flag.value_name == nullptr) {
      if (arg == name) {
        value->clear();
        return &flag;
      }
    } else if (arg.rfind(name + "=", 0) == 0) {
      *value = arg.substr(name.size() + 1);
      return &flag;
    }
  }
  return nullptr;
}

}  // namespace

std::string usage() {
  std::string out;
  for (const Flag& flag : kFlags) {
    out += "[" + flag_name(flag.name);
    if (flag.value_name != nullptr) {
      out += std::string("=") + flag.value_name;
    }
    out += "] ";
  }
  out += std::string(kEndOfFlags) + " <command> [args...]\n\n";
  out += "Config file keys are documented in cmd/bb_chroot_helper/README.md.";
  return out;
}

bool parse_config_file(const std::string& path, Config* config, std::string* error) {
  // toml::parse_file() reports an unreadable file as a parse error without a
  // useful source location, so check for it up front.
  if (!std::ifstream(path)) {
    *error = "cannot read config file " + path;
    return false;
  }

  toml::parse_result result = toml::parse_file(path);
  if (!result) {
    const toml::source_position& begin = result.error().source().begin;
    *error = path + ":" + std::to_string(begin.line) + ":" + std::to_string(begin.column) + ": " +
             std::string(result.error().description());
    return false;
  }
  const toml::table& root = result.table();

  if (root.contains(kDockerImageRef)) {
    *error = at(path, *root.get(kDockerImageRef)) + std::string(kDockerImageRef) + " is per-action; pass " +
             flag_name(kDockerImageRef) + " instead";
    return false;
  }
  if (!reject_unknown_keys(root, kTopLevelKeys, path, "", error)) {
    return false;
  }

  std::vector<std::string> keep_dirs;
  if (!get_string(root, kFetcherSocket, &config->fetcher_socket, path, error) ||
      !get_bool(root, kNetworkIsolation, &config->isolate_network, path, error) ||
      !get_user_table(root, kBuildUser, &config->build_uid, &config->build_gid, path, error) ||
      !get_user_table(root, kHostUser, &config->host_uid, &config->host_gid, path, error) ||
      !get_string_array(root, kEtcFiles, &config->etc_files, path, error) ||
      !get_string_array(root, kKeepDirs, &keep_dirs, path, error)) {
    return false;
  }
  // The built-in keep list entries are the mounts the container runtime set up,
  // so keep-dirs adds to them rather than replacing them.
  config->keep_list.insert(keep_dirs.begin(), keep_dirs.end());
  return true;
}

bool parse_command_line(int argc, char** argv, Config* config, int* command_start, std::string* error) {
  std::string config_path;
  bool have_config_path = false;
  int i = 1;
  for (; i < argc && std::string_view(argv[i]) != kEndOfFlags; i++) {
    std::string value;
    const Flag* flag = match_flag(argv[i], &value);
    if (flag == nullptr) {
      *error = "unrecognized flag \"" + std::string(argv[i]) + "\"; the helper's flags must be terminated with \"" +
               std::string(kEndOfFlags) + "\" before the command";
      return false;
    }
    if (flag->apply == nullptr) {
      config_path = value;
      have_config_path = true;
    }
  }
  if (i >= argc) {
    *error = "missing \"" + std::string(kEndOfFlags) + "\" separator before the command";
    return false;
  }
  // argv[i] is the separator, so the command starts right after it.
  *command_start = i + 1;

  if (have_config_path) {
    if (config_path.empty()) {
      *error = "empty " + flag_name(kConfig) + " path";
      return false;
    }
    if (!parse_config_file(config_path, config, error)) {
      return false;
    }
  }

  for (int j = 1; j < i; j++) {
    std::string value;
    // Non-null: the first pass rejected anything that isn't one of our flags.
    const Flag* flag = match_flag(argv[j], &value);
    if (flag->apply != nullptr && !flag->apply(value, config, error)) {
      return false;
    }
  }
  return true;
}

bool validate_config(const Config& config, std::string* error) {
  // The helper drops privileges to the host user before unsharing, so id 0 there
  // would make setuid()/setgid() a no-op. Id 0 is fine for the build user, which
  // is only the in-namespace mapping.
  if (config.host_uid == 0 || config.host_gid == 0) {
    *error = std::string(kHostUser) + " " + std::string(kUid) + " and " + std::string(kGid) + " must not be 0";
    return false;
  }
  return true;
}
