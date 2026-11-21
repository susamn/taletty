package audible

import (
	_ "embed"
	"encoding/json"
	"strconv"
)

//go:embed internal/bridge/adapter/chapters.py
var chaptersSource string

//go:embed internal/bridge/adapter/licensing.py
var licensingSource string

//go:embed internal/bridge/adapter/media_store.py
var mediaStoreSource string

//go:embed internal/bridge/adapter/remux.py
var remuxSource string

//go:embed internal/bridge/adapter/composition.py
var compositionSourceCode string

//go:embed internal/bridge/adapter/reconciliation.py
var reconciliationSource string

// downloadScript builds only engine-trusted entrypoints. The public lifecycle in
// #112 must validate the typed outcome/clean exit. On failed/canceled child exit,
// join RunRoot BEFORE launching reconcile, retaining the same borrowed root/gate
// with a fresh bounded context, never recursively entering WithRoot. A reconcile
// result cannot prove this operation's commit, durability or auth persistence.
func downloadScript(reconcile bool) string {
	sources, _ := json.Marshal(map[string]string{
		"doctor": doctorSource, "auth_store": authStoreSource, "login": browserLoginSource,
		"sdk_session": sdkSessionSource, "library": libraryPageSource, "chapters": chaptersSource,
		"licensing": licensingSource, "media_store": mediaStoreSource, "remux": remuxSource,
		"composition": compositionSourceCode, "reconciliation": reconciliationSource,
	})
	action := "download"
	if reconcile {
		action = "reconcile"
	}
	return "import json\nSOURCES=json.loads(" + strconv.Quote(string(sources)) + ")\n" + interactiveSource + "\n" + noninteractiveSource + "\nmain(SOURCES,load_sources,null_stdio,action=" + strconv.Quote(action) + ")\n"
}
