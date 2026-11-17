// Package profile resolves Taletty-owned paths without accessing account data.
package profile

import (
	"errors"
	"path/filepath"
	"strings"
)

// DefaultRoot resolves the agreed private data root from an absolute home path.
// It does not create directories, follow legacy locations or honor XDG overrides.
// The caller obtains the home directory; injecting it keeps resolution read-only
// and testable without touching the user's profile.
func DefaultRoot(home string) (string, error) {
	if !filepath.IsAbs(home) || strings.ContainsRune(home, '\x00') {
		return "", errors.New("an absolute home directory is required")
	}
	return filepath.Join(home, ".config", "taletty"), nil
}
