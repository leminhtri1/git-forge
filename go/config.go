package main

// Process-wide settings, read once from the environment (the Go twin of src/config.ts).
//
// The forge serves one account: the login the bearer token names. The suite's token is opaque, so the
// login is configuration: FORGE_LOGIN, else the suite's fixed scenario viewer. Every stored timestamp
// derives from MAKEFUN_TRIAL_NOW, never the wall clock, so two runs build byte-identical state.

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Identity struct {
	Login string
	ID    int
	Name  string
	Email string
}

type Config struct {
	Token         string
	Viewer        Identity
	NowSec        int64
	Version       string
	DefaultBranch string
}

var config Config

var digitsRE = regexp.MustCompile(`^\d+$`)

func parseNow(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if digitsRE.MatchString(raw) {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err == nil {
				if n > 1e11 {
					return n / 1000
				}
				return n
			}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.Unix()
			}
		}
	}
	return 1_767_225_600 // 2026-01-01T00:00:00Z: a fixed fallback, never the clock
}

func loadConfig() {
	login := strings.TrimSpace(os.Getenv("FORGE_LOGIN"))
	if login == "" {
		login = "adaiversen"
	}
	name := strings.TrimSpace(os.Getenv("FORGE_NAME"))
	if name == "" {
		name = login
	}
	config = Config{
		Token:         strings.TrimSpace(os.Getenv("MAKEFUN_SHOP_TOKEN")),
		Viewer:        Identity{Login: login, ID: 1000, Name: name, Email: login + "@users.noreply.forge.local"},
		NowSec:        parseNow(os.Getenv("MAKEFUN_TRIAL_NOW")),
		Version:       "git-forge 1.0.0",
		DefaultBranch: "main",
	}
}

// isoAt is an ISO-8601 timestamp offset seconds after the trial's now (GitHub's format, no millis).
func isoAt(offset int64) string {
	return time.Unix(config.NowSec+offset, 0).UTC().Format("2006-01-02T15:04:05Z")
}
