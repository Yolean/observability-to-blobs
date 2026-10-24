package main

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// The user-level host agent's config file, as developer-feed/lib/config.sh
// keeps it: KEY=VALUE lines in $OBSERVABILITY_HOST_CONFIG/env. Read only:
// the backfill produces nothing that belongs there.

var configKey = regexp.MustCompile(`^[A-Z0-9_]+$`)

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

// setting is the environment's value, else the config file's.
func setting(cfg map[string]string, key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := cfg[key]; v != "" {
		return v
	}
	return def
}
