package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]string{"GITHUB_TOKEN=main-token"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "main-token" || cfg.Mode != "log" || cfg.PollInterval != 60*time.Second {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if names := cfg.OwnerNames(); names != "" {
		t.Fatalf("owner names = %q, want empty", names)
	}
}

func TestParseModeAndInterval(t *testing.T) {
	for _, mode := range []string{"log", "READ", "DoNe"} {
		t.Run(mode, func(t *testing.T) {
			cfg, err := Parse([]string{"GITHUB_TOKEN=main", "MODE=" + mode, "POLL_INTERVAL=2m30s"})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != strings.ToLower(mode) || cfg.PollInterval != 150*time.Second {
				t.Fatalf("mode = %q, interval = %s", cfg.Mode, cfg.PollInterval)
			}
		})
	}
}

func TestOwnerTokens(t *testing.T) {
	cfg, err := Parse([]string{
		"GITHUB_TOKEN=main",
		"GITHUB_TOKEN_ZETA=last",
		"GITHUB_TOKEN_ACME_CORP=acme",
		"GITHUB_TOKEN_EMPTY=",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"acme-corp", "Acme-Corp", "ACME_CORP"} {
		token, variable, configured := cfg.TokenForOwner(owner)
		if token != "acme" || variable != "GITHUB_TOKEN_ACME_CORP" || !configured {
			t.Errorf("%s: got %q, %q, %v", owner, token, variable, configured)
		}
	}
	for _, owner := range []string{"empty", "unknown-owner"} {
		token, variable, configured := cfg.TokenForOwner(owner)
		wantVariable := "GITHUB_TOKEN_" + strings.ToUpper(strings.ReplaceAll(owner, "-", "_"))
		if token != "main" || variable != wantVariable || configured {
			t.Errorf("%s: got %q, %q, %v", owner, token, variable, configured)
		}
	}
	// OwnerNames reports the canonical uppercase environment-variable suffixes.
	if names := cfg.OwnerNames(); names != "ACME_CORP,ZETA" {
		t.Fatalf("owner names = %q, want ACME_CORP,ZETA", names)
	}
}

func TestInvalidConfigurationDoesNotExposeTokens(t *testing.T) {
	const secret = "sensitive-token"
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"missing token", nil, "GITHUB_TOKEN is required"},
		{"empty token", []string{"GITHUB_TOKEN="}, "GITHUB_TOKEN is required"},
		{"invalid mode", []string{"GITHUB_TOKEN=" + secret, "MODE=" + secret}, "MODE must be one of log, read, done"},
		{"empty mode", []string{"GITHUB_TOKEN=" + secret, "MODE="}, "MODE must be one of log, read, done"},
		{"invalid interval", []string{"GITHUB_TOKEN=" + secret, "POLL_INTERVAL=" + secret}, "POLL_INTERVAL"},
		{"empty interval", []string{"GITHUB_TOKEN=" + secret, "POLL_INTERVAL="}, "POLL_INTERVAL"},
		{"zero interval", []string{"GITHUB_TOKEN=" + secret, "POLL_INTERVAL=0s"}, "POLL_INTERVAL"},
		{"negative interval", []string{"GITHUB_TOKEN=" + secret, "POLL_INTERVAL=-1s"}, "POLL_INTERVAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error exposes token: %v", err)
			}
		})
	}
}

func TestDuplicateEnvironmentValuesUseLast(t *testing.T) {
	cfg, err := Parse([]string{
		"GITHUB_TOKEN=old", "GITHUB_TOKEN=new",
		"GITHUB_TOKEN_ORG=old", "GITHUB_TOKEN_ORG=",
		"MODE=read", "MODE=done",
		"POLL_INTERVAL=1s", "POLL_INTERVAL=2s",
		"IGNORED", "UNRELATED=value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "new" || cfg.Mode != "done" || cfg.PollInterval != 2*time.Second || cfg.OwnerNames() != "" {
		t.Fatalf("unexpected configuration: %#v", cfg)
	}
}

func TestModeRules(t *testing.T) {
	cfg, err := Parse([]string{
		"GITHUB_TOKEN=main",
		"MODE=read",
		"MODE_DONE=Acme-Corp, other-org/Noisy.Repo ,acme-corp,",
		"MODE_LOG=acme-corp/critical_service",
		"MODE_READ=",
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ repo, want string }{
		{"acme-corp/api", "done"},
		{"ACME-CORP/API", "done"},
		// A repository entry beats its owner's entry.
		{"acme-corp/critical_service", "log"},
		{"Acme-Corp/Critical_Service", "log"},
		{"other-org/noisy.repo", "done"},
		// A repository entry says nothing about the rest of its owner.
		{"other-org/quiet", "read"},
		{"unknown/repo", "read"},
		// Unlike token variable names, hyphens and underscores are distinct.
		{"acme_corp/api", "read"},
		{"acme-corp-eu/api", "read"},
	}
	for _, tt := range tests {
		if got := cfg.ModeFor(tt.repo); got != tt.want {
			t.Errorf("ModeFor(%q) = %q, want %q", tt.repo, got, tt.want)
		}
	}
	for mode, want := range map[string]string{
		"done": "acme-corp,other-org/noisy.repo",
		"log":  "acme-corp/critical_service",
		"read": "",
	} {
		if got := cfg.RuleTargets(mode); got != want {
			t.Errorf("RuleTargets(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestModeWithoutRules(t *testing.T) {
	cfg, err := Parse([]string{"GITHUB_TOKEN=main", "MODE=done"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ModeFor("acme/repo"); got != "done" {
		t.Fatalf("ModeFor = %q, want done", got)
	}
}

func TestDuplicateModeRuleVariablesUseLast(t *testing.T) {
	cfg, err := Parse([]string{"GITHUB_TOKEN=main", "MODE_DONE=acme", "MODE_DONE=other"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ModeFor("acme/repo"); got != "log" {
		t.Errorf("ModeFor(acme/repo) = %q, want log", got)
	}
	if got := cfg.ModeFor("other/repo"); got != "done" {
		t.Errorf("ModeFor(other/repo) = %q, want done", got)
	}
}

func TestInvalidModeRulesDoNotExposeValues(t *testing.T) {
	const secret = "sensitive-token"
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"misspelled variable", []string{"MODE_DOEN=acme"}, "MODE_DOEN is not a supported variable"},
		{"bare prefix", []string{"MODE_=acme"}, "MODE_ is not a supported variable"},
		{"wildcard", []string{"MODE_DONE=acme,acme/*"}, "MODE_DONE entry 2 must be an owner or owner/repo"},
		{"literal quotes", []string{`MODE_READ="acme"`}, "MODE_READ entry 1 must be an owner or owner/repo"},
		{"two slashes", []string{"MODE_LOG=acme/repo/extra"}, "MODE_LOG entry 1 must be an owner or owner/repo"},
		{"empty owner", []string{"MODE_DONE=/repo"}, "MODE_DONE entry 1 must be an owner or owner/repo"},
		{"empty repository", []string{"MODE_DONE=acme/"}, "MODE_DONE entry 1 must be an owner or owner/repo"},
		{"space inside", []string{"MODE_DONE=ok,,acme corp"}, "MODE_DONE entry 3 must be an owner or owner/repo"},
		{"secret in entry", []string{"MODE_DONE=" + secret + "/*"}, "MODE_DONE entry 1 must be an owner or owner/repo"},
		{"owner in two lists", []string{"MODE_DONE=acme", "MODE_READ=other,ACME"}, "MODE_READ entry 2 is also listed in MODE_DONE"},
		{"repository in two lists", []string{"MODE_LOG=acme/Repo", "MODE_READ=acme/repo"}, "MODE_LOG entry 1 is also listed in MODE_READ"},
		{"secret in two lists", []string{"MODE_DONE=" + secret, "MODE_LOG=" + secret}, "MODE_LOG entry 1 is also listed in MODE_DONE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(append([]string{"GITHUB_TOKEN=" + secret}, tt.env...))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error exposes a value: %v", err)
			}
		})
	}
}
