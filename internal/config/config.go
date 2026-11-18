// Package config parses the application's environment configuration.
package config

import (
	"errors"
	"sort"
	"strings"
	"time"
)

const ownerTokenPrefix = "GITHUB_TOKEN_"

// Config contains the configuration needed to poll GitHub notifications.
type Config struct {
	Token        string
	Mode         string
	PollInterval time.Duration
	OwnerTokens  map[string]string
}

// Parse parses configuration from environment entries in KEY=value form.
func Parse(environ []string) (Config, error) {
	cfg := Config{
		Mode:         "log",
		PollInterval: time.Minute,
		OwnerTokens:  make(map[string]string),
	}
	var mode, interval string
	var modeSet, intervalSet bool
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "GITHUB_TOKEN":
			cfg.Token = value
		case "MODE":
			mode, modeSet = value, true
		case "POLL_INTERVAL":
			interval, intervalSet = value, true
		default:
			if suffix, found := strings.CutPrefix(key, ownerTokenPrefix); found && suffix != "" {
				suffix = ownerSuffix(suffix)
				if value == "" {
					delete(cfg.OwnerTokens, suffix)
				} else {
					cfg.OwnerTokens[suffix] = value
				}
			}
		}
	}
	if cfg.Token == "" {
		return Config{}, errors.New("GITHUB_TOKEN is required")
	}
	if modeSet {
		cfg.Mode = strings.ToLower(mode)
		if cfg.Mode != "log" && cfg.Mode != "read" && cfg.Mode != "done" {
			return Config{}, errors.New("MODE must be one of log, read, done")
		}
	}
	if intervalSet {
		parsed, err := time.ParseDuration(interval)
		if err != nil || parsed <= 0 {
			return Config{}, errors.New("POLL_INTERVAL must be a positive Go duration")
		}
		cfg.PollInterval = parsed
	}
	return cfg, nil
}

func ownerSuffix(owner string) string {
	return strings.ToUpper(strings.ReplaceAll(owner, "-", "_"))
}

// TokenForOwner returns an owner's token or the main token as a fallback.
// The variable name is returned even if no owner token is configured.
func (c Config) TokenForOwner(owner string) (token, variableName string, configured bool) {
	suffix := ownerSuffix(owner)
	variableName = ownerTokenPrefix + suffix
	if token = c.OwnerTokens[suffix]; token != "" {
		return token, variableName, true
	}
	return c.Token, variableName, false
}

// OwnerNames returns configured owner variable suffixes in sorted uppercase form.
func (c Config) OwnerNames() string {
	names := make([]string, 0, len(c.OwnerTokens))
	for name, token := range c.OwnerTokens {
		if token != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
