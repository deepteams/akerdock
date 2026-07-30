package handlers

import (
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

func TestValidateGitSource(t *testing.T) {
	tests := []struct {
		name      string
		repo      string
		branch    string
		deployKey bool
		wantValid bool
	}{
		{name: "HTTPS repository", repo: "https://github.com/acme/app.git", branch: "main", wantValid: true},
		{name: "HTTP repository for local forge", repo: "http://gitea.local/acme/app.git", branch: "release/v1", wantValid: true},
		{name: "git protocol", repo: "git://git.example.test/acme/app.git", branch: "main", wantValid: true},
		{name: "scp SSH with key", repo: "git@git.example.test:acme/app.git", branch: "main", deployKey: true, wantValid: true},
		{name: "URL SSH with key", repo: "ssh://git@git.example.test:2222/acme/app.git", branch: "main", deployKey: true, wantValid: true},
		{name: "SSH requires key", repo: "git@git.example.test:acme/app.git", branch: "main"},
		{name: "key forbidden on HTTPS", repo: "https://github.com/acme/app.git", branch: "main", deployKey: true},
		{name: "credentials forbidden", repo: "https://user:token@example.test/acme/app.git", branch: "main"},
		{name: "unsupported scheme", repo: "file:///tmp/app.git", branch: "main"},
		{name: "missing host", repo: "https:///acme/app.git", branch: "main"},
		{name: "SSH option injection", repo: "git@host:-oProxyCommand=id", branch: "main", deployKey: true},
		{name: "branch shell injection", repo: "https://example.test/acme/app.git", branch: "main;touch /tmp/pwn"},
		{name: "branch traversal", repo: "https://example.test/acme/app.git", branch: "../main"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateGitSource(tt.repo, tt.branch, tt.deployKey)
			if tt.wantValid && len(got) != 0 {
				t.Fatalf("validateGitSource() returned errors: %#v", got)
			}
			if !tt.wantValid && len(got) == 0 {
				t.Fatal("validateGitSource() accepted an invalid source")
			}
		})
	}
}

func TestGitProviderOf(t *testing.T) {
	tests := map[string]store.GitProvider{
		"https://github.com/acme/app.git":         store.GitProviderGithub,
		"git@gitlab.example.test:acme/app.git":    store.GitProviderGitlab,
		"ssh://git@bitbucket.org/acme/app.git":    store.GitProviderBitbucket,
		"https://gitea.example.test/acme/app.git": store.GitProviderGitea,
		"https://forge.example.test/acme/app.git": store.GitProviderOther,
	}
	for repo, want := range tests {
		t.Run(repo, func(t *testing.T) {
			if got := gitProviderOf(repo); got != want {
				t.Fatalf("gitProviderOf(%q) = %q, want %q", repo, got, want)
			}
		})
	}
}

func TestValidateUptimeTarget(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		target string
		valid  bool
	}{
		{name: "HTTP", kind: "http", target: "http://app.example.test/health", valid: true},
		{name: "HTTPS", kind: "http", target: "https://app.example.test/health", valid: true},
		{name: "HTTP relative", kind: "http", target: "/health"},
		{name: "HTTP wrong scheme", kind: "http", target: "ftp://app.example.test/health"},
		{name: "HTTP credentials", kind: "http", target: "http://user:secret@app.example.test/health"},
		{name: "HTTP loopback", kind: "http", target: "http://127.0.0.1:8080/health"},
		{name: "HTTP metadata", kind: "http", target: "http://169.254.169.254/latest/meta-data/"},
		{name: "HTTP private IPv6", kind: "http", target: "http://[fd00::1]/health"},
		{name: "TCP", kind: "tcp", target: "db.example.test:5432", valid: true},
		{name: "TCP public IPv6", kind: "tcp", target: "[2606:4700:4700::1111]:443", valid: true},
		{name: "TCP loopback", kind: "tcp", target: "127.0.0.1:5432"},
		{name: "TCP private", kind: "tcp", target: "10.0.0.1:5432"},
		{name: "TCP scoped link-local IPv6", kind: "tcp", target: "[fe80::1%25en0]:80"},
		{name: "TCP unbracketed IPv6", kind: "tcp", target: "2606:4700:4700::1111:443"},
		{name: "TCP missing port", kind: "tcp", target: "db.example.test"},
		{name: "TCP zero port", kind: "tcp", target: "db.example.test:0"},
		{name: "TCP high port", kind: "tcp", target: "db.example.test:65536"},
		{name: "unknown kind", kind: "icmp", target: "app.example.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateUptimeTarget(tt.kind, tt.target)
			if (got == nil) != tt.valid {
				t.Fatalf("validateUptimeTarget(%q, %q) error = %#v, valid = %v",
					tt.kind, tt.target, got, tt.valid)
			}
		})
	}
}

func TestValidateS3Endpoint(t *testing.T) {
	tests := map[string]bool{
		"https://s3.example.test":               true,
		"http://minio.local:9000":               true,
		"http://minio.local:9000/prefix":        true,
		"s3://bucket":                           false,
		"/relative":                             false,
		"https:///missing-host":                 false,
		"https://access:secret@s3.example.test": false,
	}
	for endpoint, want := range tests {
		t.Run(endpoint, func(t *testing.T) {
			if got := validateS3Endpoint(endpoint); got != want {
				t.Fatalf("validateS3Endpoint(%q) = %v, want %v", endpoint, got, want)
			}
		})
	}
}

func TestNormalizeCron(t *testing.T) {
	tests := []struct {
		input string
		want  string
		valid bool
	}{
		{input: "hourly", want: "0 * * * *", valid: true},
		{input: "  daily  ", want: "0 3 * * *", valid: true},
		{input: "*/15 * * * *", want: "*/15 * * * *", valid: true},
		{input: "61 * * * *"},
		{input: "* * * *"},
		{input: "* * * * *; id"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, valid := normalizeCron(tt.input)
			if got != tt.want || valid != tt.valid {
				t.Fatalf("normalizeCron(%q) = (%q, %v), want (%q, %v)",
					tt.input, got, valid, tt.want, tt.valid)
			}
		})
	}
}

func TestSmallValidationHelpers(t *testing.T) {
	positive := 14
	zero := 0
	if got := drillInterval(&positive, 7); got != 14 {
		t.Fatalf("drillInterval(14, 7) = %d", got)
	}
	if got := drillInterval(&zero, 7); got != 7 {
		t.Fatalf("drillInterval(0, 7) = %d", got)
	}
	if !validTimezone("Europe/Paris") || validTimezone("Mars/Olympus") {
		t.Fatal("validTimezone did not enforce IANA timezone names")
	}
	if got, ok := parseHHMM(" 23:45 "); !ok || !got.Valid {
		t.Fatal("parseHHMM rejected a valid time")
	}
	if _, ok := parseHHMM("24:00"); ok {
		t.Fatal("parseHHMM accepted an invalid time")
	}
}
