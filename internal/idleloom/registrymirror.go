package idleloom

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// registryHostPattern matches a bare registry host with an optional port. It
// deliberately excludes schemes, paths, whitespace, and quotes so a host can
// never become a tar-traversal path or corrupt the rendered hosts.toml.
var registryHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?$`)

// RegistryMirror redirects image pulls for Host to a mirror URL via a
// containerd certs.d hosts.toml entry.
type RegistryMirror struct {
	Host string `json:"host"`
	URL  string `json:"url"`
}

// parseRegistryMirrors parses repeatable HOST=URL mirror specifications. It
// rejects malformed hosts or URLs and duplicate hosts. Mirrors that target a
// plain-http URL are accepted but reported as warnings so the caller can echo
// them to the operator.
func parseRegistryMirrors(specs []string) ([]RegistryMirror, []string, error) {
	if len(specs) == 0 {
		return nil, nil, nil
	}
	mirrors := make([]RegistryMirror, 0, len(specs))
	var warnings []string
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		equals := strings.Index(spec, "=")
		if equals <= 0 {
			return nil, nil, fmt.Errorf("registry mirror %q must use HOST=URL syntax", spec)
		}
		host := spec[:equals]
		rawURL := spec[equals+1:]
		if strings.Contains(host, "..") || !registryHostPattern.MatchString(host) {
			return nil, nil, fmt.Errorf("registry mirror host %q must be a bare registry host such as registry.example.com or registry.example.com:5000", host)
		}
		if rawURL == "" {
			return nil, nil, fmt.Errorf("registry mirror %q has an empty URL", spec)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return nil, nil, fmt.Errorf("registry mirror URL %q is invalid: %w", rawURL, err)
		}
		switch parsed.Scheme {
		case "https":
		case "http":
			warnings = append(warnings, fmt.Sprintf("registry mirror %q uses plain http; image pulls will not be encrypted", rawURL))
		default:
			return nil, nil, fmt.Errorf("registry mirror URL %q must use https", rawURL)
		}
		if parsed.Host == "" {
			return nil, nil, fmt.Errorf("registry mirror URL %q must include a host", rawURL)
		}
		if seen[host] {
			return nil, nil, fmt.Errorf("duplicate registry mirror host %q", host)
		}
		seen[host] = true
		mirrors = append(mirrors, RegistryMirror{Host: host, URL: rawURL})
	}
	return mirrors, warnings, nil
}

// renderHostsTOML produces the containerd certs.d hosts.toml content that
// redirects pulls for the mirror host to its mirror URL.
func renderHostsTOML(mirror RegistryMirror) string {
	return fmt.Sprintf(`server = "https://%s"

[host."%s"]
  capabilities = ["pull", "resolve"]
`, mirror.Host, mirror.URL)
}
