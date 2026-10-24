package runtimehttp_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

func readFixture() ([]byte, error) {
	return os.ReadFile(filepath.Join("..", "..", "..", "contracts", "runtime", "v1", "fixtures", "scenarios.json"))
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
