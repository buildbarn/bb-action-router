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

// Parse a config file on its own, with no flags involved.
ParseResult ParseFile(const std::string& contents) {
  ParseResult result;
  result.command_start = -1;
  result.ok = parse_config_file(WriteConfig(contents), &result.config, &result.error);
  return result;
}

// Every default the rest of the helper depends on.
TEST(Config, Defaults) {
  ParseResult r = Parse({"--", "/bin/sh", "-c", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.command_start, 2);
  EXPECT_EQ(r.config.docker_image_ref, "");
  EXPECT_EQ(r.config.fetcher_socket, "/var/run/fetcher/fetcher.sock");
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

TEST(Config, LastRepeatedFlagWins) {
  ParseResult r = Parse({"--network-isolation", "--no-network-isolation", "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_FALSE(r.config.isolate_network);
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
  std::string path = WriteConfig("[host-user]\nuid = 1\ngid = 1\n");
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
  EXPECT_THAT(r.error, HasSubstr("unrecognized flag \"/bin/sh\""));

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
network-isolation = true
keep-dirs = ["nix", "toolchains"]
etc-files = ["resolv.conf"]

[build-user]
uid = 1000
gid = 1001

[host-user]
uid = 65534
gid = 65534
)");
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.fetcher_socket, "/var/fetcher/fetcher.sock");
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

// Flags win over the config file regardless of where they sit relative to
// --config.
TEST(Config, FlagsOverrideConfigFile) {
  std::string path = WriteConfig(R"(
fetcher-socket = "/from-config.sock"
network-isolation = true

[build-user]
uid = 1000
gid = 1000
)");
  ParseResult r = Parse({"--build-user=5:6", "--config=" + path, "--fetcher-socket=/from-flag.sock",
                         "--no-network-isolation", "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.build_uid, 5);
  EXPECT_EQ(r.config.build_gid, 6);
  EXPECT_EQ(r.config.fetcher_socket, "/from-flag.sock");
  EXPECT_FALSE(r.config.isolate_network);

  // Settings the flags don't mention still come from the file.
  r = Parse({"--config=" + path, "--", "true"});
  ASSERT_TRUE(r.ok) << r.error;
  EXPECT_EQ(r.config.build_uid, 1000);
  EXPECT_EQ(r.config.fetcher_socket, "/from-config.sock");
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
        ConfigFileErrorCase{"buildd-user = 1\n", "unknown key \"buildd-user\""},
        ConfigFileErrorCase{"[build-user]\nuid = 0\ngid = 0\nname = \"root\"\n", "unknown key \"build-user.name\""},
        // The image ref is per-action and gets a pointed error of its own.
        ConfigFileErrorCase{"docker-image-ref = \"busybox\"\n", "pass --docker-image-ref instead"},
        // Type errors.
        ConfigFileErrorCase{"fetcher-socket = 7\n", "fetcher-socket must be a string"},
        ConfigFileErrorCase{"network-isolation = \"true\"\n", "network-isolation must be true or false"},
        ConfigFileErrorCase{"keep-dirs = \"nix\"\n", "keep-dirs must be an array of strings"},
        ConfigFileErrorCase{"etc-files = [1]\n", "etc-files must be an array of strings"},
        ConfigFileErrorCase{"build-user = 1000\n", "build-user must be a table with uid and gid keys"},
        ConfigFileErrorCase{"[build-user]\nuid = \"1000\"\ngid = 0\n", "build-user.uid must be an integer"},
        // A half-specified user would silently fall back to id 0.
        ConfigFileErrorCase{"[build-user]\nuid = 1000\n", "build-user.gid is required"},
        ConfigFileErrorCase{"[host-user]\ngid = 1000\n", "host-user.uid is required"},
        // Ids have to be plausible.
        ConfigFileErrorCase{"[build-user]\nuid = -1\ngid = 0\n",
                            "build-user.uid must be an integer between 0 and 999999999"},
        ConfigFileErrorCase{"[build-user]\nuid = 1000000000\ngid = 0\n",
                            "build-user.uid must be an integer between 0 and 999999999"},
        // Entries that are not a single path component either never match or
        // reach outside the directory they're resolved against.
        ConfigFileErrorCase{"keep-dirs = [\"var/lib\"]\n", "keep-dirs entries must be a single path component"},
        ConfigFileErrorCase{"keep-dirs = [\"..\"]\n", "keep-dirs entries must be a single path component"},
        ConfigFileErrorCase{"keep-dirs = [\"\"]\n", "keep-dirs entries must be a single path component"},
        ConfigFileErrorCase{"etc-files = [\"../shadow\"]\n", "etc-files entries must be a single path component"},
        // Malformed TOML is reported by the parser itself.
        ConfigFileErrorCase{"fetcher-socket =\n", "expected value, saw '\\n'"},
        ConfigFileErrorCase{"fetcher-socket = unquoted\n", "could not determine value type"}));

// Errors point at the offending line.
TEST(Config, ErrorNamesLine) {
  ParseResult r = ParseFile("# comment\n\nfetcher-socket = \"/a\"\nbogus = 1\n");
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr(":4:"));
}

TEST(Config, InvalidBuildUserFlag) {
  for (const char* value : {"1000", "-1:0", "a:b", ":", "1000:"}) {
    ParseResult r = Parse({std::string("--build-user=") + value, "--", "true"});
    EXPECT_FALSE(r.ok) << value;
    EXPECT_THAT(r.error, HasSubstr("invalid --build-user"));
  }
}

TEST(Config, EmptyConfigFlag) {
  ParseResult r = Parse({"--config=", "--", "true"});
  EXPECT_FALSE(r.ok);
  EXPECT_EQ(r.error, "empty --config path");
}

// A config file that isn't there is an error rather than a silent fallback to
// the built-in defaults.
TEST(Config, MissingConfigFile) {
  ParseResult r = Parse({"--config=/nonexistent/bb_chroot_helper.toml", "--", "true"});
  EXPECT_FALSE(r.ok);
  EXPECT_THAT(r.error, HasSubstr("cannot read config file /nonexistent/bb_chroot_helper.toml"));
}

// The host user is the one privileges are dropped to, so id 0 there would leave
// the helper running as real root.
TEST(Config, HostUserRootIsRejected) {
  for (const char* table : {"[host-user]\nuid = 0\ngid = 1000\n", "[host-user]\nuid = 1000\ngid = 0\n"}) {
    ParseResult r = ParseFile(table);
    ASSERT_TRUE(r.ok) << r.error;
    std::string error;
    EXPECT_FALSE(validate_config(r.config, &error)) << table;
    EXPECT_THAT(error, HasSubstr("host-user uid and gid must not be 0"));
  }
  // The build user is the in-namespace mapping, where id 0 is the default.
  ParseResult r = ParseFile("[build-user]\nuid = 0\ngid = 0\n");
  ASSERT_TRUE(r.ok) << r.error;
  std::string error;
  EXPECT_TRUE(validate_config(r.config, &error)) << error;
}

// The usage message is generated from the flag table, so check that every flag
// still shows up in it.
TEST(Config, UsageMentionsEveryFlag) {
  std::string text = usage();
  for (const char* flag : {"--config=PATH", "--docker-image-ref=REF", "--fetcher-socket=PATH", "--build-user=UID:GID",
                           "--network-isolation]", "--no-network-isolation]"}) {
    EXPECT_THAT(text, HasSubstr(flag));
  }
  EXPECT_THAT(text, HasSubstr("-- <command> [args...]"));
}

}  // namespace
