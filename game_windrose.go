//go:build windrose

package main

import "github.com/UberMorgott/mod-updaters/engine"

var gameConfig = engine.Config{
	GameName:       "Windrose",
	GameExecutable: "Windrose.exe",
	LaunchArgs:     []string{"-console"},
	Version:        "1.0.0",
	Server:         Server,
	RemoteSubdir:   "Windrose/",
	Cleanup: []engine.CleanupSpec{
		{Path: "R5/Binaries/Win64/ue4ss/Mods", Mode: engine.MirrorSubdirs},
		{Path: "R5/Content/Paks/~mods/~mods", Mode: engine.MirrorSubdirs},
	},
}
