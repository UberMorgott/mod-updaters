package main

import "github.com/UberMorgott/mod-updaters/engine"

// Populated at build time from gitignored config.txt via -ldflags -X (build.bat).
// Empty in source — the public repo must contain no server address or credentials.
var (
	cfgHost, cfgLogin, cfgPassword, cfgRemoteBase, cfgHostKey string
)

// Server is the shared SFTP server holding all modpacks. No build tag — used by
// every game config.
var Server = engine.ServerConfig{
	Host:       cfgHost,
	Login:      cfgLogin,
	Password:   cfgPassword,
	RemoteBase: cfgRemoteBase,
	HostKey:    cfgHostKey,
}
