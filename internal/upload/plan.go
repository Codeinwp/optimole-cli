// Package upload decides which local files go into which library folder (the
// planner) and uploads them through the API (the runner).
package upload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Status is the outcome of one file.
type Status string

// File statuses.
const (
	StatusPlanned  Status = "planned"
	StatusUploaded Status = "uploaded"
	StatusSkipped  Status = "skipped"
	StatusFailed   Status = "failed"
)

// Destination is where a file lands in the library: a folder path, an
// explicit folder ID, or the library root when both are zero.
type Destination struct {
	Path string
	ID   int64
}

// String formats the destination for people.
func (d Destination) String() string {
	switch {
	case d.ID > 0:
		return "folder #" + strconv.FormatInt(d.ID, 10)
	case d.Path == "":
		return "(library root)"
	default:
		return d.Path
	}
}

// key identifies the destination the way the server matches folder names:
// case-insensitively.
func (d Destination) key() string {
	if d.ID > 0 {
		return "#" + strconv.FormatInt(d.ID, 10)
	}
	return "/" + strings.ToLower(d.Path)
}

// Item is one file the planner found.
type Item struct {
	// Path is the file to open.
	Path string
	// Size is the size seen while planning; the runner checks it again.
	Size int64
	Dest Destination
	// Status is StatusPlanned for files to upload, or StatusSkipped /
	// StatusFailed with Reason.
	Status Status
	Reason string
	// info is what planning saw (lstat), to check at upload time that the
	// path still names the same regular file.
	info fs.FileInfo
}

// PlanOptions controls the planner.
type PlanOptions struct {
	// Folder is the normalized base folder path ("" = root).
	Folder string
	// FolderID is the base folder ID; it excludes Folder.
	FolderID      int64
	Recursive     bool
	IncludeHidden bool
	// Excludes are path.Match patterns, tried against the path relative to
	// the argument root and against the base name.
	Excludes []string
}

// Plan is the planner's result.
type Plan struct {
	// Items holds planned, skipped and failed files in walk order.
	Items []Item
	// Ignored counts hidden and excluded entries, which are not listed.
	Ignored int
}

// Selected returns the files to upload.
func (p *Plan) Selected() []Item {
	var out []Item
	for _, it := range p.Items {
		if it.Status == StatusPlanned {
			out = append(out, it)
		}
	}
	return out
}

// UsageError is a planning error caused by the arguments; the CLI exits 2.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// BuildPlan maps the arguments (files and directories) to library folders.
//
// A file argument goes into the base folder. A directory argument needs
// Recursive: its contents go into the base folder and each subdirectory
// becomes a subfolder below it. Hidden entries (dot-prefixed) inside
// directories are ignored unless IncludeHidden; symbolic links are never
// followed and are reported as skipped; files the server would refuse
// (extension, size, name length, folder path) are reported as skipped.
//
// When ctx is cancelled it stops and returns the plan so far with ctx's
// error.
func BuildPlan(ctx context.Context, args []string, opts PlanOptions) (*Plan, error) {
	for _, pattern := range opts.Excludes {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, usageErrorf("invalid --exclude pattern %q: %v", pattern, err)
		}
	}
	b := &builder{ctx: ctx, opts: opts, plan: &Plan{}, ids: map[any]bool{}, byPath: map[string][]fs.FileInfo{}}
	for _, arg := range args {
		if err := ctx.Err(); err != nil {
			return b.plan, err
		}
		if err := b.addArg(arg); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return b.plan, ctxErr
			}
			return nil, err
		}
	}
	return b.plan, nil
}

type builder struct {
	ctx  context.Context
	opts PlanOptions
	plan *Plan
	// ids and byPath remember the files already planned, by identity.
	ids    map[any]bool
	byPath map[string][]fs.FileInfo
}

func (b *builder) addArg(arg string) error {
	// Without trailing separators, so that "link/" is seen as the symlink
	// it is rather than as the directory it points to.
	arg = filepath.Clean(arg)
	info, err := os.Lstat(arg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return usageErrorf("%s: no such file or directory", arg)
		}
		return usageErrorf("%s: %v", arg, unwrapPathError(err))
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		b.skip(arg, "symbolic link (not followed)")
	case info.IsDir():
		if !b.opts.Recursive {
			return usageErrorf("%s is a directory; use -r to upload its contents", arg)
		}
		return b.walk(arg)
	case info.Mode().IsRegular():
		name := filepath.Base(arg)
		if b.excluded(name, name) {
			b.plan.Ignored++
			return nil
		}
		return b.addFile(arg, info, "")
	default:
		b.skip(arg, "not a regular file")
	}
	return nil
}

func (b *builder) walk(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := b.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// A directory that cannot be read (or an entry that vanished).
			b.plan.Items = append(b.plan.Items, Item{Path: p, Status: StatusFailed, Reason: "cannot read: " + unwrapPathError(err).Error()})
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if (!b.opts.IncludeHidden && strings.HasPrefix(name, ".")) || b.excluded(rel, name) {
			b.plan.Ignored++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			b.skip(p, "symbolic link (not followed)")
			return nil
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			b.skip(p, "not a regular file")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			b.plan.Items = append(b.plan.Items, Item{Path: p, Status: StatusFailed, Reason: "cannot read: " + unwrapPathError(err).Error()})
			return nil
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}
		return b.addFile(p, info, dir)
	})
}

// addFile plans one regular file; subdir is its directory relative to the
// argument root ("" for the root itself).
func (b *builder) addFile(p string, info fs.FileInfo, subdir string) error {
	if b.seenBefore(p, info) {
		return nil
	}
	size := info.Size()
	it := Item{Path: p, Size: size, Status: StatusPlanned, info: info}
	if err := CheckFilename(filepath.Base(p)); err != nil {
		it.Status, it.Reason = StatusSkipped, err.Error()
	} else if err := CheckSize(size); err != nil {
		it.Status, it.Reason = StatusSkipped, err.Error()
	}
	switch {
	case subdir == "" && b.opts.FolderID > 0:
		it.Dest = Destination{ID: b.opts.FolderID}
	case subdir == "":
		it.Dest = Destination{Path: b.opts.Folder}
	case b.opts.FolderID > 0:
		if it.Status == StatusPlanned {
			return usageErrorf("%s is in a subdirectory, and subfolders cannot be addressed under --folder-id; use --folder PATH instead", p)
		}
	default:
		joined := subdir
		if b.opts.Folder != "" {
			joined = b.opts.Folder + "/" + subdir
		}
		folder, err := NormalizeFolderPath(joined)
		if err != nil {
			folder = joined
			if it.Status == StatusPlanned {
				it.Status, it.Reason = StatusSkipped, err.Error()
			}
		}
		it.Dest = Destination{Path: folder}
	}
	if it.Status == StatusPlanned {
		pinIdentity(info)
	}
	b.plan.Items = append(b.plan.Items, it)
	return nil
}

// seenBefore reports whether the file was already planned under another
// name: the same path twice, a case alias on a case-insensitive file system,
// or a path through a symlinked parent. Files are compared by identity
// (device and inode where available), so distinct files whose names differ
// only in case stay distinct on case-sensitive file systems.
func (b *builder) seenBefore(p string, info fs.FileInfo) bool {
	if id, ok := fileIdentity(info); ok {
		if b.ids[id] {
			return true
		}
		b.ids[id] = true
		return false
	}
	key := p
	if abs, err := filepath.Abs(p); err == nil {
		key = abs
	}
	key = strings.ToLower(key)
	for _, prev := range b.byPath[key] {
		if os.SameFile(prev, info) {
			return true
		}
	}
	b.byPath[key] = append(b.byPath[key], info)
	return false
}

// pinIdentity makes a FileInfo remember which file it describes. On Windows
// os.SameFile reads the file ID lazily, by path; reading it now ties the
// info to the file seen while planning, not to whatever the path names at
// upload time. Elsewhere the identity is already in the info.
func pinIdentity(info fs.FileInfo) {
	os.SameFile(info, info)
}

func (b *builder) skip(p, reason string) {
	b.plan.Items = append(b.plan.Items, Item{Path: p, Status: StatusSkipped, Reason: reason})
}

func (b *builder) excluded(rel, name string) bool {
	for _, pattern := range b.opts.Excludes {
		if ok, _ := path.Match(pattern, rel); ok {
			return true
		}
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func unwrapPathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
