package explore

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDiscoverRulesDoesNotBlockOnNamedPipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipe.json")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Keep a writer open so an accidental read blocks. Closing it on failure
	// releases the read, letting the test finish without leaking a goroutine.
	pipe, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pipe.Close() }()
	done := make(chan []RuleFile, 1)
	go func() { done <- DiscoverRules(dir) }()
	select {
	case files := <-done:
		for _, file := range files {
			if file.Source == path {
				if file.Err == nil || !strings.Contains(file.Err.Error(), "regular file") {
					t.Fatalf("named pipe was read: %+v", file)
				}
				return
			}
		}
		t.Fatal("unusable rule source missing from catalog")
	case <-time.After(time.Second):
		_ = pipe.Close()
		<-done
		t.Fatal("rule discovery blocked on a named pipe")
	}
}
