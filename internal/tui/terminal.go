package tui

import (
	"sync"

	"github.com/gdamore/tcell/v2"
)

// All displays share terminal ownership so a forced exit can restore either
// interface, without racing a draw or finalizing the same screen twice.
var displays struct {
	sync.Mutex
	screens map[tcell.Screen]bool
}

func Own(screen tcell.Screen) {
	displays.Lock()
	defer displays.Unlock()
	if displays.screens == nil {
		displays.screens = map[tcell.Screen]bool{}
	}
	displays.screens[screen] = true
}

func Disown(screen tcell.Screen) {
	displays.Lock()
	defer displays.Unlock()
	if displays.screens[screen] {
		delete(displays.screens, screen)
		screen.Fini()
	}
}

func OnScreen(screen tcell.Screen, fn func()) {
	displays.Lock()
	defer displays.Unlock()
	if displays.screens[screen] {
		fn()
	}
}

// RestoreTerminal is safe to call from any goroutine, any number of times.
// Once restored, displays draw nothing more.
func RestoreTerminal() {
	displays.Lock()
	defer displays.Unlock()
	for screen := range displays.screens {
		delete(displays.screens, screen)
		screen.Fini()
	}
}

// Release closes a screen whose Init failed. tcell's Fini can panic after
// such a failure, before it has engaged the terminal or created its channels.
func Release(screen tcell.Screen) {
	defer func() { _ = recover() }()
	screen.Fini()
}
