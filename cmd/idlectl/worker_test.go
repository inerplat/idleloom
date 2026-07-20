package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestExplicitFlagsTracksOnlyCommandLineValues(t *testing.T) {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.String("name", "default-node", "")
	flags.Int("cpus", 4, "")
	flags.String("memory", "8g", "")
	if err := flags.Parse([]string{"--name", "mac-worker", "--cpus", "4"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	explicit := explicitFlags(flags)
	if !explicit["name"] || !explicit["cpus"] {
		t.Fatalf("flags passed on the command line must be explicit, got %v", explicit)
	}
	if explicit["memory"] {
		t.Fatalf("memory was not passed and must not be explicit, got %v", explicit)
	}
}

func TestParseSizeMiB(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{input: "8g", want: 8192},
		{input: "8gb", want: 8192},
		{input: "8gib", want: 8192},
		{input: "8192m", want: 8192},
		{input: "8192mb", want: 8192},
		{input: "8192mib", want: 8192},
		{input: "512", want: 512},
		{input: " 8G ", want: 8192},
		{input: "8 g", want: 8192},
		{input: "", wantErr: true},
		{input: "eight", wantErr: true},
		{input: "8t", wantErr: true},
		{input: "0g", wantErr: true},
		{input: "-1g", wantErr: true},
		{input: "-8192m", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseSizeMiB(test.input)
		if test.wantErr {
			if err == nil {
				t.Fatalf("parseSizeMiB(%q) = %d, expected an error", test.input, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseSizeMiB(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("parseSizeMiB(%q) = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestDefaultNamesProducesSanitizedNodeName(t *testing.T) {
	name := defaultNames()
	if !strings.HasSuffix(name, "-idle") {
		t.Fatalf("default node name %q does not end with -idle", name)
	}
	if strings.HasPrefix(name, "-") {
		t.Fatalf("default node name %q starts with a dash", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		t.Fatalf("default node name %q contains unsupported rune %q", name, r)
	}
}
