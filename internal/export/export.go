// Package export follows an asset export on the dashboard and downloads its
// ZIP archives into a local folder, while the export still runs. A later run
// into the same folder skips the archives already there and resumes the ones
// left half-done.
package export

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/api"
)

// Concurrency limits: archives are up to about 1 GB each.
const (
	DefaultConcurrency = 2
	MaxConcurrency     = 8
)

// maxURLRefreshes is how many fresh URLs one archive gets after storage
// refused one.
const maxURLRefreshes = 5

// Options configures a Run.
type Options struct {
	Client   *api.Client
	ExportID int64
	// Dir is the folder the archives go into: one subfolder per domain.
	Dir string
	// Domains limits the download to these domains (case-insensitive); all
	// when empty.
	Domains     []string
	Concurrency int
	// Sleep waits between looks at the export and between retries. Nil means
	// a timer that stops early when the context is done.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnEvent receives every event, from one goroutine.
	OnEvent func(Event)
}

// EventKind says what an Event reports.
type EventKind int

// The events of a Run.
const (
	// ExportStatus: the export's status, when first seen and on each change.
	ExportStatus EventKind = iota
	// DomainStatus: a selected domain's status, when first seen and on each
	// change, with the number of archives listed so far.
	DomainStatus
	// PartStarted: an archive download started.
	PartStarted
	// PartDone: an archive was downloaded, found already on disk, or failed.
	PartDone
)

// PartResult is how an archive ended.
type PartResult string

// Archive results.
const (
	Downloaded PartResult = "downloaded"
	Present    PartResult = "present"
	Failed     PartResult = "failed"
)

// Event is one step of a Run.
type Event struct {
	Kind EventKind
	// Status is the export's or the domain's status.
	Status string
	Domain string
	// Parts is the number of archives a domain lists (DomainStatus).
	Parts int
	Part  int64
	// Path is the archive's final path, under Dir.
	Path string
	// Size is the archive's size, or -1 when unknown.
	Size    int64
	Result  PartResult
	Err     error
	Elapsed time.Duration
}

// Outcome is how a Run ended.
type Outcome struct {
	// Export is the server's last answer, nil if none arrived.
	Export *api.Export
	// Folders maps each domain to its subfolder of Dir.
	Folders map[string]string
	// Counts of the selected archives the export listed.
	Downloaded, Present, Failed int
	// Bytes counts what this run wrote to disk.
	Bytes int64
	// Err says why the run stopped before the export finished and every
	// archive was tried: a failed look at the export, an *UnknownDomainError,
	// another command downloading into Dir, the context, or the manifest that
	// could not be written.
	Err error
}

// UnknownDomainError is a requested domain the export does not cover.
type UnknownDomainError struct {
	Domain string
	Known  []string
}

func (e *UnknownDomainError) Error() string {
	return fmt.Sprintf("export has no domain %q (it has: %s)", e.Domain, strings.Join(e.Known, ", "))
}

type partState int

const (
	pending partState = iota
	active
	done
	needURL
	failed
)

type partKey struct {
	domain string
	number int64
}

// part is the local state of one listed archive.
type part struct {
	key  partKey
	path string
	// size and url are the latest the export listed (size -1: unknown).
	size      int64
	url       string
	state     partState
	result    PartResult
	doneSize  int64
	refreshes int
}

// finished is a download goroutine's report.
type finished struct {
	part    *part
	res     jobResult
	elapsed time.Duration
}

type runner struct {
	client  *api.Client
	opts    Options
	sleep   func(context.Context, time.Duration) error
	exp     *api.Export
	folders folderMap
	// wanted is the set of selected domains (nil: all).
	wanted       map[string]bool
	domainStatus map[string]string
	parts        map[partKey]*part
	queue        []*part
	bytes        int64
	// unlock releases the lock on Dir, held from the first look at the
	// export on.
	unlock func()
}

// Run follows the export until it is final and every selected archive it
// lists is downloaded or failed, or until the context ends. Archives are
// downloaded as soon as the export lists them. An archive whose listed size
// changes after it was downloaded is downloaded again.
func Run(ctx context.Context, o Options) Outcome {
	if o.Concurrency < 1 {
		o.Concurrency = DefaultConcurrency
	}
	r := &runner{
		client:       o.Client,
		opts:         o,
		sleep:        o.Sleep,
		domainStatus: map[string]string{},
		parts:        map[partKey]*part{},
	}
	if r.sleep == nil {
		r.sleep = sleepContext
	}
	out := r.loop(ctx)
	out.Export = r.exp
	out.Folders = r.folders.names
	out.Bytes = r.bytes
	for _, p := range r.parts {
		switch {
		case p.state == failed:
			out.Failed++
		case p.state == done && p.result == Downloaded:
			out.Downloaded++
		case p.state == done:
			out.Present++
		}
	}
	if r.unlock != nil {
		if err := writeManifest(o.Dir, r.exp, r.folders.names, r.parts); err != nil && out.Err == nil {
			out.Err = err
		}
		r.unlock()
	}
	return out
}

// loop is the coordinator: it looks at the export, starts downloads and
// handles their results, all on one goroutine.
func (r *runner) loop(parent context.Context) Outcome {
	var out Outcome
	ctx, cancel := context.WithCancel(parent)
	started := time.Now()
	results := make(chan finished)
	pollDue := make(chan struct{}, 1)
	pollNow, pollScheduled := true, false
	running := 0
	defer func() {
		// Stop the downloads in flight, which keep their partial files for
		// the next run, and wait for them.
		cancel()
		for ; running > 0; running-- {
			r.finish(ctx, <-results)
		}
	}()

	for {
		if pollNow {
			pollNow = false
			exp, err := r.client.GetExport(ctx, r.opts.ExportID)
			if err == nil {
				err = r.apply(exp)
			}
			if err == nil && r.unlock == nil {
				r.unlock, err = lockDir(r.opts.Dir)
			}
			if err != nil {
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				out.Err = err
				return out
			}
		}
		for running < r.opts.Concurrency && len(r.queue) > 0 {
			p := r.queue[0]
			r.queue = r.queue[1:]
			if p.state != pending {
				continue
			}
			j := job{path: p.path, url: p.url, size: p.size}
			if onDisk(j) {
				r.finish(ctx, finished{part: p, res: jobResult{present: true, size: j.size}})
				continue
			}
			p.state = active
			running++
			r.emit(Event{Kind: PartStarted, Domain: p.key.domain, Part: p.key.number, Path: p.path, Size: p.size})
			go func() {
				start := time.Now()
				res := r.download(ctx, j)
				results <- finished{part: p, res: res, elapsed: time.Since(start)}
			}()
		}
		final := api.IsFinalStatus(r.exp.Status)
		refreshing := r.count(needURL) > 0
		if final && running == 0 && len(r.queue) == 0 && !refreshing {
			return out
		}
		if (!final || refreshing) && !pollScheduled {
			pollScheduled = true
			wait := pollInterval(time.Since(started))
			if final {
				wait = 0
			}
			go func() {
				r.sleep(ctx, wait)
				pollDue <- struct{}{}
			}()
		}
		select {
		case f := <-results:
			running--
			r.finish(ctx, f)
		case <-pollDue:
			pollScheduled, pollNow = false, true
		case <-ctx.Done():
			out.Err = ctx.Err()
			return out
		}
	}
}

// apply takes in a fresh look at the export: status events, new archives to
// download, fresh URLs, and archives whose size changed.
func (r *runner) apply(exp *api.Export) error {
	first := r.exp == nil
	statusChanged := first || exp.Status != r.exp.Status
	r.exp = exp
	names := make([]string, 0, len(exp.Domains))
	for _, d := range exp.Domains {
		names = append(names, d.Domain)
	}
	r.folders.add(names)
	if first {
		if err := r.selectDomains(names); err != nil {
			return err
		}
	}

	listed := map[partKey]bool{}
	for _, d := range exp.Domains {
		if r.wanted != nil && !r.wanted[d.Domain] {
			continue
		}
		parts := 0
		for _, lp := range d.Parts {
			if !lp.Number.Valid || lp.Number.Value <= 0 || lp.URL == "" {
				continue
			}
			parts++
			key := partKey{d.Domain, lp.Number.Value}
			listed[key] = true
			size := int64(-1)
			if lp.Size.Valid && lp.Size.Value >= 0 {
				size = lp.Size.Value
			}
			r.update(key, lp.URL, size)
		}
		if prev, seen := r.domainStatus[d.Domain]; !seen || prev != d.Status {
			r.domainStatus[d.Domain] = d.Status
			r.emit(Event{Kind: DomainStatus, Domain: d.Domain, Status: d.Status, Parts: parts})
		}
	}
	if api.IsFinalStatus(exp.Status) {
		// A final export lists everything it will: an archive waiting for a
		// URL the export no longer lists would wait forever.
		for key, p := range r.parts {
			if p.state == needURL && !listed[key] {
				r.fail(p, errors.New("the export no longer lists this archive"))
			}
		}
	}
	// After the domains, so a final status comes last.
	if statusChanged {
		r.emit(Event{Kind: ExportStatus, Status: exp.Status})
	}
	return nil
}

// update records one listed archive and queues it when it needs downloading.
func (r *runner) update(key partKey, url string, size int64) {
	p := r.parts[key]
	if p == nil {
		p = &part{
			key:  key,
			path: filepath.Join(r.opts.Dir, r.folders.names[key.domain], fmt.Sprintf("images-%d.zip", key.number)),
			size: size,
			url:  url,
		}
		r.parts[key] = p
		r.queue = append(r.queue, p)
		return
	}
	p.url, p.size = url, size
	switch {
	case p.state == needURL:
		p.state = pending
		r.queue = append(r.queue, p)
	case p.state == done && size >= 0 && size != p.doneSize:
		p.state = pending
		r.queue = append(r.queue, p)
	}
}

// selectDomains resolves the requested domains against the export's.
func (r *runner) selectDomains(names []string) error {
	if len(r.opts.Domains) == 0 {
		return nil
	}
	r.wanted = map[string]bool{}
	for _, want := range r.opts.Domains {
		found := false
		for _, name := range names {
			if strings.EqualFold(name, want) {
				r.wanted[name] = true
				found = true
			}
		}
		if !found {
			known := append([]string(nil), names...)
			sort.Strings(known)
			return &UnknownDomainError{Domain: want, Known: known}
		}
	}
	return nil
}

// finish handles a download's report.
func (r *runner) finish(ctx context.Context, f finished) {
	p, res := f.part, f.res
	r.bytes += res.written
	switch {
	case res.err == nil:
		p.doneSize = res.size
		p.result = Downloaded
		if res.present {
			p.result = Present
		}
		if p.size >= 0 && p.size != p.doneSize {
			// The export listed a new size while this one downloaded.
			p.state = pending
			r.queue = append(r.queue, p)
			return
		}
		p.state = done
		r.emit(Event{Kind: PartDone, Domain: p.key.domain, Part: p.key.number, Path: p.path, Size: p.doneSize, Result: p.result, Elapsed: f.elapsed})
	case ctx.Err() != nil:
		// Interrupted: neither done nor failed; the next run resumes it.
		p.state = pending
	case errors.Is(res.err, errNeedURL) && p.refreshes < maxURLRefreshes:
		p.refreshes++
		p.state = needURL
	default:
		r.fail(p, res.err)
	}
}

func (r *runner) fail(p *part, err error) {
	p.state = failed
	p.result = Failed
	r.emit(Event{Kind: PartDone, Domain: p.key.domain, Part: p.key.number, Path: p.path, Size: p.size, Result: Failed, Err: err})
}

func (r *runner) count(s partState) int {
	n := 0
	for _, p := range r.parts {
		if p.state == s {
			n++
		}
	}
	return n
}

func (r *runner) emit(e Event) {
	if r.opts.OnEvent != nil {
		r.opts.OnEvent(e)
	}
}

// pollInterval spaces the looks at a running export: every 5 seconds at
// first, like the dashboard, then less often for long exports.
func pollInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 2*time.Minute:
		return 5 * time.Second
	case elapsed < 10*time.Minute:
		return 15 * time.Second
	}
	return 30 * time.Second
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
