//go:build darwin || linux

package upload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

// TestRunFIFOSwappedInDoesNotHang: opening a FIFO for reading blocks until a
// writer appears; the upload must fail fast instead.
func TestRunFIFOSwappedInDoesNotHang(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, root, "a.jpg", []byte("abc"))
	plan, err := BuildPlan([]string{root}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "a.jpg")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	srv := testserver.New(t)
	var got Result
	r := Runner{Client: newClient(t, srv, testserver.DefaultKey), OnResult: func(res Result) { got = res }}
	done := make(chan struct{})
	go func() {
		r.Run(context.Background(), plan.Selected())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Unblock the stuck open before failing.
		if w, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		<-done
		t.Fatal("opening the swapped-in FIFO blocked")
	}
	if got.Status != StatusFailed || !strings.Contains(got.Err.Error(), "no longer a regular file") || srv.Count(testserver.RouteCreate) != 0 {
		t.Fatalf("result = %+v", got)
	}
}
