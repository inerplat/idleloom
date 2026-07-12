//go:build !darwin

package serviceinstall

import "fmt"

func runningCodeHash() ([]byte, error) {
	return nil, fmt.Errorf("running code identity is supported only on macOS")
}
