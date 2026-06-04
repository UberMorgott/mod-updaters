//go:build valheim

package main

import "github.com/UberMorgott/mod-updaters/engine"

var gameConfig = engine.Config{
	GameName:       "Valheim",
	GameExecutable: "valheim.exe",
	LaunchArgs:     []string{"-console"},
	Version:        "1.0.0",
	Server:         Server,
	RemoteSubdir:   "Valheim/",
	Cleanup: []engine.CleanupSpec{
		{Path: "BepInEx/plugins", Mode: engine.MirrorFiles},
	},
}
