//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package export

import "os"

// noFollow is not available: callers check with Lstat before opening.
const noFollow = 0

// lockFile does nothing here: there is no portable lock.
func lockFile(*os.File) error { return nil }
