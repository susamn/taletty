package ui_test

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestFavoritesViewToggleDetailsAndPlainUnicodeModes(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Highlight B (Stars) while A (River) is playing; f and m both target B, never A.
	send(t, r, key("j"), key("f"), key("m"))
	if len(actions) != 2 || actions[0] != (ui.Action{Kind: ui.ToggleFavorite, Key: "B"}) || actions[1] != (ui.Action{Kind: ui.ToggleFavorite, Key: "B"}) {
		t.Fatalf("f/m did not target highlighted book B: %+v", actions)
	}

	// f in PlayerPane or with no highlighted book is unavailable.
	send(t, r, key("5"), key("f"))
	if len(actions) != 2 || !strings.Contains(r.Model.View().Content, "Favorite toggle unavailable in Player") {
		t.Fatalf("f should be unavailable in PlayerPane: %+v", actions)
	}
	send(t, r, ui.SnapshotMsg{}, key("2"), key("f"))
	if len(actions) != 2 || !strings.Contains(r.Model.View().Content, "Favorite toggle unavailable: no highlighted book") {
		t.Fatalf("f should be unavailable with no highlighted book: %+v", actions)
	}

	// Restore fixture with B favorited; verify FAV column, Unicode ★, and Plain [Fav] in Books and Details with accent color.
	favData := fixture()
	favData.Books[1].Favorite = true
	send(t, r, favData, key("j"))
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Unicode, Palette: ui.Palette{Focus: color.White, Selection: color.White}})
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "| FAV") || !strings.Contains(view, "★ Stars") || !strings.Contains(view, "★ Favorite") || !strings.Contains(view, "Queued") {
		t.Fatalf("missing FAV column, Unicode favorite indicator, or preserved availability:\n%s", view)
	}
	// Also verify Book Actions menu ('x') includes Remove from Favorites (f / m).
	send(t, r, key("x"))
	if !strings.Contains(r.Model.View().Content, "Remove from Favorites (f / m)") {
		t.Fatalf("missing Remove from Favorites in Book Actions menu:\n%s", r.Model.View().Content)
	}
	send(t, r, key("esc"))
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Plain, Palette: ui.Palette{Focus: color.White}})
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "| [Fav] |") || !strings.Contains(view, "[Fav] Stars") || !strings.Contains(view, "[Fav] Favorite") || !strings.Contains(view, "Queued") {
		t.Fatalf("missing Plain FAV column, favorite indicator, or preserved availability:\n%s", view)
	}

	// Open Favorites view via Library sidebar (index 4: All, Continue, Downloaded, Favorites).
	send(t, r, key("1"), key("g"), key("j"), key("j"), key("j"), key("enter"))
	v, _, _ := r.Model.(ui.Model).ViewState()
	if v != ui.ViewFavorites || r.Model.(ui.Model).Selection() != "B" {
		t.Fatalf("Favorites view switch failed: view=%v sel=%q", v, r.Model.(ui.Model).Selection())
	}

	// Unfavoriting final visible row shows empty Favorites guidance back to All.
	unfav := fixture()
	send(t, r, unfav)
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "No books in Favorites (press 1 for All)") || r.Model.(ui.Model).Selection() != "" {
		t.Fatalf("missing empty Favorites guidance:\n%s", view)
	}

	// RestoreSessionMsg restores ViewFavorites.
	favView := ui.ViewFavorites
	send(t, r, favData, ui.RestoreSessionMsg{View: &favView, Selected: "B"})
	v, _, _ = r.Model.(ui.Model).ViewState()
	if v != ui.ViewFavorites || r.Model.(ui.Model).Selection() != "B" {
		t.Fatalf("RestoreSessionMsg did not restore ViewFavorites: view=%v sel=%q", v, r.Model.(ui.Model).Selection())
	}
}
