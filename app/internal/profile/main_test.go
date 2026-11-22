package profile

import (
	"fmt"
	"os"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestMain(m *testing.M) {
	// Share a private compilation cache with owned child fixtures so subprocess
	// lock/reopen tests measure contention rather than recompiling SQLite Wasm.
	path := os.Getenv("TALETTY_SQLITE_TEST_CACHE")
	owned := path == ""
	if owned {
		var err error
		path, err = os.MkdirTemp("", "taletty-profile-sqlite-compiler-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Setenv("TALETTY_SQLITE_TEST_CACHE", path)
	}
	code := runProfileTests(m, path)
	if owned {
		os.RemoveAll(path)
	}
	os.Exit(code)
}

func runProfileTests(m *testing.M, path string) int {
	cache, err := wazero.NewCompilationCacheWithDir(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).
		WithCoreFeatures(api.CoreFeaturesV2).WithCompilationCache(cache)
	return m.Run()
}
