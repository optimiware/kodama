package app

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// Config contains process settings. Module-specific connections are wired by
// their integration task; process readiness does not claim dependency readiness.
type Config struct {
	HTTPAddr        string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// Assembly selects what `kodama server` composes: AssemblyScaffold (the
	// default health-only shell) or AssemblyCore (the T-070 core slice).
	Assembly string
}

func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{HTTPAddr: ":8080", LogLevel: slog.LevelInfo, ShutdownTimeout: 10 * time.Second, Assembly: AssemblyScaffold}
	if v := getenv("KODAMA_ASSEMBLY"); v != "" {
		if v != AssemblyScaffold && v != AssemblyCore {
			return c, fmt.Errorf("KODAMA_ASSEMBLY must be %q or %q", AssemblyScaffold, AssemblyCore)
		}
		c.Assembly = v
	}
	if v := getenv("KODAMA_HTTP_ADDR"); v != "" {
		c.HTTPAddr = v
	}
	_, port, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil {
		return c, fmt.Errorf("KODAMA_HTTP_ADDR must be host:port: %w", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return c, fmt.Errorf("KODAMA_HTTP_ADDR port must be between 0 and 65535")
	}
	if v := getenv("KODAMA_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return c, fmt.Errorf("KODAMA_LOG_LEVEL: %w", err)
		}
	}
	if v := getenv("KODAMA_SHUTDOWN_TIMEOUT"); v != "" {
		c.ShutdownTimeout, err = time.ParseDuration(v)
		if err != nil || c.ShutdownTimeout <= 0 || c.ShutdownTimeout > time.Minute {
			return c, fmt.Errorf("KODAMA_SHUTDOWN_TIMEOUT must be a duration in (0, 1m]")
		}
	}
	return c, nil
}
