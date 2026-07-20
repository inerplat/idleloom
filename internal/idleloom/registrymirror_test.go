package idleloom

import (
	"strings"
	"testing"
)

func TestParseRegistryMirrors(t *testing.T) {
	mirrors, warnings, err := parseRegistryMirrors([]string{
		"nks.kr.private-ncr.ntruss.com=https://nks.kr.ncr.ntruss.com",
		"docker.io=https://mirror.example.com",
	})
	if err != nil {
		t.Fatalf("parseRegistryMirrors: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(mirrors) != 2 {
		t.Fatalf("got %d mirrors, want 2", len(mirrors))
	}
	if mirrors[0].Host != "nks.kr.private-ncr.ntruss.com" || mirrors[0].URL != "https://nks.kr.ncr.ntruss.com" {
		t.Fatalf("unexpected first mirror: %+v", mirrors[0])
	}
}

func TestParseRegistryMirrorsHTTPWarns(t *testing.T) {
	mirrors, warnings, err := parseRegistryMirrors([]string{"reg.example.com=http://mirror.example.com"})
	if err != nil {
		t.Fatalf("parseRegistryMirrors: %v", err)
	}
	if len(mirrors) != 1 {
		t.Fatalf("got %d mirrors, want 1", len(mirrors))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "plain http") {
		t.Fatalf("expected a plain-http warning, got %v", warnings)
	}
}

func TestParseRegistryMirrorsErrors(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want string
	}{
		{"missing equals", "reg.example.com", "must use HOST=URL syntax"},
		{"empty host", "=https://mirror.example.com", "must use HOST=URL syntax"},
		{"host with scheme", "https://reg.example.com=https://mirror.example.com", "bare registry host"},
		{"host with path", "reg.example.com/v2=https://mirror.example.com", "bare registry host"},
		{"host traversal", "..=https://mirror.example.com", "bare registry host"},
		{"host dotdot label", "a/../b=https://mirror.example.com", "bare registry host"},
		{"host with space", "reg example.com=https://mirror.example.com", "bare registry host"},
		{"host with quote", "reg\".example.com=https://mirror.example.com", "bare registry host"},
		{"host with shell metachar", "reg;rm.example.com=https://mirror.example.com", "bare registry host"},
		{"empty url", "reg.example.com=", "has an empty URL"},
		{"bad scheme", "reg.example.com=ftp://mirror.example.com", "must use https"},
		{"no url host", "reg.example.com=https://", "must include a host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseRegistryMirrors([]string{tc.spec}); err == nil {
				t.Fatalf("expected an error for %q", tc.spec)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseRegistryMirrorsRejectsDuplicateHost(t *testing.T) {
	_, _, err := parseRegistryMirrors([]string{
		"reg.example.com=https://a.example.com",
		"reg.example.com=https://b.example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate registry mirror host") {
		t.Fatalf("expected a duplicate-host error, got %v", err)
	}
}

func TestRenderHostsTOML(t *testing.T) {
	got := renderHostsTOML(RegistryMirror{Host: "reg.example.com", URL: "https://mirror.example.com"})
	want := `server = "https://reg.example.com"

[host."https://mirror.example.com"]
  capabilities = ["pull", "resolve"]
`
	if got != want {
		t.Fatalf("hosts.toml =\n%q\nwant\n%q", got, want)
	}
}
