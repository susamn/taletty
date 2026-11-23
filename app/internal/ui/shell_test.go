package ui_test

import (
	"context"
	"flag"
	"image/color"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/ui"
)

func fixture() ui.SnapshotMsg {
	return ui.SnapshotMsg{
		Books: []ui.Book{
			{Key: "A", Title: "River", Author: "Writer A", Availability: ui.Ready, Chapters: []string{"A chapter"}},
			{Key: "B", Title: "Stars", Author: "Writer B", Availability: ui.Queued, Chapters: []string{"B chapter"}},
		},
		Player: ui.Player{Key: "A", Session: "session-a", Title: "River", Chapter: "A chapter", State: ui.Playing,
			Position: 12 * time.Second, Duration: time.Hour, Speed: 1.5, Volume: 75, VolumeKnown: true},
		Activity: "Offline cache",
	}
}

func start(t *testing.T, data ui.SnapshotMsg, dispatch func(ui.Action) tea.Cmd) *testkit.Runner {
	t.Helper()
	r, err := testkit.New(context.Background(), ui.NewModel(data, dispatch), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	return r
}

func send(t *testing.T, r *testkit.Runner, messages ...tea.Msg) {
	t.Helper()
	for _, message := range messages {
		if err := r.Send(message); err != nil {
			t.Fatal(err)
		}
	}
}

func key(name string) tea.KeyPressMsg {
	codes := map[string]rune{
		"tab": tea.KeyTab, "left": tea.KeyLeft, "right": tea.KeyRight, "up": tea.KeyUp, "down": tea.KeyDown,
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "home": tea.KeyHome, "end": tea.KeyEnd,
		"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
	}
	if name == "shift+tab" {
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	}
	if strings.HasPrefix(name, "ctrl+") {
		return tea.KeyPressMsg{Code: rune(name[len("ctrl+")]), Mod: tea.ModCtrl}
	}
	if code, ok := codes[name]; ok {
		return tea.KeyPressMsg{Code: code}
	}
	code, _ := utf8.DecodeRuneInString(name)
	return tea.KeyPressMsg{Code: code, Text: name}
}

func screen(t *testing.T, r *testkit.Runner, width, height int) string {
	t.Helper()
	send(t, r, tea.WindowSizeMsg{Width: width, Height: height})
	view := ansi.Strip(r.Model.View().Content)
	if width <= 0 || height <= 0 {
		if view != "" {
			t.Fatalf("empty terminal produced %q", view)
		}
		return view
	}
	lines := strings.Split(view, "\n")
	if width >= 80 && height >= 20 && len(lines) != height || len(lines) > height {
		t.Fatalf("wrong rendered height at %dx%d: %d", width, height, len(lines))
	}
	for _, line := range lines {
		if ansi.StringWidth(line) != width {
			t.Fatalf("wrong cell width at %dx%d: %d %q", width, height, ansi.StringWidth(line), line)
		}
	}
	return view
}

func TestShellBrowseAndCommandCompletionNeverConfuseCurrentAudio(t *testing.T) {
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
	send(t, r, key("j"), key("j"), key("k"), key("down"))
	if r.Model.(ui.Model).Selection() != "B" || len(actions) != 0 {
		t.Fatal("browsing dispatched an action or lost selection")
	}
	view := r.Model.View().Content
	for _, want := range []string{"Details - Stars", "Chapters - Stars", "Player - Playing", "River | A chapter", "Remaining 00:39:52 at speed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing independent target %q", want)
		}
	}
	send(t, r, key(" "))
	if len(actions) != 1 || actions[0] != (ui.Action{Kind: ui.TogglePlayback, Key: "A", Session: "session-a"}) || r.Pending() != 1 {
		t.Fatalf("wrong current-audio action: %+v", actions)
	}
	if !strings.Contains(r.Model.View().Content, "Player - Playing") {
		t.Fatal("optimistic UI changed playback before command completion")
	}
	if err := r.Step(0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Player - Paused") || r.Model.(ui.Model).Selection() != "B" {
		t.Fatal("snapshot stole browsing target or did not repaint")
	}
	send(t, r, key("enter"), key("4"), key("enter"))
	for _, action := range actions[1:] {
		if action != (ui.Action{Kind: ui.PlayBook, Key: "B"}) {
			t.Fatalf("explicit play targeted wrong book: %+v", action)
		}
	}
	if !strings.Contains(r.Model.View().Content, "River | A chapter") {
		t.Fatal("explicit request switched audio before coordinator completion")
	}
}

func TestShellFocusResizeAndAppearanceJourney(t *testing.T) {
	r := start(t, fixture(), nil)
	screen(t, r, 144, 42)
	for _, want := range []ui.Pane{ui.Chapters, ui.Details, ui.PlayerPane, ui.Library, ui.Books} {
		send(t, r, key("tab"))
		if r.Model.(ui.Model).Focus() != want {
			t.Fatalf("tab focus: want %v", want)
		}
	}
	send(t, r, key("shift+tab"))
	if r.Model.(ui.Model).Focus() != ui.Library {
		t.Fatal("reverse tab did not wrap")
	}
	for number, want := range []ui.Pane{ui.Library, ui.Books, ui.Chapters, ui.Details, ui.PlayerPane} {
		send(t, r, key(string(rune('1'+number))))
		if r.Model.(ui.Model).Focus() != want {
			t.Fatal("numeric focus failed")
		}
		screen(t, r, 144, 42)
	}
	dimensions := [][2]int{{0, 0}, {-1, -1}, {1, 1}, {144, 20}, {80, 42}}
	for _, width := range []int{79, 80, 81, 143, 144, 145} {
		for _, height := range []int{19, 20, 21, 41, 42, 43} {
			dimensions = append(dimensions, [2]int{width, height})
		}
	}
	for _, size := range dimensions {
		screen(t, r, size[0], size[1])
		if r.Model.(ui.Model).Focus() != ui.PlayerPane {
			t.Fatal("resize changed logical focus")
		}
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "Details - River") || strings.Count(view, "Player - Playing") != 1 {
		t.Fatal("compact Player focus lost last content pane or duplicated player")
	}
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Plain, Palette: ui.Palette{Background: color.Black, Focus: color.RGBA{R: 255, A: 255}, Border: color.White, Text: color.White, Selection: color.White, Muted: color.White, Error: color.White}})
	styled := r.Model.View().Content
	if !strings.Contains(styled, "\x1b[") || !strings.Contains(ansi.Strip(styled), "[Focus] Player") || strings.Contains(styled, "●") {
		t.Fatal("semantic palette/plain repaint failed")
	}
	screen(t, r, 144, 42)
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Plain, Palette: ui.Palette{Background: color.Black}})
	screen(t, r, 80, 20)
}

func TestShellSnapshotIsolationStableIdentityAndStickyFailure(t *testing.T) {
	data := fixture()
	r := start(t, data, nil)
	data.Books[0].Title, data.Books[0].Chapters[0] = "mutated", "mutated"
	view := screen(t, r, 144, 42)
	if strings.Contains(view, "mutated") {
		t.Fatal("constructor retained mutable input")
	}
	send(t, r, key("j"), ui.NoticeMsg{Error: "Disk full\x1b[2J", Activity: "Failed"})
	data = fixture()
	data.Books[0], data.Books[1] = data.Books[1], data.Books[0]
	send(t, r, data, ui.NoticeMsg{Activity: "Unrelated success"}, nil)
	data.Books[0].Chapters[0] = "mutated"
	if r.Model.(ui.Model).Selection() != "B" || !strings.Contains(r.Model.View().Content, "Error: Disk full") || strings.Contains(r.Model.View().Content, "mutated") {
		t.Fatal("refresh lost identity, isolation or sticky failure")
	}
	send(t, r, ui.DismissFailureMsg{})
	if !strings.Contains(r.Model.View().Content, "Unrelated success") {
		t.Fatal("failure dismissal did not reveal activity")
	}
	data = fixture()
	data.Books = data.Books[:1]
	data.Error = "Snapshot failure"
	send(t, r, data)
	if r.Model.(ui.Model).Selection() != "A" || !strings.Contains(r.Model.View().Content, "Snapshot failure") {
		t.Fatal("removed selection fallback/snapshot error failed")
	}
	data.Books = nil
	send(t, r, data)
	if r.Model.(ui.Model).Selection() != "" || !strings.Contains(r.Model.View().Content, "River | A chapter") {
		t.Fatal("empty library lost independent current audio")
	}
}

func TestShellTypedTransportControlsAndQuit(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return func() tea.Msg { return ui.NoticeMsg{Activity: "Request received"} }
	})
	screen(t, r, 80, 20)
	for i, name := range []string{" ", ",", ".", "-", "=", "[", "]"} {
		send(t, r, key(name))
		want := []ui.ActionKind{ui.TogglePlayback, ui.SeekBackward, ui.SeekForward, ui.VolumeDown, ui.VolumeUp, ui.SpeedDown, ui.SpeedUp}[i]
		if actions[len(actions)-1] != (ui.Action{Kind: want, Key: "A", Session: "session-a"}) {
			t.Fatalf("wrong transport key %q: %+v", name, actions)
		}
	}
	send(t, r, key("5"), key("left"), key("enter"), key("right"), key("right"), key("enter"))
	if actions[7].Kind != ui.SeekForward || actions[8].Kind != ui.SeekBackward {
		t.Fatal("Player arrows did not select controls")
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	replacement := fixture()
	replacement.Player.Session = "replacement-session"
	send(t, r, replacement, key(" "))
	if actions[len(actions)-1].Session != "replacement-session" || actions[0].Session != "session-a" {
		t.Fatal("same-book replacement lost source session identity")
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	screen(t, r, 79, 19)
	before := len(actions)
	send(t, r, key("2"), key(" "), key("j"), key("enter"))
	if len(actions) != before || r.Model.(ui.Model).Focus() != ui.PlayerPane {
		t.Fatal("hidden input escaped resize owner")
	}
	send(t, r, key("q"), key("ctrl+c"))
	if actions[before] != (ui.Action{Kind: ui.RequestQuit}) || actions[before+1].Kind != ui.RequestQuit {
		t.Fatal("quit did not request coordinator cleanup")
	}
}

func TestShellEmptyMetadataAndStateRenderingRemainTruthful(t *testing.T) {
	r := start(t, ui.SnapshotMsg{}, nil)
	if r.Model.View().Content != "" {
		t.Fatal("unmeasured startup emitted an overflowing view")
	}
	view := screen(t, r, 80, 20)
	for _, want := range []string{"No books cached", "No current audio", "Transport disabled"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing empty state %q", want)
		}
	}
	send(t, r, key("up"), key("enter"), key(" "), key("q"), key("4"))
	if !strings.Contains(r.Model.View().Content, "No book highlighted") {
		t.Fatal("empty details fabricated a target")
	}
	for _, state := range []ui.PlaybackState{ui.Stopped, ui.Playing, ui.Paused, ui.Finished, ui.PlaybackFailed} {
		for _, glyphs := range []ui.GlyphMode{ui.Plain, ui.Unicode} {
			for _, availability := range []ui.Availability{-1, ui.AvailabilityUnknown, ui.NotDownloaded, ui.Queued, ui.Downloading, ui.Preparing, ui.Ready, ui.Failed, ui.Canceled} {
				data := ui.SnapshotMsg{Books: []ui.Book{{Key: "empty", Favorite: true, Availability: availability}}, Player: ui.Player{Key: "empty", State: state}}
				send(t, r, data, ui.AppearanceMsg{Glyphs: glyphs}, key("2"))
				view := screen(t, r, 144, 42)
				stateName := []string{"Stopped", "Playing", "Paused", "Finished", "Failed"}[state]
				statusNames := []string{"Availability unknown", "Not downloaded", "Queued", "Downloading", "Preparing", "Ready", "Failed", "Canceled"}
				statusName := statusNames[max(0, availability)]
				if !strings.Contains(view, "Player - "+stateName) || !strings.Contains(view, statusName) {
					t.Fatalf("missing named states %s / %s", stateName, statusName)
				}
				for _, want := range []string{"Untitled", "No chapters available", "No chapter metadata", "unknown duration", "Speed unknown", "Volume unknown", "Remaining unknown"} {
					if !strings.Contains(view, want) {
						t.Fatalf("missing honest metadata %q", want)
					}
				}
				favorite := "[Fav]"
				if glyphs == ui.Unicode {
					favorite = "★"
				}
				if !strings.Contains(view, favorite) {
					t.Fatal("glyph mode hid favorite state")
				}
				if strings.Contains(view, "●") || strings.Contains(view, "[o") {
					t.Fatal("unknown duration invented determinate progress")
				}
			}
		}
	}
}

func TestShellLongUnicodeSanitizedAndCursorKeptInView(t *testing.T) {
	data := fixture()
	for i := 0; i < 50; i++ {
		data.Books = append(data.Books, ui.Book{Key: string(rune(1000 + i)), Title: strings.Repeat("界e\u0301👩‍💻\x1b[2J", 20), Description: "\x1b]0;ATTACK\x07Description\nMore", Chapters: []string{strings.Repeat("章", 100)}})
	}
	r := start(t, data, nil)
	screen(t, r, 80, 20)
	for range 100 {
		send(t, r, key("j"))
	}
	if r.Model.(ui.Model).Selection() != data.Books[len(data.Books)-1].Key {
		t.Fatal("cursor not bounded at last row")
	}
	for _, glyphs := range []ui.GlyphMode{ui.Plain, ui.Unicode} {
		send(t, r, ui.AppearanceMsg{Glyphs: glyphs})
		for _, size := range [][2]int{{80, 20}, {144, 42}, {145, 43}} {
			view := screen(t, r, size[0], size[1])
			if strings.Contains(r.Model.View().Content, "\x1b") || strings.Contains(view, "ATTACK") || !strings.Contains(view, ">") || !strings.Contains(view, "[Focus] Books") || !strings.Contains(view, "Availability unknown") {
				t.Fatal("unsafe metadata, lost cursor or hidden focus tag")
			}
		}
	}
}

func TestShellPlayerTimingBoundsAndDisabledRequests(t *testing.T) {
	for _, speed := range []float64{0, math.NaN(), math.Inf(1), 1e-300, 1.5} {
		for _, position := range []time.Duration{-time.Second, 12 * time.Second, 2 * time.Hour} {
			data := fixture()
			data.Player.Speed, data.Player.Position, data.Player.Sleep = speed, position, "12m"
			r := start(t, data, nil)
			view := screen(t, r, 144, 42)
			if strings.Contains(view, "NaN") || strings.Contains(view, "+Inf") || !strings.Contains(view, "Sleep 12m") {
				t.Fatal("invalid player formatting")
			}
			send(t, r, key(" "))
			if !strings.Contains(r.Model.View().Content, "Action unavailable: no coordinator") {
				t.Fatal("nil dispatcher silently accepted transport")
			}
		}
	}
	data := fixture()
	data.Player.State = ui.Stopped
	r := start(t, data, nil)
	screen(t, r, 80, 20)
	send(t, r, key(" "))
	if r.Pending() != 0 || !strings.Contains(r.Model.View().Content, "Transport disabled: no loaded audio") {
		t.Fatal("stopped state accepted loaded transport")
	}
	data.Player.State, data.Player.Session = ui.Playing, ""
	send(t, r, data, key(" "))
	if r.Pending() != 0 || !strings.Contains(r.Model.View().Content, "Transport disabled: no loaded audio") {
		t.Fatal("sessionless snapshot accepted unsafe transport")
	}
}

var updateShellGolden = flag.Bool("update-shell-golden", false, "rewrite reviewed shell terminal fixture")

func TestShellMinimumPlainGolden(t *testing.T) {
	r := start(t, fixture(), func(ui.Action) tea.Cmd { return nil })
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Plain}, ui.NoticeMsg{Error: "Offline: retry explicitly"})
	lines := strings.Split(screen(t, r, 80, 20), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	got := strings.Join(lines, "\n") + "\n"
	path := "testdata/shell-minimum-plain.golden"
	if *updateShellGolden {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("review shell changes before updating %s:\n%s", path, got)
	}
}
