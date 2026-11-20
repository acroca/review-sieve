// Package config parses the application's environment configuration.
package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ownerTokenPrefix = "GITHUB_TOKEN_"
	modeRulePrefix   = "MODE_"
)

// ruleModes lists the modes that have a rule variable, in the order the
// variables are read. A target found in two lists is reported against the
// later one.
var ruleModes = []string{"done", "read", "log"}

// Config contains the configuration needed to poll GitHub notifications.
type Config struct {
	Token        string
	Mode         string
	PollInterval time.Duration
	OwnerTokens  map[string]string
	// Rules maps a lowercase owner or owner/repo to the mode used for it
	// instead of Mode.
	Rules map[string]string
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
	lists := make(map[string]string)
	var unsupported []string
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
		case "MODE_DONE", "MODE_READ", "MODE_LOG":
			lists[key] = value
		default:
			if strings.HasPrefix(key, modeRulePrefix) {
				// A misspelled rule variable would otherwise do nothing.
				unsupported = append(unsupported, key)
				continue
			}
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
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return Config{}, fmt.Errorf("%s is not a supported variable; use MODE_DONE, MODE_READ or MODE_LOG", unsupported[0])
	}
	rules, err := parseRules(lists)
	if err != nil {
		return Config{}, err
	}
	cfg.Rules = rules
	return cfg, nil
}

// parseRules reads the comma-separated MODE_<MODE> lists. Errors name an
// entry by its position, never by its value.
func parseRules(lists map[string]string) (map[string]string, error) {
	rules := make(map[string]string)
	for _, mode := range ruleModes {
		variable := ruleVariable(mode)
		for i, entry := range strings.Split(lists[variable], ",") {
			target := strings.ToLower(strings.TrimSpace(entry))
			if target == "" {
				continue
			}
			if !validTarget(target) {
				return nil, fmt.Errorf("%s entry %d must be an owner or owner/repo", variable, i+1)
			}
			if other, listed := rules[target]; listed && other != mode {
				return nil, fmt.Errorf("%s entry %d is also listed in %s", variable, i+1, ruleVariable(other))
			}
			rules[target] = mode
		}
	}
	return rules, nil
}

func ruleVariable(mode string) string {
	return modeRulePrefix + strings.ToUpper(mode)
}

// validTarget reports whether target is an owner or an owner/repo. Anything
// GitHub never uses in a name is rejected, so a wildcard or a quote kept by
// an env file fails at startup instead of silently never matching.
func validTarget(target string) bool {
	owner, repo, isRepo := strings.Cut(target, "/")
	return validName(owner) && (!isRepo || validName(repo))
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// ModeFor returns the mode for a repository given as owner/repo: its own
// rule, else its owner's rule, else Mode.
func (c Config) ModeFor(repo string) string {
	repo = strings.ToLower(repo)
	if mode, ok := c.Rules[repo]; ok {
		return mode
	}
	owner, _, _ := strings.Cut(repo, "/")
	if mode, ok := c.Rules[owner]; ok {
		return mode
	}
	return c.Mode
}

// RuleTargets returns the owners and repositories with a rule for mode, in
// sorted lowercase form.
func (c Config) RuleTargets(mode string) string {
	var targets []string
	for target, ruleMode := range c.Rules {
		if ruleMode == mode {
			targets = append(targets, target)
		}
	}
	sort.Strings(targets)
	return strings.Join(targets, ",")
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
