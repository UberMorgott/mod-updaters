package main

import "github.com/UberMorgott/mod-updaters/engine"

// Server is the shared SFTP server holding all modpacks. No build tag — used by
// every game config.
var Server = engine.ServerConfig{
	Host:       "morgott.keenetic.pro:22",
	Login:      "modman",
	Password:   "Br2ctG7FGSqPhr4",
	RemoteBase: "/tmp/mnt/01DB6F2D5E1A6080/modpacks/",
}
