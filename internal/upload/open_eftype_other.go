//go:build darwin || dragonfly || freebsd || linux || openbsd

package upload

func isEFTYPE(error) bool { return false }
