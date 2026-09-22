package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchGame starts the game executable with /high priority, detached.
func launchGame(cfg Config) {
	args := append([]string{"/C", "start", "/B", "/high", cfg.GameExecutable}, cfg.LaunchArgs...)
	cmd := exec.CommandContext(context.Background(), "cmd", args...) //nolint:gosec // G204: executable/args come from the build-time game config, not user input
	cmd.Dir = "."
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка запуска игры:", err)
	}
}

// SanityCheck validates the config before running: the game executable must
// exist next to the updater, and every Cleanup path must be relative (no "..",
// not absolute). Ported from WUR's sanityCheck.
func SanityCheck(cfg Config) error {
	for _, spec := range cfg.Cleanup {
		p := spec.Path
		if p == "" || filepath.IsAbs(p) || strings.Contains(p, "..") {
			return fmt.Errorf("unsafe Cleanup path: %q", p)
		}
	}
	if _, err := os.Stat(cfg.GameExecutable); err != nil {
		return fmt.Errorf("%s не найден рядом с загрузчиком — запусти из корня папки %s",
			cfg.GameExecutable, cfg.GameName)
	}
	return nil
}
