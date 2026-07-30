// Package logging configures the process-wide structured logger.
package logging

import (
	"fmt"
	"log/slog"
	"os"
)

// LevelEnvVar is the environment variable holding the minimum level to log at.
// Accepted values are the slog level names (debug, info, warn, error), with an
// optional offset, e.g. "debug-4" for even more verbosity than debug.
const LevelEnvVar = "LOG_LEVEL"

// DefaultLevel is the level used when LevelEnvVar is unset or empty.
const DefaultLevel = slog.LevelInfo

// Configure installs a text handler on stderr as the default slog logger, at
// the level requested through LevelEnvVar.
//
// Lines written through the standard "log" package — which is what our
// dependencies use — are routed to the same handler at level info, so they are
// suppressed if the requested level is above that.
//
// It returns an error if LevelEnvVar holds something that isn't a level name.
func Configure() error {
	level := DefaultLevel
	if s := os.Getenv(LevelEnvVar); s != "" {
		if err := level.UnmarshalText([]byte(s)); err != nil {
			return fmt.Errorf("invalid %s value %q: %w", LevelEnvVar, s, err)
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
	return nil
}
