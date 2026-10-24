package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The user-level host agent's config file, as developer-feed/lib/config.sh
// keeps it: KEY=VALUE lines in $OBSERVABILITY_HOST_CONFIG/env (default
// ~/.config/observability-host/v1/env), never a secret. dev-feed-r2 takes
// its defaults from there, after its flags and the environment, and writes
// back the non-secrets it produces, saying what it changed.

var (
	configKey   = regexp.MustCompile(`^[A-Z0-9_]+$`)
	configValue = regexp.MustCompile(`^[A-Za-z0-9._:/@+,=-]*$`)
)

func configFile() string {
	dir := os.Getenv("OBSERVABILITY_HOST_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "observability-host", "v1")
	}
	return filepath.Join(dir, "env")
}

// readConfig is the file's keys and values; a missing file is none. The
// last line of a key wins, as when the shell sources it.
func readConfig(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && configKey.MatchString(k) {
			m[k] = v
		}
	}
	return m
}

// setting is a flag's default: the environment, else the config file.
func setting(cfg map[string]string, key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := cfg[key]; v != "" {
		return v
	}
	return def
}

// setConfig writes key=value to the file, creating it and its directory
// readable by the user only, and says on w whether it was added, changed
// (from what) or already so: lib/config.sh's cfg_set.
func setConfig(path, key, value string, w io.Writer) error {
	if !configKey.MatchString(key) {
		return fmt.Errorf("config: bad key %q", key)
	}
	if !configValue.MatchString(value) {
		return fmt.Errorf("config: %s: value %q has characters env can't hold", key, value)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = []string{"# The user-level host agent's settings (developer-feed/macos/README.md).",
			"# Maintained by the setup scripts, which say what they change; edits stay.",
			"# No secrets: the bucket credential is in the login Keychain."}
	}
	old, found := "", false
	for i, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok && k == key {
			old, found = v, true
			lines[i] = key + "=" + value
		}
	}
	switch {
	case found && old == value:
		fmt.Fprintf(w, "config: %s=%s (unchanged)\n", key, value)
		return nil
	case found:
		fmt.Fprintf(w, "config: %s=%s (was %s)\n", key, value, old)
	default:
		lines = append(lines, key+"="+value)
		fmt.Fprintf(w, "config: %s=%s (added)\n", key, value)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
