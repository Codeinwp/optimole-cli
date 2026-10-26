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
	// Path is the file to open. For files found in a directory it lies
	// under the directory's physically resolved path.
	Path string
	// Display is the path as the user named it: the argument, plus the path
	// below it for files found in a directory.
	Display string
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

	// pin replaces pinIdentity in tests.
	pin func(fs.FileInfo) bool
}

// Plan is the planner's result.
type Plan struct {
	// Items holds planned, skipped and failed files in walk order.
	Items []Item
	// Ignored counts hidden and excluded entries, which are not listed.
	Ignored int
}

// DisplayPath is the path to show: Display, or Path when there is none.
func (it Item) DisplayPath() string {
	if it.Display != "" {
		return it.Display
	}
	return it.Path
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
	b := &builder{ctx: ctx, opts: opts, plan: &Plan{}, planned: newIdentitySet(), others: newIdentitySet(), pin: pinIdentity}
	if opts.pin != nil {
		b.pin = opts.pin
	}
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
	// planned remembers the files to upload and others the skipped or
	// failed ones, by identity. They are kept apart so that a refused name
	// ("a.exe") never hides an accepted hard link to the same file ("b.jpg").
	planned *identitySet
	others  *identitySet
	pin     func(fs.FileInfo) bool
}

func (b *builder) addArg(arg string) error {
	arg = trimArg(arg)
	display := displayArg(arg)
	info, err := os.Lstat(arg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return usageErrorf("%s: no such file or directory", arg)
		}
		return usageErrorf("%s: %v", arg, unwrapPathError(err))
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		b.skip(arg, display, "symbolic link (not followed)")
	case info.IsDir():
		if !b.opts.Recursive {
			return usageErrorf("%s is a directory; use -r to upload its contents", arg)
		}
		root, err := physicalDir(arg)
		if err != nil {
			return usageErrorf("%s: %v", arg, unwrapPathError(err))
		}
		return b.walk(display, root)
	case info.Mode().IsRegular():
		name := filepath.Base(arg)
		if b.excluded(name, name) {
			b.plan.Ignored++
			return nil
		}
		return b.addFile(arg, display, info, "")
	default:
		b.skip(arg, display, "not a regular file")
	}
	return nil
}

// physicalDir resolves a directory argument the way the kernel does (each
// symlink in it, then any ".." after it) into an absolute path free of
// symlinks. The walk needs that: filepath.WalkDir builds child paths with
// filepath.Join, which cleans them lexically, so walking "link/.." would list
// one directory (the link target's parent) but descend through another (the
// argument's own parent). Below a symlink-free root, lexical and physical
// paths agree, and children are still lstat-checked and never followed.
func physicalDir(arg string) (string, error) {
	resolved, err := filepath.EvalSymlinks(arg)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(resolved) {
		return resolved, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Getwd may answer with $PWD, which can itself go through symlinks.
	if wd, err = filepath.EvalSymlinks(wd); err != nil {
		return "", err
	}
	return filepath.Join(wd, resolved), nil
}

// displayArg is how an argument is shown: as typed, tidied with
// filepath.Clean only when it has no ".." (where cleaning could change which
// file it names).
func displayArg(arg string) string {
	for _, part := range strings.FieldsFunc(arg, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part == ".." {
			return arg
		}
	}
	return filepath.Clean(arg)
}

// joinDisplay shows a path found below an argument: the argument as shown,
// then the relative path (in OS form), without cleaning.
func joinDisplay(display, rel string) string {
	switch {
	case rel == "." || rel == "":
		return display
	case display == ".":
		return rel
	case display == "" || strings.HasSuffix(display, "/") || os.IsPathSeparator(display[len(display)-1]):
		return display + rel
	}
	return display + string(filepath.Separator) + rel
}

// trimArg removes trailing separators and trailing "." segments ("dir/",
// "dir/.", "dir/./"), so that "link/" is seen as the symlink it is rather than
// as the directory it points to. Nothing else changes: an inner ".." keeps
// its meaning ("link/../a.jpg" is the a.jpg next to the link's target, which
// filepath.Clean would turn into the local a.jpg).
func trimArg(arg string) string {
	vol := filepath.VolumeName(arg)
	rest := arg[len(vol):]
	isSep := func(c byte) bool { return c == '/' || os.IsPathSeparator(c) }
	for {
		end := len(rest)
		for end > 0 && isSep(rest[end-1]) {
			end--
		}
		switch {
		case end == 0 && len(rest) > 0:
			return vol + rest[:1] // the root itself
		case end == 0:
			return vol
		case rest[:end] == ".":
			return vol + "."
		case end >= 2 && rest[end-1] == '.' && isSep(rest[end-2]):
			rest = rest[:end-1] // drop a trailing "." segment, then its separator
			continue
		}
		return vol + rest[:end]
	}
}

// walk plans the contents of a directory. root is its physically resolved
// path (see physicalDir); display is how the argument is shown.
func (b *builder) walk(display, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := b.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		osRel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		shown := joinDisplay(display, osRel)
		if err != nil {
			// A directory that cannot be read (or an entry that vanished).
			b.plan.Items = append(b.plan.Items, Item{Path: p, Display: shown, Status: StatusFailed, Reason: "cannot read: " + unwrapPathError(err).Error()})
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(osRel)
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
			b.skip(p, shown, "symbolic link (not followed)")
			return nil
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			b.skip(p, shown, "not a regular file")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			b.plan.Items = append(b.plan.Items, Item{Path: p, Display: shown, Status: StatusFailed, Reason: "cannot read: " + unwrapPathError(err).Error()})
			return nil
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}
		return b.addFile(p, shown, info, dir)
	})
}

// addFile plans one regular file; subdir is its directory relative to the
// argument root ("" for the root itself).
func (b *builder) addFile(p, display string, info fs.FileInfo, subdir string) error {
	size := info.Size()
	it := Item{Path: p, Display: display, Size: size, Status: StatusPlanned, info: info}
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
	if it.Status == StatusPlanned && !b.pin(info) {
		// Without a pinned identity the upload-time check could accept a
		// same-size replacement, so the file is not uploaded.
		it.Status, it.Reason = StatusFailed, "cannot read: the file changed while it was being listed"
	}
	// Identities are recorded only now, once the checks have decided which
	// set the file belongs to.
	set := b.others
	if it.Status == StatusPlanned {
		set = b.planned
	}
	if set.seen(p, info) {
		return nil
	}
	b.plan.Items = append(b.plan.Items, it)
	return nil
}

// identitySet remembers files by identity: device and inode where
// available, otherwise os.SameFile among case-folded absolute paths.
type identitySet struct {
	ids    map[any]bool
	byPath map[string][]fs.FileInfo
}

func newIdentitySet() *identitySet {
	return &identitySet{ids: map[any]bool{}, byPath: map[string][]fs.FileInfo{}}
}

// seen reports whether the file is already in the set, under this name or
// another: the same path twice, a case alias on a case-insensitive file
// system, a path through a symlinked parent, or a hard link. Otherwise it
// adds the file. Distinct files whose names differ only in case stay
// distinct on case-sensitive file systems.
func (s *identitySet) seen(p string, info fs.FileInfo) bool {
	if id, ok := fileIdentity(info); ok {
		if s.ids[id] {
			return true
		}
		s.ids[id] = true
		return false
	}
	key := p
	if abs, err := filepath.Abs(p); err == nil {
		key = abs
	}
	key = strings.ToLower(key)
	for _, prev := range s.byPath[key] {
		if os.SameFile(prev, info) {
			return true
		}
	}
	s.byPath[key] = append(s.byPath[key], info)
	return false
}

// pinIdentity makes a FileInfo remember which file it describes and reports
// whether it could. On Windows os.SameFile reads the file ID lazily, by path;
// reading it now ties the info to the file seen while planning, not to
// whatever the path names at upload time, and it fails (false) when the file
// is already gone. Elsewhere the identity is already in the info.
func pinIdentity(info fs.FileInfo) bool {
	return os.SameFile(info, info)
}

func (b *builder) skip(p, display, reason string) {
	b.plan.Items = append(b.plan.Items, Item{Path: p, Display: display, Status: StatusSkipped, Reason: reason})
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
