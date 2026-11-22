package ui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func libraryFixture() ui.SnapshotMsg {
	acq1 := int64(1000)
	acq2 := int64(2000)
	return ui.SnapshotMsg{
		Books: []ui.Book{
			{
				Key:             "k1",
				Title:           "Zebra Tales",
				Subtitle:        "Desert Chronicles",
				Author:          "Alice Writer",
				Narrator:        "Bob Voice",
				Duration:        90 * time.Minute,
				HasDuration:     true,
				RecentAtMS:      500,
				InProgress:      true,
				AcquisitionAtMS: &acq1,
				CatalogOrder:    0,
				Availability:    ui.Ready,
			},
			{
				Key:          "k2",
				Title:        "Alpha Dawn",
				Subtitle:     "Starlight",
				Author:       "Unknown author",
				Narrator:     "Clara Speaker",
				HasDuration:  false,
				Finished:     true,
				CatalogOrder: 1,
				Availability: ui.NotDownloaded,
			},
			{
				Key:             "k3",
				Title:           "Middle Earth",
				Subtitle:        "Journey",
				Author:          "Aaron Author",
				Narrator:        "Dana Reader",
				Duration:        30 * time.Minute,
				HasDuration:     true,
				RecentAtMS:      900,
				InProgress:      true,
				AcquisitionAtMS: &acq2,
				CatalogOrder:    2,
				Availability:    ui.Ready,
			},
		},
		Player: ui.Player{
			Key: "k1", Session: "s1", Title: "Zebra Tales", State: ui.Playing,
			Position: 5 * time.Minute, Duration: 90 * time.Minute, Speed: 1.0, Volume: 80, VolumeKnown: true,
		},
	}
}

func TestJ03LibraryViewsDecorativeSkipAndSelectionPreservation(t *testing.T) {
	var actions []ui.Action
	r := start(t, libraryFixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Default SortTitle selects Alpha Dawn (k2) first; move to Zebra Tales (k1).
	send(t, r, key("G"))
	if r.Model.(ui.Model).Selection() != "k1" {
		t.Fatalf("expected k1 selected under SortTitle, got %q", r.Model.(ui.Model).Selection())
	}

	// Focus Library sidebar (1); decorative heading row must be skipped on navigation.
	send(t, r, key("1"))
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "LIBRARY VIEWS") || !strings.Contains(view, "Continue (2)") || !strings.Contains(view, "Downloaded (2)") || !strings.Contains(view, "Finished (1)") {
		t.Fatalf("missing sidebar views/counts:\n%s", view)
	}
	send(t, r, key("k"), key("up"), key("g"), key("home")) // stays on first selectable row (All), never heading
	send(t, r, key("j"), key("enter"))                     // Continue Listening -> preserves k1!
	model := r.Model.(ui.Model)
	v, _, _ := model.ViewState()
	if v != ui.ViewContinue || model.Selection() != "k1" || model.Focus() != ui.Books {
		t.Fatalf("Continue Listening switch failed: view=%v sel=%q focus=%v", v, model.Selection(), model.Focus())
	}

	// Switch to Finished (only k2) -> selection updates cleanly to k2.
	// G lands on Bookmarks entry; k skips VIEWS / OVERLAYS heading to Finished.
	send(t, r, key("1"), key("G"), key("k"), key("enter"))
	model = r.Model.(ui.Model)
	v, _, _ = model.ViewState()
	if v != ui.ViewFinished || model.Selection() != "k2" {
		t.Fatalf("Finished switch failed: view=%v sel=%q", v, model.Selection())
	}

	// Empty view message when no books match Downloaded after removing Ready books.
	emptyDL := libraryFixture()
	for i := range emptyDL.Books {
		emptyDL.Books[i].Availability = ui.NotDownloaded
	}
	send(t, r, emptyDL, key("1"), key("k"), key("k"), key("enter")) // Downloaded (0)
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "No books in Downloaded") || r.Model.(ui.Model).Selection() != "" {
		t.Fatalf("expected empty Downloaded message:\n%s", view)
	}
	if len(actions) != 0 {
		t.Fatalf("library view navigation leaked actions: %+v", actions)
	}
}

func TestJ03ContributorFilterOwnershipAndEscSemantics(t *testing.T) {
	var actions []ui.Action
	r := start(t, libraryFixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Filter unavailable in Details and PlayerPane.
	for _, paneKey := range []string{"3", "5"} {
		send(t, r, key(paneKey), key("/"))
		if r.Model.(ui.Model).FilterEditing() || !strings.Contains(r.Model.View().Content, "Filter unavailable") {
			t.Fatalf("filter should be unavailable in pane %s", paneKey)
		}
	}

	// Open filter in Books (2), verify typing ownership (q/1-5/ctrl+c/tab stay in filter), then type "clara".
	send(t, r, key("2"), key("/"))
	if !r.Model.(ui.Model).FilterEditing() {
		t.Fatal("expected FilterEditing true")
	}
	for _, owned := range []string{"q", "ctrl+c", "tab", "1", "2", "3", "4", "5"} {
		send(t, r, key(owned))
	}
	if len(actions) != 0 || !r.Model.(ui.Model).FilterEditing() {
		t.Fatalf("filter editor leaked owned keys: actions=%+v", actions)
	}
	send(t, r, key("ctrl+u"))
	for _, ch := range []string{"c", "l", "a", "r", "a", "x"} {
		send(t, r, key(ch))
	}
	send(t, r, tea.KeyPressMsg{Code: tea.KeyBackspace})
	// Enter applies filter ONLY (never plays row!).
	send(t, r, key("enter"))
	if r.Model.(ui.Model).FilterEditing() || len(actions) != 0 || r.Model.(ui.Model).Selection() != "k2" {
		t.Fatalf("Enter in filter played row or lost match: editing=%v actions=%+v sel=%q", r.Model.(ui.Model).FilterEditing(), actions, r.Model.(ui.Model).Selection())
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, `Filter: "clara"`) {
		t.Fatalf("expected filter tag in Books title:\n%s", view)
	}

	// Open filter again, type "nomatch", then Esc restores prior filter "clara".
	send(t, r, key("/"), key("ctrl+u"), key("n"), key("o"), key("m"), key("a"), key("t"), key("c"), key("h"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "No matches for filter") {
		t.Fatalf("expected no-matches message:\n%s", view)
	}
	send(t, r, key("esc"))
	_, _, q := r.Model.(ui.Model).ViewState()
	if q != "clara" || r.Model.(ui.Model).Selection() != "k2" {
		t.Fatalf("Esc while editing did not restore prior filter: q=%q sel=%q", q, r.Model.(ui.Model).Selection())
	}

	// Esc outside editing clears applied filter while preserving selection k2.
	send(t, r, key("esc"))
	_, _, q = r.Model.(ui.Model).ViewState()
	if q != "" || r.Model.(ui.Model).Selection() != "k2" {
		t.Fatalf("Esc outside editing did not clear filter cleanly: q=%q sel=%q", q, r.Model.(ui.Model).Selection())
	}

	// Paste and space inside filter match subtitle "Desert Chronicles".
	send(t, r, key("/"), tea.PasteMsg{Content: "desert\x1b[2J"}, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}, key("c"), key("enter"))
	if r.Model.(ui.Model).Selection() != "k1" {
		t.Fatalf("expected subtitle match k1, got %q", r.Model.(ui.Model).Selection())
	}
}

func TestJ03StableSortingMissingAcquisitionAndAsyncRefresh(t *testing.T) {
	var actions []ui.Action
	r := start(t, libraryFixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Sort unavailable in PlayerPane.
	send(t, r, key("5"), key("s"))
	if r.Model.(ui.Model).SortOpen() {
		t.Fatal("sort should be unavailable in PlayerPane")
	}

	// Select k1, open Sort menu in Books, verify key ownership and Esc cancel.
	send(t, r, key("2"), key("G"), key("s"))
	if !r.Model.(ui.Model).SortOpen() {
		t.Fatal("expected SortOpen true")
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Sort Library") {
		t.Fatalf("missing Sort Library dialog:\n%s", view)
	}
	for _, blocked := range []string{" ", "q", "ctrl+c", "1", "2", "3", "4", "5", "?"} {
		send(t, r, key(blocked))
	}
	if len(actions) != 0 || !r.Model.(ui.Model).SortOpen() {
		t.Fatalf("Sort menu leaked blocked keys: actions=%+v", actions)
	}
	send(t, r, key("esc"))
	if r.Model.(ui.Model).SortOpen() {
		t.Fatal("Esc did not close Sort menu")
	}

	// Cycle through Author, Duration, Recent, Acquisition; verify k1 stays selected and order is deterministic.
	for _, wantSort := range []ui.SortOrder{ui.SortAuthor, ui.SortDuration, ui.SortRecent, ui.SortAcquisition} {
		send(t, r, key("s"), key("j"), key("k"), key("tab"), key("shift+tab"), key("down"), key("enter"))
		_, gotSort, _ := r.Model.(ui.Model).ViewState()
		if gotSort != wantSort || r.Model.(ui.Model).Selection() != "k1" {
			t.Fatalf("sort %v failed: got=%v sel=%q", wantSort, gotSort, r.Model.(ui.Model).Selection())
		}
	}
	for _, v := range []ui.LibraryView{ui.ViewAll, ui.ViewContinue, ui.ViewDownloaded, ui.ViewFavorites, ui.ViewFinished} {
		if v.Label() == "" {
			t.Fatalf("empty label for %v", v)
		}
	}
	// Verify SortAcquisition orders known timestamps descending before unknown timestamps.
	send(t, r, libraryFixture(), key("g"))
	if r.Model.(ui.Model).Selection() != "k3" {
		t.Fatalf("expected newest acquisition k3 first, got %q", r.Model.(ui.Model).Selection())
	}

	// Missing acquisition timestamps preserve CatalogOrder without inventing timestamps.
	noAcq := ui.SnapshotMsg{
		Books: []ui.Book{
			{Key: "c1", Title: "Z", CatalogOrder: 0},
			{Key: "c2", Title: "A", CatalogOrder: 1},
		},
	}
	send(t, r, noAcq)
	send(t, r, key("g"))
	if r.Model.(ui.Model).Selection() != "c1" {
		t.Fatalf("expected CatalogOrder 0 (c1) first under SortAcquisition without timestamps, got %q", r.Model.(ui.Model).Selection())
	}

	// Asynchronous refresh preserves selected BookKey even when reordered, and falls back when deleted.
	send(t, r, key("j")) // select c2
	reordered := ui.SnapshotMsg{
		Books: []ui.Book{
			{Key: "c2", Title: "A", CatalogOrder: 0},
			{Key: "c1", Title: "Z", CatalogOrder: 1},
		},
	}
	send(t, r, reordered)
	if r.Model.(ui.Model).Selection() != "c2" {
		t.Fatalf("async refresh lost selection c2: %q", r.Model.(ui.Model).Selection())
	}
	deleted := ui.SnapshotMsg{
		Books: []ui.Book{{Key: "c1", Title: "Z", CatalogOrder: 0}},
	}
	send(t, r, deleted)
	if r.Model.(ui.Model).Selection() != "c1" {
		t.Fatalf("deleted selection did not fall back to c1: %q", r.Model.(ui.Model).Selection())
	}

	// RestoreSessionMsg restores View, Sort, Filter, and Selection.
	v := ui.ViewDownloaded
	s := ui.SortDuration
	f := "alpha"
	send(t, r, libraryFixture(), ui.RestoreSessionMsg{View: &v, Sort: &s, Filter: &f, Selected: "k1"})
	gotV, gotS, gotF := r.Model.(ui.Model).ViewState()
	if gotV != ui.ViewDownloaded || gotS != ui.SortDuration || gotF != "alpha" {
		t.Fatalf("RestoreSessionMsg failed: view=%v sort=%v filter=%q", gotV, gotS, gotF)
	}
}
