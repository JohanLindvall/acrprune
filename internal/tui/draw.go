// Package tui provides the drawing and terminal lifecycle shared by the batch
// dashboard and the statistics explorer.
package tui

import (
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

// Theme uses terminal defaults without color, retaining emphasis and selection.
type Theme struct {
	Base, Muted, Accent, Good, Warn, Danger, Panel, Selected, Border tcell.Style
}

func NewTheme(color bool) Theme {
	b := tcell.StyleDefault
	if !color {
		return Theme{b, b, b.Bold(true), b, b.Bold(true), b.Bold(true), b, b.Reverse(true), b}
	}
	b = b.Foreground(tcell.NewHexColor(0xdce4ee)).Background(tcell.NewHexColor(0x111923))
	return Theme{
		Base: b, Muted: b.Foreground(tcell.NewHexColor(0x98a9bc)),
		Accent:   b.Foreground(tcell.NewHexColor(0x62d6e8)).Bold(true),
		Good:     b.Foreground(tcell.NewHexColor(0x87d8a0)),
		Warn:     b.Foreground(tcell.NewHexColor(0xf1c474)),
		Danger:   b.Foreground(tcell.NewHexColor(0xff8b91)).Bold(true),
		Panel:    b.Background(tcell.NewHexColor(0x1b2938)),
		Selected: b.Background(tcell.NewHexColor(0x254459)).Bold(true),
		Border:   b.Foreground(tcell.NewHexColor(0x40556c)),
	}
}

// OnPanel preserves the panel background underneath colored text.
func (t Theme) OnPanel(style tcell.Style) tcell.Style {
	_, background, _ := t.Panel.Decompose()
	return style.Background(background)
}

// Clean prevents control and bidi formatting characters from changing layout.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		// Joiners are part of emoji graphemes and scripts such as Persian.
		// Preserve them without admitting bidi overrides or other controls.
		if !unicode.IsGraphic(r) && r != '\u200c' && r != '\u200d' {
			return ' '
		}
		return r
	}, s)
}

// Wrap splits sanitized text at word or grapheme boundaries. It examines only
// the next line's worth of text, so long paragraphs take linear time. A
// grapheme wider than width occupies a line by itself; Text clips it on draw.
func Wrap(text string, width int) []string {
	text = Clean(text)
	width = max(1, width)
	var lines []string
	for text != "" {
		g := uniseg.NewGraphemes(text)
		end, space, cells := 0, 0, 0
		for g.Next() {
			if cells+g.Width() > width {
				if g.Str() == " " {
					space = end // a word ending exactly at the margin fits
				}
				if end == 0 {
					_, end = g.Positions()
				}
				break
			}
			cells += g.Width()
			_, end = g.Positions()
			if g.Str() == " " {
				space = end
			}
		}
		if end == len(text) {
			lines = append(lines, text)
			break
		}
		if space > 0 {
			end = space
		}
		lines = append(lines, strings.TrimRight(text[:end], " "))
		text = strings.TrimLeft(text[end:], " ")
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// Text writes a single line, clipping at grapheme boundaries and reserving an
// ellipsis when truncated. All coordinates are clipped to the screen.
func Text(screen tcell.Screen, x, y, width int, style tcell.Style, text string) {
	w, h := screen.Size()
	if x < 0 || y < 0 || y >= h || width < 1 || x >= w {
		return
	}
	end := min(x+width, w)
	text = Clean(text)
	truncated := uniseg.StringWidth(text) > end-x
	if truncated {
		end--
	}
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		cw := graphemes.Width()
		if cw == 0 {
			continue
		}
		if x+cw > end {
			break
		}
		runes := graphemes.Runes()
		screen.SetContent(x, y, runes[0], runes[1:], style)
		x += cw
	}
	if truncated {
		screen.SetContent(x, y, '…', nil, style)
	}
}

func Fill(screen tcell.Screen, x, y, width, height int, style tcell.Style) {
	w, h := screen.Size()
	for row := max(y, 0); row < min(y+height, h); row++ {
		for col := max(x, 0); col < min(x+width, w); col++ {
			screen.SetContent(col, row, ' ', nil, style)
		}
	}
}

// Rule draws a divider with an optional inset label.
func Rule(screen tcell.Screen, x, y, width int, theme Theme, title string) {
	Text(screen, x, y, width, theme.Border, strings.Repeat("─", max(0, width)))
	if title != "" {
		Text(screen, x+1, y, width-2, theme.Accent, " "+title+" ")
	}
}

func Meter(screen tcell.Screen, x, y, width int, fraction float64, fill, empty tcell.Style) {
	filled := int(float64(width) * max(0, min(fraction, 1)))
	Text(screen, x, y, filled, fill, strings.Repeat("━", max(0, filled)))
	Text(screen, x+filled, y, width-filled, empty, strings.Repeat("─", max(0, width-filled)))
}

// Dialog replaces the background with a centered, word-wrapped panel.
func Dialog(screen tcell.Screen, theme Theme, title string, lines []string) {
	w, h := screen.Size()
	width := max(0, min(w-2, 100))
	inner := max(1, width-4)
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, Wrap(line, inner)...)
	}
	height := min(h, len(wrapped)+4)
	x, y := (w-width)/2, (h-height)/2
	Fill(screen, x, y, width, height, theme.Base)
	Rule(screen, x, y, width, theme, title)
	for i, line := range wrapped {
		if i >= height-3 {
			break
		}
		Text(screen, x+2, y+1+i, inner, theme.Base, line)
	}
	Text(screen, x+2, y+height-1, inner, theme.Muted, "Esc close")
}
