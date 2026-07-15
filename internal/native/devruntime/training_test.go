package devruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunEnvironmentName(t *testing.T) {
	for _, valid := range []string{"LEARNING_RATE", "EPOCHS", "X1"} {
		if !runEnvironmentName(valid) {
			t.Fatalf("valid environment name %q was rejected", valid)
		}
	}
	for _, invalid := range []string{"", "lower", "1FIRST", "HAS-DASH", "HAS.DOT"} {
		if runEnvironmentName(invalid) {
			t.Fatalf("invalid environment name %q was accepted", invalid)
		}
	}
}

func TestFindPython312ChecksInterpreterVersion(t *testing.T) {
	directory := t.TempDir()
	writeInterpreter := func(name, version string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := writeInterpreter("python-valid", "3.12")
	if resolved, err := FindPython312(valid); err != nil || resolved != valid {
		t.Fatalf("FindPython312 valid = %q, %v", resolved, err)
	}
	wrong := writeInterpreter("python-wrong", "3.11")
	if _, err := FindPython312(wrong); err == nil || !strings.Contains(err.Error(), "requires Python 3.12") {
		t.Fatalf("FindPython312 wrong-version error = %v", err)
	}
}

func TestMLXPlatformVersionRequiresMacOS26(t *testing.T) {
	for _, test := range []struct {
		version string
		valid   bool
	}{
		{version: "26.0", valid: true},
		{version: "27.1.2", valid: true},
		{version: "15.7", valid: false},
		{version: "invalid", valid: false},
	} {
		actual := mlxPlatformVersionSupported(test.version)
		if actual != test.valid {
			t.Fatalf("version %q support = %v, want %v", test.version, actual, test.valid)
		}
	}
}
