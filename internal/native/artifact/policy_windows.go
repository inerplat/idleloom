//go:build windows

package artifact

import "fmt"

// VerifyExtractedTree is not implemented on Windows. Artifact staging runs on
// the Native Metal host, which is macOS only; the verification relies on
// openat/O_NOFOLLOW semantics that Windows does not provide. idlectl still
// builds for Windows so the Linux Worker commands are available there.
func (p Policy) VerifyExtractedTree(_ string, manifest Manifest) error {
	if err := p.ValidateDeclaration(manifest); err != nil {
		return err
	}
	return fmt.Errorf("artifact tree verification is not supported on Windows; Native Metal artifacts are staged on macOS hosts")
}
