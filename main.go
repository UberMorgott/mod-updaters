//go:build valheim || windrose

package main

import "github.com/UberMorgott/mod-updaters/engine"

// main runs the shared engine with the active game's config. engine.Run performs
// the sanity check (game exe present, Cleanup paths safe) before driving the TUI.
func main() {
	engine.Run(gameConfig)
}
