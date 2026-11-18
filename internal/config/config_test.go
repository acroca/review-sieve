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
