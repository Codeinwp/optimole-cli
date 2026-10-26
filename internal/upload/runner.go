package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Codeinwp/optimole-cli/internal/api"
)

// Concurrency bounds.
const (
	DefaultConcurrency = 4
	MaxConcurrency     = 16
)

// ErrInterrupted marks files stopped by cancellation (Ctrl-C).
var ErrInterrupted = errors.New("interrupted")

// Client is what the runner needs from the API.
type Client interface {
	CreateAsset(ctx context.Context, req api.CreateAssetRequest) (*api.CreateAssetResponse, error)
	PutObject(ctx context.Context, target api.UploadTarget, r io.ReaderAt, size int64) error
	CompleteAsset(ctx context.Context, key string, dims *api.Dimensions) (*api.Asset, error)
}

// Result is the outcome of one file.
type Result struct {
	Item   Item
	Status Status
	Key    string
	URL    string
	// FolderID is the folder the asset landed in, when the server said.
	FolderID int64
	Err      error
	// NotAttempted marks files never sent because the run stopped early
	// (Ctrl-C, an invalid key, or the account's upload limit).
	NotAttempted bool
}

// Runner uploads planned files.
type Runner struct {
	Client Client
	// Concurrency is the number of files in flight (1-16, default 4).
	Concurrency   int
	CreateFolders bool
	// OnResult receives every result, one at a time, from the goroutine
	// that called Run.
	OnResult func(Result)
}

// Outcome summarizes a run.
type Outcome struct {
	Uploaded     int
	Failed       int
	NotAttempted int
	Interrupted  bool
	// Stopped is the error that ended the run early, if any.
	Stopped error
}

// task is one file to send. The first file of each destination is its leader.
type task struct {
	index    int
	item     Item
	group    *group
	leader   bool
	folderID int64
}

// group holds the files bound for one destination. Only its leader runs
// until the leader finishes; that keeps parallel requests from racing to
// create the same folder, and lets the rest send the folder ID.
type group struct {
	waiting []task
}

type finished struct {
	task   task
	result Result
}

// Run uploads the items and reports each result. Leaders of different
// destinations run in parallel (except, with CreateFolders, leaders that could
// create the same missing parent folder); within a destination, the rest start
// once the leader has finished.
//
// After a leader finishes:
//   - if the server placed it in a folder (success, or a failure after the
//     asset was created), the rest run in parallel with that folder ID;
//   - if the server refused the destination (invalid folder, or a 403
//     without upload_limit_reached), the rest fail with the same error,
//     without more requests;
//   - otherwise the next file becomes the leader.
//
// A 401 or upload_limit_reached stops the whole run; files not yet sent are
// reported as not attempted. Cancelling ctx does the same.
func (r *Runner) Run(ctx context.Context, items []Item) Outcome {
	conc := r.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	conc = min(conc, MaxConcurrency)

	var out Outcome
	reported := make([]bool, len(items))
	emit := func(index int, res Result) {
		reported[index] = true
		switch {
		case res.Status == StatusUploaded:
			out.Uploaded++
		case res.NotAttempted:
			out.NotAttempted++
			out.Failed++
		default:
			out.Failed++
		}
		if r.OnResult != nil {
			r.OnResult(res)
		}
	}

	groups := map[string]*group{}
	var ready []task
	for i, it := range items {
		key := it.Dest.key()
		if g, ok := groups[key]; ok {
			g.waiting = append(g.waiting, task{index: i, item: it, group: g})
			continue
		}
		g := &group{}
		groups[key] = g
		ready = append(ready, task{index: i, item: it, group: g, leader: true})
	}

	gate := newFolderGate(r.CreateFolders)
	done := make(chan finished)
	inflight := 0
	for {
		for out.Stopped == nil && inflight < conc {
			if err := ctx.Err(); err != nil {
				out.Stopped = err
				break
			}
			i := gate.next(ready)
			if i < 0 {
				break
			}
			t := ready[i]
			ready = append(ready[:i], ready[i+1:]...)
			gate.start(t)
			inflight++
			go func(t task) {
				done <- finished{task: t, result: r.uploadOne(ctx, t)}
			}(t)
		}
		if inflight == 0 {
			break
		}
		f := <-done
		inflight--
		gate.finish(f.task, f.result)
		emit(f.task.index, f.result)
		if out.Stopped == nil {
			if err := ctx.Err(); err != nil {
				out.Stopped = err
			} else if fatal(f.result.Err) {
				out.Stopped = f.result.Err
			}
		}
		if !f.task.leader {
			continue
		}
		g := f.task.group
		switch {
		case f.result.Status == StatusUploaded || f.result.FolderID > 0:
			for _, w := range g.waiting {
				w.folderID = f.result.FolderID
				ready = append(ready, w)
			}
			g.waiting = nil
		case destinationRefused(f.result.Err):
			for _, w := range g.waiting {
				emit(w.index, Result{Item: w.item, Status: StatusFailed, Err: f.result.Err})
			}
			g.waiting = nil
		case len(g.waiting) > 0:
			next := g.waiting[0]
			next.leader = true
			g.waiting = g.waiting[1:]
			ready = append(ready, next)
		}
	}

	if out.Stopped != nil {
		out.Interrupted = errors.Is(out.Stopped, context.Canceled) || errors.Is(out.Stopped, context.DeadlineExceeded)
		reason := fmt.Errorf("not attempted: %w", out.Stopped)
		if out.Interrupted {
			reason = fmt.Errorf("not started: %w", ErrInterrupted)
		}
		for i, it := range items {
			if !reported[i] {
				emit(i, Result{Item: it, Status: StatusFailed, Err: reason, NotAttempted: true})
			}
		}
	}
	return out
}

// folderGate keeps leaders that could create the same missing folder apart.
// Leaders of "A/x" and "A/y" both create "A" when it is missing, so they run
// one after the other until "A" is known to exist; leaders whose paths share
// no folder, or only folders known to exist, run in parallel. It only acts
// when folders may be created.
type folderGate struct {
	enabled bool
	known   map[string]bool     // lower-cased paths known to exist
	running map[*group][]string // lower-cased segments of in-flight leaders
}

func newFolderGate(enabled bool) *folderGate {
	return &folderGate{enabled: enabled, known: map[string]bool{}, running: map[*group][]string{}}
}

func segments(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(strings.ToLower(p), "/")
}

// next returns the index of the first task that may start, or -1.
func (g *folderGate) next(ready []task) int {
	for i, t := range ready {
		if !g.blocked(t) {
			return i
		}
	}
	return -1
}

func (g *folderGate) blocked(t task) bool {
	if !g.enabled || !t.leader || t.item.Dest.ID > 0 || t.item.Dest.Path == "" {
		return false
	}
	segs := segments(t.item.Dest.Path)
	for _, other := range g.running {
		n := 0
		for n < len(segs) && n < len(other) && segs[n] == other[n] {
			n++
		}
		if n > 0 && !g.known[strings.Join(segs[:n], "/")] {
			return true
		}
	}
	return false
}

func (g *folderGate) start(t task) {
	if g.enabled && t.leader && t.item.Dest.ID == 0 && t.item.Dest.Path != "" {
		g.running[t.group] = segments(t.item.Dest.Path)
	}
}

func (g *folderGate) finish(t task, res Result) {
	if !t.leader {
		return
	}
	delete(g.running, t.group)
	if t.item.Dest.ID == 0 && (res.Status == StatusUploaded || res.FolderID > 0) {
		segs := segments(t.item.Dest.Path)
		for i := range segs {
			g.known[strings.Join(segs[:i+1], "/")] = true
		}
	}
}

// fatal reports errors that make every other upload fail too.
func fatal(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusUnauthorized || apiErr.Code == api.CodeUploadLimitReached
}

// destinationRefused reports errors about the destination rather than the
// file: every other file bound there would get the same answer.
func destinationRefused(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Op != api.OpCreateAsset {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusUnprocessableEntity:
		return apiErr.HasField("folder_path") || apiErr.HasField("folder_id")
	case http.StatusForbidden:
		return apiErr.Code != api.CodeUploadLimitReached
	}
	return false
}

// uploadOne runs the three steps for one file. The PUT itself retries the
// same URL on network errors and 5xx answers. Only when storage refuses the
// URL (403) or it expires is a new asset reserved, once; if an earlier PUT
// may have stored the bytes, completing the first asset is tried before that,
// since a second reservation would publish a duplicate.
func (r *Runner) uploadOne(ctx context.Context, t task) Result {
	res := Result{Item: t.item, Status: StatusFailed}
	f, size, err := openForUpload(t.item)
	if err != nil {
		res.Err = err
		return res
	}
	defer f.Close()
	name := filepath.Base(t.item.Path)
	dims := imageDimensions(f, size, name)
	folderID := t.folderID

	for reservation := 1; reservation <= 2; reservation++ {
		key, url, err := r.attempt(ctx, t.item, f, size, dims, &folderID)
		res.Key = key
		if folderID > 0 {
			res.FolderID = folderID
		}
		if err == nil {
			res.Status, res.URL, res.Err = StatusUploaded, url, nil
			return res
		}
		res.Err = err
		if ctx.Err() != nil {
			res.Err = ErrInterrupted
			return res
		}
		var se *api.StorageError
		if !errors.As(err, &se) || !se.NeedsNewAsset() || reservation == 2 {
			return res
		}
		if se.Uncertain {
			asset, cerr := r.Client.CompleteAsset(ctx, key, dims)
			if cerr == nil {
				res.Status, res.URL, res.Err = StatusUploaded, asset.URL, nil
				return res
			}
			if !isUploadNotFound(cerr) {
				// Unknown whether the bytes landed: a new asset could be a duplicate.
				res.Err = fmt.Errorf("%w; checking whether it was stored anyway: %v", err, cerr)
				return res
			}
		}
	}
	return res
}

func isUploadNotFound(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict && apiErr.Code == api.CodeUploadNotFound
}

// openForUpload opens a planned file and checks it still has the planned
// size.
func openForUpload(it Item) (*os.File, int64, error) {
	f, err := os.Open(it.Path)
	if err != nil {
		return nil, 0, fmt.Errorf("open: %w", unwrapPathError(err))
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat: %w", unwrapPathError(err))
	}
	if info.Size() != it.Size {
		f.Close()
		return nil, 0, fmt.Errorf("file changed since it was listed (now %d bytes, was %d)", info.Size(), it.Size)
	}
	return f, info.Size(), nil
}

// attempt makes one create → PUT → complete pass. folderID, when set, is
// sent instead of the folder path, and is updated from the create answer.
func (r *Runner) attempt(ctx context.Context, it Item, f io.ReaderAt, size int64, dims *api.Dimensions, folderID *int64) (key, url string, err error) {
	req := api.CreateAssetRequest{Filename: filepath.Base(it.Path), Size: size}
	switch {
	case *folderID > 0:
		req.FolderID = *folderID
	case it.Dest.ID > 0:
		req.FolderID = it.Dest.ID
	case it.Dest.Path != "":
		req.FolderPath = it.Dest.Path
		req.CreateFolders = r.CreateFolders
	}
	created, err := r.Client.CreateAsset(ctx, req)
	if err != nil {
		return "", "", r.withHint(err, it)
	}
	if created.Asset.FolderID.Valid && created.Asset.FolderID.Value > 0 {
		*folderID = created.Asset.FolderID.Value
	}
	key = created.Asset.Key
	target, err := created.Target()
	if err != nil {
		return key, "", err
	}
	if err := r.Client.PutObject(ctx, target, f, size); err != nil {
		return key, "", err
	}
	asset, err := r.Client.CompleteAsset(ctx, key, dims)
	if err != nil {
		return key, "", err
	}
	return key, asset.URL, nil
}

// HintError adds advice to an error.
type HintError struct {
	Err  error
	Hint string
}

func (e *HintError) Error() string { return e.Err.Error() + "; " + e.Hint }

func (e *HintError) Unwrap() error { return e.Err }

func (r *Runner) withHint(err error, it Item) error {
	var apiErr *api.Error
	if r.CreateFolders || it.Dest.Path == "" || !errors.As(err, &apiErr) ||
		apiErr.StatusCode != http.StatusUnprocessableEntity || !apiErr.HasField("folder_path") {
		return err
	}
	return &HintError{Err: err, Hint: "if the folder does not exist yet, add --create-folders to create it (full-access keys only)"}
}
