package idleloom

import "testing"

func TestNormalizeKubernetesVersion(t *testing.T) {
	for input, expected := range map[string]string{
		"v1.35.4":             "v1.35.4",
		"v1.31.5-gke.1234000": "v1.31.5",
		"v1.30.13-eks-abcdef": "v1.30.13",
		"v1.29.8+rke2r1":      "v1.29.8",
	} {
		got, err := normalizeKubernetesVersion(input)
		if err != nil {
			t.Errorf("normalizeKubernetesVersion(%q): %v", input, err)
			continue
		}
		if got != expected {
			t.Errorf("normalizeKubernetesVersion(%q) = %q, want %q", input, got, expected)
		}
	}
	if _, err := normalizeKubernetesVersion("development"); err == nil {
		t.Fatal("expected an invalid GitVersion to be rejected")
	}
}
