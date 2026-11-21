package store

import (
	"fmt"
	"os"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestMain(m *testing.M) {
	// Share a private compilation cache with owned process fixtures so their
	// deadlines measure contention, not another JIT build. Constrained hosts
	// can prewarm an explicitly supplied cache with the non-race suite. SQLite
	// initialization remains inside Go's test deadline; nothing is skipped.
	path := os.Getenv("TALETTY_SQLITE_TEST_CACHE")
	owned := path == ""
	if owned {
		var err error
		path, err = os.MkdirTemp("", "taletty-sqlite-compiler-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Setenv("TALETTY_SQLITE_TEST_CACHE", path)
	}
	code := runStoreTests(m, path)
	if owned {
		os.RemoveAll(path)
	}
	os.Exit(code)
}

func runStoreTests(m *testing.M, path string) int {
	cache, err := wazero.NewCompilationCacheWithDir(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Same limits/features as upstream's default runtime, not an alternate
	// interpreter or fake SQLite. No production runtime configuration changed.
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).
		WithCoreFeatures(api.CoreFeaturesV2).WithCompilationCache(cache)
	return m.Run()
}
