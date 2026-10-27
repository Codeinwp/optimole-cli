//go:build !windows

package upload

// pathIdentity is only needed where FileInfo carries no file ID (Windows).
func pathIdentity(string) (any, bool) { return nil, false }
