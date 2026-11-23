package ui_test

import (
	"math"
	"testing"

	"github.com/susamn/taletty/app/internal/ui"
)

func TestApprovedReferenceBudgets(t *testing.T) {
	wide := ui.ComputeLayout(144, 42)
	want := ui.Layout{
		Mode: ui.Wide, Screen: ui.Rect{Width: 144, Height: 42},
		Content:  ui.Rect{Width: 144, Height: 34},
		Library:  ui.Rect{Width: 28, Height: 34},
		Books:    ui.Rect{X: 28, Width: 66, Height: 17},
		Details:  ui.Rect{X: 28, Y: 17, Width: 66, Height: 17},
		Chapters: ui.Rect{X: 94, Width: 50, Height: 34},
		Player:   ui.Rect{Y: 34, Width: 144, Height: 6},
		Activity: ui.Rect{Y: 40, Width: 144, Height: 1},
		Hints:    ui.Rect{Y: 41, Width: 144, Height: 1},
	}
	if wide != want {
		t.Fatalf("W1 geometry:\n got %+v\nwant %+v", wide, want)
	}
	for _, height := range []int{20, 28, 42} {
		compact := ui.ComputeLayout(80, height)
		if compact.Mode != ui.Compact || compact.Content.Height != height-7 || compact.Player.Height != 5 {
			t.Fatalf("compact budget at 80x%d: %+v", height, compact)
		}
		if compact.Library != (ui.Rect{}) || compact.Books != (ui.Rect{}) || compact.Details != (ui.Rect{}) || compact.Chapters != (ui.Rect{}) {
			t.Fatal("compact must not allocate hidden wide panes")
		}
	}
}

func TestEveryResponsiveBoundaryStaysBoundedAndDisjoint(t *testing.T) {
	widths := []int{-1, 0, 1, 79, 80, 81, 143, 144, 145, 200, math.MaxInt}
	heights := []int{-1, 0, 1, 19, 20, 21, 41, 42, 43, 60, math.MaxInt}
	for _, width := range widths {
		for _, height := range heights {
			layout := ui.ComputeLayout(width, height)
			if layout.Screen.Width != max(0, width) || layout.Screen.Height != max(0, height) {
				t.Fatalf("resize normalization: %+v", layout)
			}
			if width < 80 || height < 20 {
				if layout.Mode != ui.ResizeOnly || layout.Content != (ui.Rect{}) || layout.Player != (ui.Rect{}) || layout.Activity != (ui.Rect{}) || layout.Hints != (ui.Rect{}) {
					t.Fatalf("below-minimum allocation at %dx%d: %+v", width, height, layout)
				}
				continue
			}
			regions := []ui.Rect{layout.Player, layout.Activity, layout.Hints}
			if width >= 144 && height >= 42 {
				if layout.Mode != ui.Wide || layout.Library.Width+layout.Books.Width+layout.Chapters.Width != width || layout.Details.Height+layout.Books.Height != layout.Content.Height {
					t.Fatalf("wide budget at %dx%d: %+v", width, height, layout)
				}
				regions = append(regions, layout.Library, layout.Books, layout.Details, layout.Chapters)
			} else {
				if layout.Mode != ui.Compact {
					t.Fatalf("incorrect wide breakpoint at %dx%d", width, height)
				}
				regions = append(regions, layout.Content)
			}
			if layout.Player.Width != width || layout.Player.Y != layout.Content.Height || layout.Player.Y+layout.Player.Height != layout.Activity.Y || layout.Activity.Y+1 != layout.Hints.Y {
				t.Fatalf("player/status discontinuity at %dx%d", width, height)
			}
			for i, region := range regions {
				if region.Width <= 0 || region.Height <= 0 || region.X < 0 || region.Y < 0 || region.X+region.Width > width || region.Y+region.Height > height {
					t.Fatalf("out-of-bounds region at %dx%d: %+v", width, height, region)
				}
				for _, other := range regions[i+1:] {
					if region.X < other.X+other.Width && other.X < region.X+region.Width && region.Y < other.Y+other.Height && other.Y < region.Y+region.Height {
						t.Fatalf("overlapping regions: %+v / %+v", region, other)
					}
				}
			}
		}
	}
}

func TestDialogsStayInContentAndNeverCoverPlayer(t *testing.T) {
	minimum := ui.ComputeLayout(80, 20)
	if got := minimum.Dialog(1000, 1000); got != (ui.Rect{X: 2, Y: 1, Width: 76, Height: 11}) {
		t.Fatalf("minimum dialog: %+v", got)
	}
	for _, dimensions := range [][2]int{{80, 20}, {80, 28}, {144, 42}, {200, 60}} {
		layout := ui.ComputeLayout(dimensions[0], dimensions[1])
		for _, preferred := range [][2]int{{1, 1}, {70, 15}, {999, 999}} {
			dialog := layout.Dialog(preferred[0], preferred[1])
			if dialog.X < 2 || dialog.Y < 1 || dialog.X+dialog.Width > layout.Content.Width-2 || dialog.Y+dialog.Height > layout.Content.Height-1 {
				t.Fatalf("dialog escaped margins: %+v in %+v", dialog, layout)
			}
			if left, right := dialog.X, layout.Content.Width-dialog.X-dialog.Width; left-right > 1 || right-left > 1 {
				t.Fatal("dialog not horizontally centered")
			}
		}
	}
	for _, size := range [][2]int{{0, 2}, {2, 0}, {-1, 2}, {2, -1}} {
		if minimum.Dialog(size[0], size[1]) != (ui.Rect{}) {
			t.Fatal("empty dialog allocated cells")
		}
	}
	for _, layout := range []ui.Layout{ui.ComputeLayout(79, 20), {}, {Mode: ui.Compact, Content: ui.Rect{Width: 4, Height: 13}}, {Mode: ui.Compact, Content: ui.Rect{Width: 80, Height: 2}}} {
		if layout.Dialog(70, 15) != (ui.Rect{}) {
			t.Fatal("unsupported content allocated a dialog")
		}
	}
}
