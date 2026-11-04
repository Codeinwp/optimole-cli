package export

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/api"
)

const (
	partSuffix = ".part"
	// maxResumes bounds the attempts of one archive even while each attempt
	// makes progress.
	maxResumes = 50
	maxBackoff = 30 * time.Second
)

// stallTimeout aborts an attempt whose body stops arriving (tests shorten it).
var stallTimeout = 2 * time.Minute

// errNeedURL means storage refused the URL (it expired) or the archive is not
// the size the export listed: a fresh look at the export gives the current
// URL and size.
var errNeedURL = errors.New("the download URL was refused or the archive changed")

// errStalled is an attempt aborted because no bytes arrived for stallTimeout.
var errStalled = errors.New("the download stalled")

// writeError is a failure writing to the local disk; retrying will not help.
type writeError struct{ err error }

func (e *writeError) Error() string { return e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }

// job is one archive to download.
type job struct {
	path string
	url  string
	// size is the size the export listed, or -1 when it gave none.
	size int64
}

// jobResult is how a job ended.
type jobResult struct {
	// present: the archive was already on disk, nothing was downloaded.
	present bool
	// written counts the bytes written to disk, failed attempts included.
	written int64
	// size is the archive's size once it is in place.
	size int64
	err  error
}

// onDisk reports whether the archive is already in place: a file of the
// listed size (only checked archives are renamed into place).
func onDisk(j job) bool {
	if j.size < 0 {
		return false
	}
	st, err := os.Stat(j.path)
	return err == nil && st.Mode().IsRegular() && st.Size() == j.size
}

// download puts one archive at j.path. The bytes go to a ".part" file named
// after the expected size, so a later run resumes only an archive of the same
// size; the file is renamed into place once its size matches and it opens as
// a ZIP. A broken ZIP is downloaded once more from scratch.
func (r *runner) download(ctx context.Context, j job) jobResult {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return jobResult{err: &writeError{err}}
	}
	tmp := tempPath(j)
	removeStaleTemps(j.path, tmp)

	var res jobResult
	for round := 0; round < 2; round++ {
		if err := r.fetch(ctx, j, tmp, &res); err != nil {
			res.err = err
			return res
		}
		size, err := checkArchive(tmp, j.size)
		if err == nil {
			if err := os.Rename(tmp, j.path); err != nil {
				res.err = &writeError{err}
				return res
			}
			return jobResult{written: res.written, size: size}
		}
		os.Remove(tmp)
		res.err = err
	}
	return res
}

// tempPath names the partial file after the size it should reach.
func tempPath(j job) string {
	if j.size < 0 {
		return j.path + partSuffix
	}
	return j.path + "." + strconv.FormatInt(j.size, 10) + partSuffix
}

// removeStaleTemps deletes partial files an earlier run left for another
// version (size) of the archive.
func removeStaleTemps(final, keep string) {
	dir, base := filepath.Split(final)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, base+".") && strings.HasSuffix(name, partSuffix) && filepath.Join(dir, name) != keep {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// fetch completes tmp from storage, resuming from its current size. Network
// failures, 5xx answers, stalls and early ends are retried with backoff; the
// count restarts whenever an attempt makes progress.
func (r *runner) fetch(ctx context.Context, j job, tmp string, res *jobResult) error {
	failures := 0
	for attempt := 1; ; attempt++ {
		n, err := r.fetchOnce(ctx, j, tmp)
		res.written += n
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) || attempt >= maxResumes {
			return err
		}
		if n > 0 {
			failures = 0
		}
		failures++
		if failures >= api.MaxAttempts {
			return err
		}
		if err := r.sleep(ctx, backoff(failures)); err != nil {
			return err
		}
	}
}

// fetchOnce makes one GET and appends the body to tmp. It returns the bytes
// written.
func (r *runner) fetchOnce(ctx context.Context, j job, tmp string) (int64, error) {
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, &writeError{err}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, &writeError{err}
	}
	offset := st.Size()
	if j.size >= 0 && offset >= j.size {
		if offset == j.size {
			return 0, nil
		}
		offset = 0
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	obj, err := r.client.GetObject(attemptCtx, j.url, offset)
	if err != nil {
		return 0, classifyStorageError(err, f)
	}
	defer obj.Body.Close()
	if obj.Offset != offset {
		offset = 0
	}
	if j.size >= 0 && obj.Size >= 0 && obj.Size != j.size {
		return 0, fmt.Errorf("storage has %d bytes, the export listed %d: %w", obj.Size, j.size, errNeedURL)
	}
	if err := f.Truncate(offset); err != nil {
		return 0, &writeError{err}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, &writeError{err}
	}
	want := j.size
	if want < 0 {
		want = obj.Size
	}

	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	var src io.Reader = &stallReader{r: obj.Body, timer: stall}
	if want >= 0 {
		// One byte more than expected shows an archive that grew.
		src = io.LimitReader(src, want-offset+1)
	}
	w := &fileWriter{f: f}
	n, err := io.Copy(w, src)
	switch {
	case w.err != nil:
		return n, &writeError{w.err}
	case err != nil && ctx.Err() == nil && attemptCtx.Err() != nil:
		return n, errStalled
	case err != nil:
		return n, fmt.Errorf("%s: %w", api.OpDownloadArchive, err)
	}
	total := offset + n
	switch {
	case want >= 0 && total > want:
		return n, fmt.Errorf("storage sent more than the listed %d bytes: %w", want, errNeedURL)
	case want >= 0 && total < want:
		return n, fmt.Errorf("%s: the archive ended after %d of %d bytes", api.OpDownloadArchive, total, want)
	}
	if err := f.Sync(); err != nil {
		return n, &writeError{err}
	}
	return n, nil
}

// classifyStorageError maps a failed GET: 403 means the URL expired, 416 that
// the partial file is longer than the archive (it is emptied for the retry).
func classifyStorageError(err error, f *os.File) error {
	var se *api.StorageError
	if !errors.As(err, &se) {
		return err
	}
	switch se.StatusCode {
	case http.StatusForbidden:
		return fmt.Errorf("%v: %w", err, errNeedURL)
	case http.StatusRequestedRangeNotSatisfiable:
		if terr := f.Truncate(0); terr != nil {
			return &writeError{terr}
		}
	}
	return err
}

// retryable reports whether another attempt may succeed.
func retryable(err error) bool {
	var we *writeError
	if errors.As(err, &we) || errors.Is(err, errNeedURL) {
		return false
	}
	var se *api.StorageError
	if errors.As(err, &se) && se.StatusCode != 0 {
		return se.StatusCode >= 500 || se.StatusCode == http.StatusRequestedRangeNotSatisfiable || se.StatusCode == http.StatusTooManyRequests
	}
	return true
}

// checkArchive checks the downloaded file's size and that it opens as a ZIP
// (its central directory reads), and returns the size.
func checkArchive(path string, want int64) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, &writeError{err}
	}
	if want >= 0 && st.Size() != want {
		return 0, fmt.Errorf("the archive has %d bytes, the export listed %d", st.Size(), want)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return 0, fmt.Errorf("the downloaded archive is not a valid ZIP: %w", err)
	}
	zr.Close()
	return st.Size(), nil
}

func backoff(failures int) time.Duration {
	d := time.Second << (failures - 1)
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// stallReader pushes back the stall timer whenever bytes arrive.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallTimeout)
	}
	return n, err
}

// fileWriter remembers a write error, so it is not mistaken for a network one.
type fileWriter struct {
	f   *os.File
	err error
}

func (w *fileWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}
