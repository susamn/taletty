package ui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestHelpOwnsKeysAndRestoresEveryRootOrigin(t *testing.T) {
	for pane := ui.Library; pane <= ui.PlayerPane; pane++ {
		var actions []ui.Action
		r := start(t, fixture(), func(action ui.Action) tea.Cmd {
			actions = append(actions, action)
			return nil
		})
		screen(t, r, 80, 20)
		send(t, r, key("j"), key(string(rune('1'+pane))))
		origin := r.Model.View().Content
		send(t, r, key("?"))
		help := r.Model.View().Content
		if !strings.Contains(help, "[Focus] Help") || strings.Count(help, "[Focus]") != 1 {
			t.Fatal("Help must be the only focused view, including from Player")
		}
		for _, blocked := range []string{"tab", "shift+tab", "1", "2", "3", "4", "5", "q", "ctrl+c", "b", "B", "f", "x", "D", "S", "a", "t", "/", "enter", "left", "right", "?"} {
			send(t, r, key(blocked))
			if r.Model.View().Content != help || len(actions) != 0 {
				t.Fatalf("Help key %q leaked to root pane %v", blocked, pane)
			}
		}
		if r.Model.(ui.Model).Focus() != pane || r.Model.(ui.Model).Selection() != "B" {
			t.Fatal("Help changed underlying focus/selection")
		}
		send(t, r, key("esc"))
		if r.Model.View().Content != origin {
			t.Fatal("Esc did not restore the exact origin, controls and selection")
		}
		send(t, r, key("tab"))
		if r.Model.(ui.Model).Focus() != (pane+1)%5 {
			t.Fatal("root input was not restored after closing Help")
		}
	}
}

func TestHelpWhitelistsTransportOnceWithoutOptimisticPlayback(t *testing.T) {
	data := fixture()
	var actions []ui.Action
	r := start(t, data, func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return func() tea.Msg {
			data.Player.State = ui.Paused
			return data
		}
	})
	screen(t, r, 144, 42)
	send(t, r, key("j"), key("5"), key("right"), key("?"))
	for i, name := range []string{" ", ",", ".", "-", "=", "[", "]"} {
		send(t, r, key(name))
		kind := []ui.ActionKind{ui.TogglePlayback, ui.SeekBackward, ui.SeekForward, ui.VolumeDown, ui.VolumeUp, ui.SpeedDown, ui.SpeedUp}[i]
		if len(actions) != i+1 || actions[i] != (ui.Action{Kind: kind, Key: "A", Session: "session-a"}) {
			t.Fatalf("transport dispatched incorrectly or twice: %+v", actions)
		}
	}
	if !strings.Contains(r.Model.View().Content, "Player - Playing") {
		t.Fatal("Help transport optimistically changed playback")
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Player - Paused") || !strings.Contains(r.Model.View().Content, "[Focus] Help") || r.Model.(ui.Model).Selection() != "B" {
		t.Fatal("completion stole Help/selection or failed to repaint player")
	}
	data.Player.Key, data.Player.Session, data.Player.Title = "C", "session-c", "Cloud"
	send(t, r, data, key(" "))
	if len(actions) != 8 || actions[7] != (ui.Action{Kind: ui.TogglePlayback, Key: "C", Session: "session-c"}) {
		t.Fatal("Help transport did not follow the authoritative current session")
	}
	send(t, r, key("right"), key("esc"), key("enter"))
	if actions[len(actions)-1].Kind != ui.SeekBackward {
		t.Fatal("Help leaked Right to the underlying player control")
	}
}

func TestHelpScrollsLongSafeErrorsAndPreservesReadingPositionOnResize(t *testing.T) {
	data := fixture()
	data.Error = strings.Repeat("界e\u0301 👩‍💻 ", 1000) + " FINAL-DETAIL\x1b]0;ATTACK\x07"
	r := start(t, data, nil)
	screen(t, r, 80, 20)
	send(t, r, key("?"))
	for _, step := range []struct{ key, want string }{
		{"j", "Help 2-"}, {"down", "Help 3-"}, {"k", "Help 2-"}, {"up", "Help 1-"}, {"up", "Help 1-"},
		{"pgdown", "Help 12-"}, {"ctrl+d", "Help 17-"}, {"ctrl+u", "Help 12-"}, {"pgup", "Help 1-"},
	} {
		send(t, r, key(step.key))
		if !strings.Contains(r.Model.View().Content, step.want) {
			t.Fatalf("Help scroll %q did not reach %q", step.key, step.want)
		}
	}
	send(t, r, key("pgdown"))
	for _, mode := range []ui.GlyphMode{ui.Plain, ui.Unicode} {
		send(t, r, ui.AppearanceMsg{Glyphs: mode})
		for _, size := range [][2]int{{144, 42}, {80, 42}, {144, 20}, {80, 20}} {
			view := screen(t, r, size[0], size[1])
			if strings.Contains(r.Model.View().Content, "\x1b") || strings.Contains(view, "ATTACK") {
				t.Fatal("unsafe controls reached Help")
			}
		}
		if !strings.Contains(r.Model.View().Content, "Help 12-") {
			t.Fatal("resize/repaint discarded reading position")
		}
	}
	send(t, r, key("end"))
	end := r.Model.View().Content
	if !strings.Contains(end, "FINAL-DETAIL") {
		t.Fatal("long error details cannot be read at the bottom")
	}
	send(t, r, key("G"), key("down"))
	if r.Model.View().Content != end {
		t.Fatal("Help scroll escaped the last page")
	}
	send(t, r, ui.NoticeMsg{Activity: "Unrelated success"})
	if !strings.Contains(r.Model.View().Content, "FINAL-DETAIL") {
		t.Fatal("background success discarded error details")
	}
	send(t, r, ui.DismissFailureMsg{})
	if !strings.Contains(r.Model.View().Content, "No recorded failure") {
		t.Fatal("shortened content did not clamp the Help viewport")
	}
	send(t, r, key("home"), key("g"))
	if !strings.Contains(r.Model.View().Content, "Help 1-") {
		t.Fatal("Home/g did not restore first page")
	}
}

func TestHiddenHelpYieldsOnlyToResizeQuitAndRecoversAfterShutdownFailure(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return func() tea.Msg { return ui.NoticeMsg{Error: "Shutdown failed; retry"} }
	})
	screen(t, r, 80, 20)
	send(t, r, key("j"), key("?"), key("pgdown"))
	for _, size := range [][2]int{{79, 20}, {80, 19}, {0, 0}, {1, 1}} {
		screen(t, r, size[0], size[1])
		for _, name := range []string{"esc", "tab", "1", "?", " ", "enter", "b"} {
			send(t, r, key(name))
		}
	}
	if len(actions) != 0 {
		t.Fatal("hidden Help accepted transport/context input")
	}
	send(t, r, key("q"), key("ctrl+c"))
	if len(actions) != 2 || actions[0] != (ui.Action{Kind: ui.RequestQuit}) || actions[1].Kind != ui.RequestQuit {
		t.Fatal("resize owner did not request orderly shutdown")
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Help 12-") || !strings.Contains(view, "Shutdown failed; retry") || r.Model.(ui.Model).Focus() != ui.Books || r.Model.(ui.Model).Selection() != "B" {
		t.Fatal("failed shutdown/resize lost Help, reading position or origin")
	}
	send(t, r, key("esc"))
	if !strings.Contains(r.Model.View().Content, "[Focus] Books") {
		t.Fatal("recovered Help could not return to its origin")
	}
}

func TestHelpLabelsDisabledReasonsWithoutDispatch(t *testing.T) {
	for _, pane := range []string{"2", "5"} {
		for _, connected := range []bool{false, true} {
			calls := 0
			var dispatch func(ui.Action) tea.Cmd
			if connected {
				dispatch = func(ui.Action) tea.Cmd {
					calls++
					return nil
				}
			}
			r := start(t, ui.SnapshotMsg{}, dispatch)
			screen(t, r, 144, 42)
			send(t, r, key(pane), key("?"), key(" "))
			view := r.Model.View().Content
			reason := "no coordinator"
			if connected {
				reason = "no loaded audio"
			}
			if !strings.Contains(view, "Transport disabled: "+reason) || calls != 0 || !strings.Contains(view, "No current audio") {
				t.Fatal("missing disabled reason or unexpected request")
			}
			if connected && pane == "2" && !strings.Contains(view, "Enter disabled: no highlighted book") {
				t.Fatal("empty origin advertised play")
			}
			if connected && pane == "5" && !strings.Contains(view, "Enter disabled: no loaded audio") {
				t.Fatal("empty player advertised activation")
			}
		}
	}
}
