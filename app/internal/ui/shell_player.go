package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func clock(duration time.Duration) string {
	seconds := int64(max(0, duration) / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
}

func playbackLabel(state PlaybackState) string {
	switch state {
	case Playing:
		return "Playing"
	case Paused:
		return "Paused"
	case Finished:
		return "Finished"
	case PlaybackFailed:
		return "Failed"
	default:
		return "Stopped"
	}
}

func (m Model) timeline(width int) string {
	p := m.data.Player
	duration := "unknown duration"
	if p.Duration > 0 {
		duration = clock(p.Duration)
	}
	labels := clock(p.Position) + " / " + duration
	trackWidth := max(1, width-ansi.StringWidth(labels)-3)
	line, dot, tick := "-", "o", "|"
	if m.appearance.Glyphs != Plain {
		line, dot, tick = "─", "●", "│"
	}
	cells := make([]string, trackWidth)
	for i := range cells {
		cells[i] = line
	}
	if p.Duration > 0 {
		for _, start := range p.ChapterTicks {
			if start <= 0 || start >= p.Duration {
				continue
			}
			ratio := min(1.0, max(0.0, float64(start)/float64(p.Duration)))
			pos := int(float64(trackWidth-1) * ratio)
			cells[pos] = tick
		}
		ratio := min(1.0, max(0.0, float64(p.Position)/float64(p.Duration)))
		position := int(float64(trackWidth-1) * ratio)
		cells[position] = dot
	}
	return labels + " [" + strings.Join(cells, "") + "]"
}

func spread(left, right string, width int) string {
	return left + strings.Repeat(" ", max(1, width-ansi.StringWidth(cleanLine(left))-ansi.StringWidth(cleanLine(right)))) + right
}

func adjustedRemaining(pos, total time.Duration, speed float64) (time.Duration, bool) {
	if total <= 0 || speed <= 0 || math.IsNaN(speed) || math.IsInf(speed, 0) {
		return 0, false
	}
	rem := total - min(total, max(0, pos))
	seconds := float64(rem) / float64(time.Second) / speed
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func (m Model) playerLines(size Rect) []string {
	p := m.data.Player
	state := playbackLabel(p.State)
	if p.Key == "" {
		return m.frame(PlayerPane, size, "Player - No current audio", []string{"No current audio", "Position/duration unavailable", "Transport disabled: no loaded audio"}, -1)
	}
	chapter := p.Chapter
	if strings.TrimSpace(cleanLine(chapter)) == "" {
		chapter = "No chapter metadata"
	}
	rows := []string{title(p.Title) + " | " + chapter, m.timeline(size.Width - 4)}
	controls := []string{"Resume", "-15s", "+15s"}
	if p.State == Playing {
		controls[0] = "Pause"
	}
	controls[m.control] = "[" + controls[m.control] + "]"
	transport := strings.Join(controls, "  ")
	if !p.transportAvailable() || m.dispatch == nil {
		transport = "Transport disabled"
	}
	speed := "Speed unknown"
	if p.Speed > 0 && !math.IsInf(p.Speed, 0) {
		speed = fmt.Sprintf("%.1fx", p.Speed)
	}
	volume := "Volume unknown"
	if p.VolumeKnown {
		volume = fmt.Sprintf("Vol %d%%", min(100, max(0, p.Volume)))
	}
	sleep := p.Sleep
	if sleep == "" {
		sleep = "Off"
	}
	rows = append(rows, spread(transport, speed+" | "+volume+" | Sleep "+sleep, size.Width-4))
	if size.Height == 6 {
		remaining := "Remaining unknown"
		if bookRem, ok := adjustedRemaining(p.Position, p.Duration, p.Speed); ok {
			remaining = "Remaining " + clock(bookRem) + " at speed"
		}
		if strings.TrimSpace(cleanLine(p.Chapter)) != "" {
			if chRem, ok := adjustedRemaining(p.Position, p.ChapterEnd, p.Speed); ok {
				remaining += " | Chapter left " + clock(chRem)
			} else {
				remaining += " | Chapter left unknown"
			}
		}
		rows = append(rows, remaining)
	}
	return m.frame(PlayerPane, size, "Player - "+state, rows, -1)
}
