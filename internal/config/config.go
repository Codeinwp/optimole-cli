// Package config resolves the API key and base URL, and stores the key saved
// by "optimole login".
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables.
const (
	EnvAPIKey = "OPTIMOLE_API_KEY"
	EnvAPIURL = "OPTIMOLE_API_URL"
)

// DefaultAPIURL is the production dashboard API.
const DefaultAPIURL = "https://dashboard.optimole.com/api"

// Source says where a setting came from.
type Source string

// Setting sources, in precedence order.
const (
	SourceFlag    Source = "--api-key flag"
	SourceEnv     Source = EnvAPIKey
	SourceFile    Source = "config file"
	SourceDefault Source = "default"
	SourceNone    Source = ""
)

// File is the saved configuration.
type File struct {
	APIKey string `json:"api_key"`
}

// Path returns the config file path below a user config directory
// (os.UserConfigDir): <dir>/optimole/config.json.
func Path(userConfigDir string) string {
	return filepath.Join(userConfigDir, "optimole", "config.json")
}

// ErrNoAPIKey means no source provided an API key.
var ErrNoAPIKey = errors.New("no API key: run `optimole login`, set " + EnvAPIKey + ", or pass --api-key")

// Load reads the config file. A missing file is not an error: found is false.
func Load(path string) (f File, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, true, fmt.Errorf("reading %s: invalid JSON", path)
	}
	f.APIKey = strings.TrimSpace(f.APIKey)
	return f, true, nil
}

// Save writes the config file atomically: the directory is 0700 and the file
// 0600 (on Windows the permission bits have no effect beyond read-only).
func Save(path string, f File) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("securing %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// Remove deletes the config file. removed is false when there was none.
func Remove(path string) (removed bool, err error) {
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// LoosePermissions reports whether a config file can be read by other users.
// It is always false on Windows.
func LoosePermissions(path string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o077 != 0
}

// Inputs are the places a setting can come from.
type Inputs struct {
	FlagAPIKey string
	FlagAPIURL string
	Getenv     func(string) string
	// ConfigPath is read only when neither the flag nor the environment
	// gives a key.
	ConfigPath string
}

// Settings are the resolved settings.
type Settings struct {
	APIKey    string
	KeySource Source
	APIURL    string
	URLSource Source
	// LooseConfig is set when the key came from a config file other users
	// can read.
	LooseConfig bool
}

// ResolveURL picks the base URL: --api-url, then OPTIMOLE_API_URL, then the
// production default. It does not validate it.
func ResolveURL(in Inputs) (string, Source) {
	if v := strings.TrimSpace(in.FlagAPIURL); v != "" {
		return v, SourceFlag
	}
	if in.Getenv != nil {
		if v := strings.TrimSpace(in.Getenv(EnvAPIURL)); v != "" {
			return v, Source(EnvAPIURL)
		}
	}
	return DefaultAPIURL, SourceDefault
}

// Resolve picks the API key (--api-key, then OPTIMOLE_API_KEY, then the
// config file) and the base URL. It returns ErrNoAPIKey when there is no key.
func Resolve(in Inputs) (Settings, error) {
	var s Settings
	s.APIURL, s.URLSource = ResolveURL(in)
	switch {
	case strings.TrimSpace(in.FlagAPIKey) != "":
		s.APIKey, s.KeySource = strings.TrimSpace(in.FlagAPIKey), SourceFlag
	case in.Getenv != nil && strings.TrimSpace(in.Getenv(EnvAPIKey)) != "":
		s.APIKey, s.KeySource = strings.TrimSpace(in.Getenv(EnvAPIKey)), SourceEnv
	case in.ConfigPath != "":
		f, found, err := Load(in.ConfigPath)
		if err != nil {
			return s, err
		}
		if found && f.APIKey != "" {
			s.APIKey, s.KeySource = f.APIKey, SourceFile
			s.LooseConfig = LoosePermissions(in.ConfigPath)
		}
	}
	if s.APIKey == "" {
		return s, ErrNoAPIKey
	}
	return s, nil
}
