// Unit tests for the bb_chroot_helper config file / command line parser.
#include "config.h"

#include <fstream>
#include <string>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"

namespace {

using ::testing::HasSubstr;
using ::testing::TempDir;

// Write `contents` to a uniquely named file in the test's temporary directory
// and return its path.
std::string WriteConfig(const std::string& contents) {
  static int counter = 0;
  std::string path = TempDir() + "/config_test_" + std::to_string(++counter) + ".toml";
  std::ofstream f(path);
  f << contents;
  f.close();
  return path;
}

struct ParseResult {
  bool ok;
  Config config;
  int command_start;
  std::string error;
};

// Run the arguments through parse_command_line(), with argv[0] prepended.
ParseResult Parse(const std::vector<std::string>& args) {
  std::string program = "bb_chroot_helper";
  std::vector<char*> argv;
  argv.push_back(const_cast<char*>(program.c_str()));
  for (const auto& arg : args) {
    argv.push_back(const_cast<char*>(arg.c_str()));
  }

  ParseResult result;
  result.command_start = -1;
  result.ok = parse_command_line(static_cast<int>(argv.size()), argv.data(), &result.config, &result.command_start,
                                 &result.error);
  return result;
}

// Parse a config file on its own, with no other flags involved.
ParseResult ParseFile(const std::string& contents) {
  return Parse({"--config=" + WriteConfig(contents), "--", "true"});
}

// Every default the rest of the helper depends on.
TEST(Config, Defaults) {
  ParseResult r = Parse({"--", "/bin/sh", "-c", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 2);
  EXPECT_EQ(r.config.docker_image_ref, "");
  EXPECT_EQ(r.config.fetcher_socket, "/var/run/fetcher/fetcher.sock");
  EXPECT_EQ(r.config.staging_root, "/var/action_root");
  EXPECT_EQ(r.config.root_mode, RootMode::Overlay);
  EXPECT_FALSE(r.config.isolate_network);
  EXPECT_EQ(r.config.build_uid, 0);
  EXPECT_EQ(r.config.build_gid, 0);
  EXPECT_EQ(r.config.host_uid, 1000);
  EXPECT_EQ(r.config.host_gid, 1000);
  EXPECT_EQ(r.config.keep_list, std::set<std::string>({"dev", "proc", "sys", "tmp"}));
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({"resolv.conf", "hostname", "hosts"}));
}

// The flag-only invocation the action router used before config files existed.
TEST(Config, FlagsOnly) {
  ParseResult r = Parse({"--docker-image-ref=busybox@sha256:abc", "--fetcher-socket=/var/fetcher/fetcher.sock",
                         "--build-user=1000:1001", "--network-isolation", "--", "/usr/bin/make", "-j", "all"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 6);
  EXPECT_EQ(r.config.docker_image_ref, "busybox@sha256:abc");
  EXPECT_EQ(r.config.fetcher_socket, "/var/fetcher/fetcher.sock");
  EXPECT_EQ(r.config.build_uid, 1000);
  EXPECT_EQ(r.config.build_gid, 1001);
  EXPECT_TRUE(r.config.isolate_network);
}

// An explicitly empty --docker-image-ref selects inline mode, the same as
// leaving it out.
TEST(Config, EmptyDockerImageRef) {
  ParseResult r = Parse({"--docker-image-ref", "", "--network-isolation", "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 5);
  EXPECT_EQ(r.config.docker_image_ref, "");
  EXPECT_TRUE(r.config.isolate_network);
}

// CLI11 would take the next flag as the value of "--name=", so it's rejected.
TEST(Config, EmptyValueAfterEqualsIsRejected) {
  for (const std::string& flag : {"--docker-image-ref=", "--fetcher-socket=", "--keep-dirs="}) {
    ParseResult r = Parse({flag, "--network-isolation", "--", "true"});
    EXPECT_FALSE(r.ok) << flag;
    EXPECT_NE(r.error.find("separate argument"), std::string::npos) << flag << ": " << r.error;
  }
}

// Only the helper's own flags are checked, not the action's command.
TEST(Config, EmptyValueAfterEqualsInCommandIsKept) {
  ParseResult r = Parse({"--", "make", "--flag="});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 2);
}

// A single-valued flag given twice is ambiguous, so it's rejected rather than
// resolved by picking one of the values.
TEST(Config, RepeatedScalarFlagIsRejected) {
  for (const std::vector<std::string>& flags : std::vector<std::vector<std::string>>{
           {"--network-isolation", "--no-network-isolation"},
           {"--fetcher-socket=/a.sock", "--fetcher-socket=/b.sock"},
           {"--root-mode=overlay", "--root-mode=tmpfs"},
           {"--build-user=1:1", "--build-user=2:2"},
       }) {
    std::vector<std::string> args = flags;
    args.insert(args.end(), {"--", "true"});
    ParseResult r = Parse(args);
    EXPECT_FALSE(r.ok) << flags[0];
  }
}

// Flag parsing stops at the "--" separator, so the action's own arguments are
// never interpreted as helper flags — not even the ones that would reconfigure
// the sandbox.
TEST(Config, StopsAtCommand) {
  std::string path = WriteConfig("fetcher-socket = \"/from-config.sock\"\n");
  ParseResult r = Parse({"--build-user=7:7", "--", "/bin/sh", "-c", "--config=" + path, "--network-isolation"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 3);
  EXPECT_EQ(r.config.build_uid, 7);
  // Neither the --config nor the --network-isolation after /bin/sh applied.
  EXPECT_EQ(r.config.fetcher_socket, "/var/run/fetcher/fetcher.sock");
  EXPECT_FALSE(r.config.isolate_network);
}

// An action whose own argv starts with something that looks like a helper flag
// is passed through as the command.
TEST(Config, CommandStartingWithFlagIsNotAFlag) {
  std::string path = WriteConfig("host-user = \"1:1\"\n");
  ParseResult r = Parse({"--", "--config=" + path, "--build-user=7:7"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 2);
  EXPECT_EQ(r.config.host_uid, 1000);
  EXPECT_EQ(r.config.host_gid, 1000);
  EXPECT_EQ(r.config.build_uid, 0);
  EXPECT_EQ(r.config.build_gid, 0);
}

// Without the separator we can't tell the helper's flags from the action's, so
// the invocation is rejected rather than guessed at.
TEST(Config, MissingEndOfFlagsSeparator) {
  ParseResult r = Parse({"--build-user=7:7", "/bin/sh", "-c", "true"});
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr("missing \"--\" separator"));

  r = Parse({"--build-user=7:7"});
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr("missing \"--\" separator"));

  r = Parse({});
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr("missing \"--\" separator"));
}

// A command is required by main(), which detects its absence as
// command_start == argc.
TEST(Config, MissingCommand) {
  ParseResult r = Parse({"--build-user=7:7", "--"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 3);
}

TEST(Config, ConfigFile) {
  ParseResult r = ParseFile(R"(
# A comment.
fetcher-socket = "/var/fetcher/fetcher.sock"
root-mode = "tmpfs"
staging-root = "/tmp/bb_chroot_helper"
network-isolation = true
keep-dirs = ["nix", "toolchains"]
etc-files = ["resolv.conf"]
build-user = "1000:1001"
host-user = "65534:65534"
)");
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.fetcher_socket, "/var/fetcher/fetcher.sock");
  EXPECT_EQ(r.config.staging_root, "/tmp/bb_chroot_helper");
  EXPECT_EQ(r.config.root_mode, RootMode::Tmpfs);
  EXPECT_TRUE(r.config.isolate_network);
  EXPECT_EQ(r.config.build_uid, 1000);
  EXPECT_EQ(r.config.build_gid, 1001);
  EXPECT_EQ(r.config.host_uid, 65534);
  EXPECT_EQ(r.config.host_gid, 65534);
  // keep-dirs adds to the built-in entries...
  EXPECT_EQ(r.config.keep_list, std::set<std::string>({"dev", "nix", "proc", "sys", "tmp", "toolchains"}));
  // ...while etc-files replaces them.
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({"resolv.conf"}));
}

// An empty file leaves every default in place.
TEST(Config, EmptyConfigFile) {
  ParseResult r = ParseFile("");
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.build_uid, 0);
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({"resolv.conf", "hostname", "hosts"}));
}

// Empty arrays are honoured: no keep-dirs added, no /etc files bind-mounted.
TEST(Config, EmptyArrays) {
  ParseResult r = ParseFile("keep-dirs = []\netc-files = []\n");
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.keep_list, std::set<std::string>({"dev", "proc", "sys", "tmp"}));
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({}));
}

// The parser can't tell a scalar from a single-element array, so a scalar
// given for a list option is a one-element list, and a one-element array given
// for a single-valued option is that value.
TEST(Config, SingleValuesAndSingleElementArrays) {
  ParseResult r = ParseFile("keep-dirs = \"nix\"\netc-files = \"hosts\"\nfetcher-socket = [\"/a.sock\"]\n");
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.keep_list, std::set<std::string>({"dev", "nix", "proc", "sys", "tmp"}));
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({"hosts"}));
  EXPECT_EQ(r.config.fetcher_socket, "/a.sock");
}

// Flags win over the config file regardless of where they sit relative to
// --config.
TEST(Config, FlagsOverrideConfigFile) {
  std::string path = WriteConfig(R"(
fetcher-socket = "/from-config.sock"
root-mode = "tmpfs"
staging-root = "/var/from-config"
network-isolation = true
build-user = "1000:1000"
etc-files = ["hosts"]
)");
  ParseResult r = Parse({"--build-user=5:6", "--config=" + path, "--fetcher-socket=/from-flag.sock",
                         "--staging-root=/tmp/from-flag", "--root-mode=overlay", "--no-network-isolation",
                         "--etc-files=resolv.conf,hostname", "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.build_uid, 5);
  EXPECT_EQ(r.config.build_gid, 6);
  EXPECT_EQ(r.config.fetcher_socket, "/from-flag.sock");
  EXPECT_EQ(r.config.staging_root, "/tmp/from-flag");
  EXPECT_EQ(r.config.root_mode, RootMode::Overlay);
  EXPECT_FALSE(r.config.isolate_network);
  // A list given as a flag replaces the file's list rather than merging with it.
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({"resolv.conf", "hostname"}));

  // Settings the flags don't mention still come from the file.
  r = Parse({"--config=" + path, "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.build_uid, 1000);
  EXPECT_EQ(r.config.fetcher_socket, "/from-config.sock");
  EXPECT_EQ(r.config.staging_root, "/var/from-config");
  EXPECT_EQ(r.config.root_mode, RootMode::Tmpfs);
  EXPECT_TRUE(r.config.isolate_network);
}

struct ConfigFileErrorCase {
  const char* contents;
  const char* want_error;
};

class ConfigFileError : public ::testing::TestWithParam<ConfigFileErrorCase> {};

TEST_P(ConfigFileError, IsRejected) {
  ParseResult r = ParseFile(GetParam().contents);
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr(GetParam().want_error));
}

INSTANTIATE_TEST_SUITE_P(
    Config, ConfigFileError,
    ::testing::Values(
        // Unknown and misspelled keys are fatal rather than ignored.
        ConfigFileErrorCase{"buildd-user = \"1:1\"\n", "not able to parse buildd-user"},
        // The old table form of the users is an unknown key too.
        ConfigFileErrorCase{"[build-user]\nuid = 0\ngid = 0\n", "not able to parse build-user"},
        // Flags that only make sense on the command line.
        ConfigFileErrorCase{"docker-image-ref = \"busybox\"\n", "not allowed in a configuration file"},
        ConfigFileErrorCase{"config = \"/other.toml\"\n", "not allowed in a configuration file"},
        ConfigFileErrorCase{"root-mode = \"chroot\"\n", "chroot not in {overlay,tmpfs}"},
        ConfigFileErrorCase{"network-isolation = \"maybe\"\n", "network-isolation"},
        // A half-specified user would silently fall back to id 0.
        ConfigFileErrorCase{"build-user = \"1000\"\n", "want UID:GID"},
        ConfigFileErrorCase{"host-user = \":1000\"\n", "want UID:GID"},
        ConfigFileErrorCase{"build-user = \"1000000000:0\"\n", "want UID:GID"},
        // Entries that are not a single path component either never match or
        // reach outside the directory they're resolved against.
        ConfigFileErrorCase{"keep-dirs = [\"var/lib\"]\n", "single path component"},
        ConfigFileErrorCase{"keep-dirs = [\"..\"]\n", "single path component"},
        ConfigFileErrorCase{"etc-files = [\"../shadow\"]\n", "single path component"},
        // A single-valued option given several values, either as an array
        // or by repeating the key.
        ConfigFileErrorCase{"fetcher-socket = [\"/a.sock\", \"/b.sock\"]\n", "--fetcher-socket: At most 1"},
        ConfigFileErrorCase{"root-mode = [\"overlay\", \"tmpfs\"]\n", "--root-mode: At most 1"},
        ConfigFileErrorCase{"network-isolation = [true, false]\n", "too many inputs for a flag"},
        ConfigFileErrorCase{"build-user = [\"1:1\", \"2:2\"]\n", "--build-user: At most 1"},
        ConfigFileErrorCase{"staging-root = \"/var/a\"\nstaging-root = \"/var/b\"\n", "--staging-root: At most 1"}));

TEST(Config, InvalidUserFlag) {
  for (const char* flag : {"--build-user=", "--host-user="}) {
    for (const char* value : {"1000", "-1:0", "a:b", ":", "1000:"}) {
      ParseResult r = Parse({std::string(flag) + value, "--", "true"});
      EXPECT_FALSE(r.ok) << flag << value;
      EXPECT_THAT(r.error, HasSubstr("want UID:GID"));
    }
  }
}

// Every config file setting is also a flag.
TEST(Config, ListFlags) {
  ParseResult r =
      Parse({"--keep-dirs=nix", "--keep-dirs=runner,toolchains", "--etc-files=hosts", "--host-user=5:6", "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.keep_list, std::set<std::string>({"dev", "nix", "proc", "runner", "sys", "tmp", "toolchains"}));
  EXPECT_EQ(r.config.etc_files, std::vector<std::string>({"hosts"}));
  EXPECT_EQ(r.config.host_uid, 5);
  EXPECT_EQ(r.config.host_gid, 6);

  r = Parse({"--keep-dirs=../etc", "--", "true"});
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr("single path component"));
}

TEST(Config, EmptyConfigFlag) {
  ParseResult r = Parse({"--config=", "--", "true"});
  EXPECT_FALSE(r.ok);
}

// A config file that isn't there is an error rather than a silent fallback to
// the built-in defaults.
TEST(Config, MissingConfigFile) {
  ParseResult r = Parse({"--config=/nonexistent/bb_chroot_helper.toml", "--", "true"});
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr("/nonexistent/bb_chroot_helper.toml was not readable"));
}

// The host user is the one privileges are dropped to, so id 0 there would leave
// the helper running as real root.
TEST(Config, HostUserRootIsRejected) {
  for (const char* table : {"host-user = \"0:1000\"\n", "host-user = \"1000:0\"\n"}) {
    ParseResult r = ParseFile(table);
    ASSERT_TRUE(r.ok) << r.error;
    std::string error;
    EXPECT_FALSE(validate_config(r.config, &error)) << table;
    EXPECT_THAT(error, HasSubstr("host-user uid and gid must not be 0"));
  }
  // The build user is the in-namespace mapping, where id 0 is the default.
  ParseResult r = ParseFile("build-user = \"0:0\"\n");
  ASSERT_TRUE(r.ok) << r.error;
  std::string error;
  EXPECT_TRUE(validate_config(r.config, &error)) << error;
}

TEST(Config, RootModeValidation) {
  for (const char* mode : {"overlay", "tmpfs", "", "unknown"}) {
    ParseResult r = Parse({std::string("--root-mode=") + mode, "--", "true"});
    bool expected = std::string(mode) == "overlay" || std::string(mode) == "tmpfs";
    EXPECT_EQ(r.ok, expected) << mode << ": " << r.error;
    if (expected) {
      EXPECT_EQ(r.config.root_mode, std::string(mode) == "overlay" ? RootMode::Overlay : RootMode::Tmpfs);
    }
  }
}

TEST(Config, InvalidStagingRootIsRejected) {
  for (const char* staging_root : {"relative/path", "/stage", "/var/../stage", "/var//stage"}) {
    ParseResult r = Parse({std::string("--staging-root=") + staging_root, "--", "true"});
    ASSERT_TRUE(r.ok) << r.error;
    std::string error;
    EXPECT_FALSE(validate_config(r.config, &error)) << staging_root;
    EXPECT_THAT(error, HasSubstr("staging-root must be a normalized absolute path below a top-level directory"));
  }
}

// The usage message is generated from the option table, so check that every
// option still shows up in it.
TEST(Config, UsageMentionsEveryFlag) {
  std::string text = usage();
  for (const char* flag :
       {"--config", "--docker-image-ref", "--fetcher-socket", "--staging-root", "--root-mode", "--build-user",
        "--host-user", "--network-isolation", "--no-network-isolation", "--keep-dirs", "--etc-files"}) {
    EXPECT_THAT(text, HasSubstr(flag));
  }
  EXPECT_THAT(text, HasSubstr("-- <command> [args...]"));
}

}  // namespace
