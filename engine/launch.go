package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchGame starts the game executable with /high priority, detached.
func launchGame(cfg Config) {
	args := append([]string{"/C", "start", "/B", "/high", cfg.GameExecutable}, cfg.LaunchArgs...)
	cmd := exec.Command("cmd", args...)
	cmd.Dir = "."
	cmd.Start()
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
