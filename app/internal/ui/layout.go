// Package ui contains terminal presentation independent of providers and storage.
package ui

const (
	MinWidth   = 80
	MinHeight  = 20
	WideWidth  = 144
	WideHeight = 42
)

// LayoutMode describes the supported composition, not a second navigation model.
type LayoutMode int

const (
	ResizeOnly LayoutMode = iota
	Compact
	Wide
)

// Rect includes every border/padding cell allocated to a region.
// Renderers must fit inside it, rather than adding chrome outside its budget.
type Rect struct {
	X, Y, Width, Height int
}

// Layout is shared by rendering, paging and dialog placement. Content is the
// envelope above Player; in Compact it displays the focused logical pane.
// Individual pane rectangles are populated only in Wide mode.
type Layout struct {
	Mode                              LayoutMode
	Screen, Content                   Rect
	Library, Books, Details, Chapters Rect
	Player, Activity, Hints           Rect
}

// ComputeLayout applies the approved R1 geometry without accessing UI state.
// Unsupported sizes retain only Screen; hiding a modal never discards its draft.
func ComputeLayout(width, height int) Layout {
	width, height = max(0, width), max(0, height)
	layout := Layout{Screen: Rect{Width: width, Height: height}}
	if width < MinWidth || height < MinHeight {
		return layout
	}
	layout.Mode = Compact
	playerRows := 5
	if width >= WideWidth && height >= WideHeight {
		layout.Mode = Wide
		playerRows = 6
	}
	contentRows := height - playerRows - 2 // activity and hints own one row each
	layout.Content = Rect{Width: width, Height: contentRows}
	layout.Player = Rect{Y: contentRows, Width: width, Height: playerRows}
	layout.Activity = Rect{Y: height - 2, Width: width, Height: 1}
	layout.Hints = Rect{Y: height - 1, Width: width, Height: 1}
	if layout.Mode == Wide {
		// Podmutt-style proportional distribution:
		// - Library [1]: min(28, width/5), at least 24 columns (~19%)
		// - Chapters [3]: ~35% of width (width/3 + width/72), at least 42 columns (50 at 144)
		// - Books [2] & Details [4]: remaining middle width (~46%), 50%/50% vertical split
		libraryWidth := max(24, min(28, width/5))
		chaptersWidth := max(42, width/3+width/72)
		middleWidth := width - libraryWidth - chaptersWidth
		bookRows := contentRows / 2
		layout.Library = Rect{Width: libraryWidth, Height: contentRows}
		layout.Books = Rect{X: libraryWidth, Width: middleWidth, Height: bookRows}
		layout.Details = Rect{X: libraryWidth, Y: bookRows, Width: middleWidth, Height: contentRows - bookRows}
		layout.Chapters = Rect{X: width - chaptersWidth, Width: chaptersWidth, Height: contentRows}
	}
	return layout
}

// Dialog centers a preferred size inside Content with at least two side columns
// and one vertical row of margin. Unsupported/empty dialogs have no rectangle.
func (layout Layout) Dialog(width, height int) Rect {
	if layout.Mode == ResizeOnly || width <= 0 || height <= 0 || layout.Content.Width <= 4 || layout.Content.Height <= 2 {
		return Rect{}
	}
	width = min(width, layout.Content.Width-4)
	height = min(height, layout.Content.Height-2)
	return Rect{
		X:      layout.Content.X + (layout.Content.Width-width)/2,
		Y:      layout.Content.Y + (layout.Content.Height-height)/2,
		Width:  width,
		Height: height,
	}
}
