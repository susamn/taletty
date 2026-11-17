package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/susamn/taletty/app/internal/profile"
)

func TestDefaultRootIsReadOnlyAndIndependentOfOtherApps(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, ".config", "audible-tui")
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(old, "auth.json")
	if err := os.WriteFile(marker, []byte("not-a-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "elsewhere"))
	for _, input := range []string{home, home + string(filepath.Separator)} {
		root, err := profile.DefaultRoot(input)
		if err != nil || root != filepath.Join(home, ".config", "taletty") {
			t.Fatalf("unexpected root %q: %v", root, err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("resolving paths must not initialize a profile")
		}
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "not-a-credential" {
		t.Fatal("old app data was modified")
	}
	withSpaces := filepath.Join(home, "home with spaces")
	root, err := profile.DefaultRoot(withSpaces)
	if err != nil || root != filepath.Join(withSpaces, ".config", "taletty") {
		t.Fatalf("home with spaces: %q, %v", root, err)
	}
}

func TestDefaultRootRejectsInvalidHome(t *testing.T) {
	for _, home := range []string{"", "relative", "~/home", "\x00", "/home/private\x00"} {
		root, err := profile.DefaultRoot(home)
		if err == nil || root != "" {
			t.Fatalf("invalid home accepted: %q", home)
		}
		if err.Error() != "an absolute home directory is required" {
			t.Fatal("path errors must not echo private inputs")
		}
	}
}
