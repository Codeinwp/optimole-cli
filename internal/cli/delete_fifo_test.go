//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDeleteInterruptedOpeningAFIFO: opening a FIFO waits for a writer;
// Ctrl-C must not wait with it.
func TestDeleteInterruptedOpeningAFIFO(t *testing.T) {
	h := newHarness(t).withKey()
	fifo := filepath.Join(t.TempDir(), "keys")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	// A writer at the end releases the open the command left behind.
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan int, 1)
	go func() { done <- h.runCtx(ctx, "delete", "--keys", fifo, "--yes") }()
	select {
	case code := <-done:
		if code != ExitInterrupted || !strings.Contains(h.stderr.String(), "interrupted; nothing was deleted") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening the keys FIFO ignored Ctrl-C")
	}
}
