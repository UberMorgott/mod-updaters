package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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
// exist next to the updater, and every Cleanup path must be a clean relative
// forward-slash path inside the game root. Ported from WUR's sanityCheck.
func SanityCheck(cfg Config) error {
	for _, spec := range cfg.Cleanup {
		if !validCleanupPath(spec.Path) {
			return fmt.Errorf("unsafe Cleanup path: %q", spec.Path)
		}
	}
	if _, err := os.Stat(cfg.GameExecutable); err != nil {
		return fmt.Errorf("%s не найден рядом с загрузчиком — запусти из корня папки %s",
			cfg.GameExecutable, cfg.GameName)
	}
	return nil
}

// validCleanupPath reports whether p is safe to mirror: non-empty, not the game
// root itself, no "..", not absolute, no backslashes, and in clean form (a
// trailing "/" or "a//b" would never match the SFTP keep-set and would make
// the mirror wipe everything under it).
func validCleanupPath(p string) bool {
	if p == "" || p == "." || p != path.Clean(p) {
		return false
	}
	_, err := filepath.Localize(p)
	return err == nil
}
