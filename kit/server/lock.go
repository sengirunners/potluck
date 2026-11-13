package server

import "github.com/sengirunners/potluck/kit/config"

// FileLock is held by the running daemon for its whole life.
const FileLock = config.FileHomeLock

// LockHome takes the one-daemon-per-ZM_HOME lock (config.LockHome).
func LockHome(home string) (release func(), err error) { return config.LockHome(home) }
