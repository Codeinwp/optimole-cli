package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPath(t *testing.T) {
	if got, want := Path("/home/u/.config"), filepath.Join("/home/u/.config", "optimole", "config.json"); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestSaveLoadRemove(t *testing.T) {
	p := Path(t.TempDir())
	if _, found, err := Load(p); found || err != nil {
		t.Fatalf("Load(missing) = found %v, err %v", found, err)
	}
	if err := Save(p, File{APIKey: "key-one-123"}); err != nil {
		t.Fatal(err)
	}
	f, found, err := Load(p)
	if err != nil || !found || f.APIKey != "key-one-123" {
		t.Fatalf("Load = %+v, %v, %v", f, found, err)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, filepath.Dir(p), 0o700)
		assertMode(t, p, 0o600)
	}
	if LoosePermissions(p) {
		t.Fatal("fresh config reported as loose")
	}
	removed, err := Remove(p)
	if !removed || err != nil {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	if removed, err := Remove(p); removed || err != nil {
		t.Fatalf("second Remove = %v, %v", removed, err)
	}
}

func TestSaveTightensExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	p := Path(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"api_key":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if !LoosePermissions(p) {
		t.Fatal("0644 config not reported as loose")
	}
	if err := Save(p, File{APIKey: "new-key-1234"}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Dir(p), 0o700)
	assertMode(t, p, 0o600)
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func assertMode(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", p, got, want)
	}
}

func TestLoadInvalidJSONDoesNotEchoContent(t *testing.T) {
	p := Path(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`secret-key-material`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(p)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	p := Path(t.TempDir())
	if err := Save(p, File{APIKey: "file-key-1234"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		in      Inputs
		key     string
		keySrc  Source
		url     string
		urlSrc  Source
		wantErr error
	}{
		{"flag wins", Inputs{FlagAPIKey: "flag-key-1234", FlagAPIURL: "https://flag/api", Getenv: env(map[string]string{EnvAPIKey: "env-key-1234", EnvAPIURL: "https://env/api"}), ConfigPath: p},
			"flag-key-1234", SourceFlag, "https://flag/api", SourceFlag, nil},
		{"env beats file", Inputs{Getenv: env(map[string]string{EnvAPIKey: "env-key-1234", EnvAPIURL: "https://env/api"}), ConfigPath: p},
			"env-key-1234", SourceEnv, "https://env/api", Source(EnvAPIURL), nil},
		{"file last", Inputs{Getenv: env(nil), ConfigPath: p},
			"file-key-1234", SourceFile, DefaultAPIURL, SourceDefault, nil},
		{"blank values are ignored", Inputs{FlagAPIKey: "  ", Getenv: env(map[string]string{EnvAPIKey: " ", EnvAPIURL: " "}), ConfigPath: p},
			"file-key-1234", SourceFile, DefaultAPIURL, SourceDefault, nil},
		{"no key anywhere", Inputs{Getenv: env(nil), ConfigPath: filepath.Join(t.TempDir(), "none.json")},
			"", SourceNone, DefaultAPIURL, SourceDefault, ErrNoAPIKey},
	}
	for _, tc := range cases {
		s, err := Resolve(tc.in)
		if !errors.Is(err, tc.wantErr) || s.APIKey != tc.key || s.KeySource != tc.keySrc || s.APIURL != tc.url || s.URLSource != tc.urlSrc {
			t.Errorf("%s: got %+v, %v", tc.name, s, err)
		}
	}
}

func TestResolveSkipsConfigFileWhenKeyIsGiven(t *testing.T) {
	p := Path(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Inputs{FlagAPIKey: "flag-key-1234", ConfigPath: p}); err != nil {
		t.Fatalf("a broken config file broke --api-key: %v", err)
	}
	if _, err := Resolve(Inputs{ConfigPath: p}); err == nil || errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("a broken config file was not reported: %v", err)
	}
}
