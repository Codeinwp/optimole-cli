package upload

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile creates a file (and its directories) below root.
func writeFile(t *testing.T, root, rel string, size int) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type planned struct {
	status Status
	dest   Destination
	reason string
}

// byRel indexes the plan by path relative to root, with forward slashes.
func byRel(t *testing.T, root string, p *Plan) map[string]planned {
	t.Helper()
	out := map[string]planned{}
	for _, it := range p.Items {
		rel, err := filepath.Rel(root, it.DisplayPath())
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(rel)] = planned{it.Status, it.Dest, it.Reason}
	}
	return out
}

func TestPlanRecursiveMapping(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.jpg", 10)
	writeFile(t, root, "B.PNG", 10)
	writeFile(t, root, "notes.bmp", 10)
	writeFile(t, root, "index.html", 10)
	writeFile(t, root, "README", 10)
	writeFile(t, root, "empty.png", 0)
	writeFile(t, root, ".hidden.jpg", 10)
	writeFile(t, root, ".git/config.jpg", 10)
	writeFile(t, root, "2024/c.jpg", 10)
	writeFile(t, root, "2024/raw/d.gif", 10)
	writeFile(t, root, "2024/skip-me.jpg", 10)
	writeFile(t, root, "drafts/e.jpg", 10)
	writeFile(t, root, "2024/thumbs/f.jpg", 10)
	big := writeFile(t, root, "big.mp4", 0)
	if err := os.Truncate(big, MaxFileSize+1); err != nil {
		t.Fatal(err)
	}
	exact := writeFile(t, root, "exact.mp4", 0)
	if err := os.Truncate(exact, MaxFileSize); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{
		Folder:    "Campaigns",
		Recursive: true,
		Excludes:  []string{"skip-*", "drafts", "2024/thumbs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	want := map[string]planned{
		"a.jpg":          {StatusPlanned, Destination{Path: "Campaigns"}, ""},
		"B.PNG":          {StatusPlanned, Destination{Path: "Campaigns"}, ""},
		"exact.mp4":      {StatusPlanned, Destination{Path: "Campaigns"}, ""},
		"2024/c.jpg":     {StatusPlanned, Destination{Path: "Campaigns/2024"}, ""},
		"2024/raw/d.gif": {StatusPlanned, Destination{Path: "Campaigns/2024/raw"}, ""},
		"notes.bmp":      {StatusSkipped, Destination{Path: "Campaigns"}, "unsupported file type .bmp"},
		"index.html":     {StatusSkipped, Destination{Path: "Campaigns"}, "blocked file type .html"},
		"README":         {StatusSkipped, Destination{Path: "Campaigns"}, "no file extension"},
		"empty.png":      {StatusSkipped, Destination{Path: "Campaigns"}, "empty file"},
		"big.mp4":        {StatusSkipped, Destination{Path: "Campaigns"}, "larger than the 100 MB limit (100.0 MB)"},
	}
	if len(got) != len(want) {
		t.Errorf("got %d items, want %d: %v", len(got), len(want), got)
	}
	for rel, w := range want {
		if g, ok := got[rel]; !ok || g != w {
			t.Errorf("%s = %+v (present %v), want %+v", rel, g, ok, w)
		}
	}
	// .hidden.jpg, .git, skip-me.jpg, drafts and 2024/thumbs.
	if plan.Ignored != 5 {
		t.Errorf("Ignored = %d, want 5", plan.Ignored)
	}
	if n := len(plan.Selected()); n != 5 {
		t.Errorf("Selected = %d, want 5", n)
	}
}

func TestPlanRootAndHidden(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.jpg", 1)
	writeFile(t, root, ".hidden.jpg", 1)
	writeFile(t, root, ".cache/b.jpg", 1)
	writeFile(t, root, "sub/c.jpg", 1)

	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{Recursive: true, IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	want := map[string]Destination{
		"a.jpg":        {},
		".hidden.jpg":  {},
		".cache/b.jpg": {Path: ".cache"},
		"sub/c.jpg":    {Path: "sub"},
	}
	for rel, d := range want {
		if got[rel].status != StatusPlanned || got[rel].dest != d {
			t.Errorf("%s = %+v, want planned to %+v", rel, got[rel], d)
		}
	}
	if plan.Ignored != 0 {
		t.Errorf("Ignored = %d", plan.Ignored)
	}
}

func TestPlanExplicitFiles(t *testing.T) {
	root := t.TempDir()
	hidden := writeFile(t, root, ".logo.png", 1)
	a := writeFile(t, root, "a.jpg", 1)
	thumb := writeFile(t, root, "a-thumb.jpg", 1)

	plan, err := BuildPlan(context.Background(), []string{hidden, a, a, thumb}, PlanOptions{Folder: "X", Excludes: []string{"*-thumb.jpg"}})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	// An explicitly named hidden file is uploaded; duplicates collapse; the
	// exclude pattern applies to named files too.
	if len(plan.Items) != 2 || got[".logo.png"].status != StatusPlanned || got["a.jpg"].dest != (Destination{Path: "X"}) {
		t.Fatalf("plan = %+v", plan.Items)
	}
	if plan.Ignored != 1 {
		t.Fatalf("Ignored = %d, want 1", plan.Ignored)
	}
}

func TestPlanSymlinksAreNotFollowed(t *testing.T) {
	root := t.TempDir()
	target := writeFile(t, root, "real/a.jpg", 1)
	if err := os.Symlink(target, filepath.Join(root, "link.jpg")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	for _, rel := range []string{"link.jpg", "linkdir"} {
		if got[rel].status != StatusSkipped || got[rel].reason != "symbolic link (not followed)" {
			t.Errorf("%s = %+v, want skipped symlink", rel, got[rel])
		}
	}
	if got["real/a.jpg"].status != StatusPlanned {
		t.Errorf("real/a.jpg = %+v", got["real/a.jpg"])
	}
	if len(plan.Items) != 3 {
		t.Errorf("items = %+v; linkdir must not be walked", plan.Items)
	}

	// A symlink named on the command line is skipped too.
	plan, err = BuildPlan(context.Background(), []string{filepath.Join(root, "link.jpg")}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Status != StatusSkipped {
		t.Fatalf("plan = %+v", plan.Items)
	}
}

// TestPlanTrailingSlashSymlink: "link/" resolves to the directory on most
// systems; the argument is cleaned first, so the symlink is still refused.
func TestPlanTrailingSlashSymlink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "real/a.jpg", 1)
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	for _, arg := range []string{link + string(filepath.Separator), link + "/", link + "/."} {
		plan, err := BuildPlan(context.Background(), []string{arg}, PlanOptions{Recursive: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Items) != 1 || plan.Items[0].Status != StatusSkipped || plan.Items[0].Reason != "symbolic link (not followed)" {
			t.Fatalf("%s: plan = %+v", arg, plan.Items)
		}
	}
}

// TestPlanKeepsInnerDotDot: "link/../photo.jpg" names the photo.jpg next to
// the link's target, not the local one; only trailing separators and "."
// segments are trimmed from arguments.
func TestPlanKeepsInnerDotDot(t *testing.T) {
	root := t.TempDir()
	other := writeFile(t, root, "other/photo.jpg", 5)
	writeFile(t, root, "other/subdir/x.jpg", 1)
	writeFile(t, root, "work/photo.jpg", 7)
	if err := os.Symlink(filepath.Join(root, "other", "subdir"), filepath.Join(root, "work", "link")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	arg := filepath.Join(root, "work", "link") + string(filepath.Separator) + ".." + string(filepath.Separator) + "photo.jpg"
	plan, err := BuildPlan(context.Background(), []string{arg}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Path != arg || plan.Items[0].Size != 5 || !os.SameFile(plan.Items[0].info, want) {
		t.Fatalf("plan = %+v, want %s (the target's sibling)", plan.Items, other)
	}
}

// TestPlanRefusedHardLinkDoesNotHideAcceptedOne: "a.exe" and "b.jpg" are
// one file; the refused name must not swallow the accepted one.
func TestPlanRefusedHardLinkDoesNotHideAcceptedOne(t *testing.T) {
	root := t.TempDir()
	exe := writeFile(t, root, "a.exe", 4)
	if err := os.Link(exe, filepath.Join(root, "b.jpg")); err != nil {
		t.Skipf("cannot create hard links here: %v", err)
	}
	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	if got["a.exe"].status != StatusSkipped || got["b.jpg"].status != StatusPlanned || len(plan.Selected()) != 1 {
		t.Fatalf("plan = %+v", plan.Items)
	}
	// Named twice, the refused file is still listed once.
	plan, err = BuildPlan(context.Background(), []string{exe, exe}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("plan = %+v", plan.Items)
	}
}

// TestPlanRejectsFilesWhoseIdentityCannotBePinned: on Windows the file ID is
// read by path while planning; if that fails (the file vanished), the upload
// check would have nothing to compare with, so the file must not be planned.
func TestPlanRejectsFilesWhoseIdentityCannotBePinned(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.jpg", 3)
	writeFile(t, root, "b.jpg", 3)
	pin := func(info fs.FileInfo) bool { return info.Name() != "a.jpg" }
	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{Recursive: true, pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	if a := got["a.jpg"]; a.status != StatusFailed || !strings.Contains(a.reason, "changed while it was being listed") {
		t.Fatalf("a.jpg = %+v", a)
	}
	if got["b.jpg"].status != StatusPlanned || len(plan.Selected()) != 1 {
		t.Fatalf("plan = %+v", plan.Items)
	}
	// The real pin works on files that exist.
	info, err := os.Lstat(filepath.Join(root, "b.jpg"))
	if err != nil || !pinIdentity(info) {
		t.Fatalf("pinIdentity(existing file) = false, %v", err)
	}
}

// TestPlanWalksThePhysicalRoot: work/link points to data/leaf, data/album is
// a real directory and work/album points to secrets. "work/link/.." is data,
// physically; walking it lexically would list data but descend through
// work/album into secrets.
func TestPlanWalksThePhysicalRoot(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "data/leaf/.keep", 1)
	a := writeFile(t, base, "data/album/a.jpg", 2)
	b := writeFile(t, base, "data/album/b.jpg", 3)
	secret := writeFile(t, base, "secrets/secret.jpg", 2)
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "data", "leaf"), filepath.Join(base, "work", "link")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(base, "secrets"), filepath.Join(base, "work", "album")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	arg := filepath.Join(base, "work", "link") + sep + ".."

	plan, err := BuildPlan(context.Background(), []string{arg}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	selected := plan.Selected()
	if len(selected) != 2 {
		t.Fatalf("selected = %+v", selected)
	}
	secretInfo, _ := os.Lstat(secret)
	for i, want := range []string{a, b} {
		wantInfo, _ := os.Lstat(want)
		it := selected[i]
		if !os.SameFile(it.info, wantInfo) || os.SameFile(it.info, secretInfo) {
			t.Fatalf("item %d (%s) is not %s", i, it.Path, want)
		}
		if it.Dest != (Destination{Path: "album"}) {
			t.Fatalf("item %d dest = %+v", i, it.Dest)
		}
		// Shown as typed: the argument, then the path below it.
		if wantShown := arg + sep + "album" + sep + filepath.Base(want); it.DisplayPath() != wantShown {
			t.Fatalf("item %d shown as %q, want %q", i, it.DisplayPath(), wantShown)
		}
		// Opened through the resolved root, which is free of symlinks.
		if resolved, _ := filepath.EvalSymlinks(it.Path); resolved != it.Path {
			t.Fatalf("item %d path %q goes through a symlink", i, it.Path)
		}
	}
}

// TestPlanPlainDirectoryArgumentsUnchanged: "dir", "dir/" and "./dir" show
// the same paths and map to the same folders as before the physical walk.
func TestPlanPlainDirectoryArgumentsUnchanged(t *testing.T) {
	base := t.TempDir()
	writeFile(t, base, "dir/a.jpg", 1)
	writeFile(t, base, "dir/sub/b.jpg", 1)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	sep := string(filepath.Separator)
	abs := filepath.Join(base, "dir")
	cases := map[string]string{
		"dir":             "dir",
		"dir" + sep:       "dir",
		"." + sep + "dir": "dir",
		abs:               abs,
		abs + sep:         abs,
	}
	for arg, shownRoot := range cases {
		plan, err := BuildPlan(context.Background(), []string{arg}, PlanOptions{Recursive: true, Folder: "F"})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Destination{}
		for _, it := range plan.Selected() {
			got[it.DisplayPath()] = it.Dest
		}
		want := map[string]Destination{
			filepath.Join(shownRoot, "a.jpg"):        {Path: "F"},
			filepath.Join(shownRoot, "sub", "b.jpg"): {Path: "F/sub"},
		}
		if len(got) != len(want) {
			t.Fatalf("%q: got %v, want %v", arg, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%q: got %v, want %v", arg, got, want)
			}
		}
	}
}

// TestPlanWindowsDriveRootedArgument: on Windows "\dir" names dir at the root
// of the current drive, not below the working directory, even when a
// directory with that path also exists there.
func TestPlanWindowsDriveRootedArgument(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-rooted paths exist only on Windows")
	}
	base := t.TempDir()
	writeFile(t, base, "photos/right.jpg", 1)
	rooted := strings.TrimPrefix(filepath.Join(base, "photos"), filepath.VolumeName(base))

	// The same path below the working directory holds a different file.
	work := filepath.Join(base, "work")
	writeFile(t, work, filepath.Join(rooted, "wrong.jpg"), 1)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	plan, err := BuildPlan(context.Background(), []string{rooted}, PlanOptions{Recursive: true, Folder: "F"})
	if err != nil {
		t.Fatal(err)
	}
	selected := plan.Selected()
	if len(selected) != 1 || filepath.Base(selected[0].Path) != "right.jpg" {
		t.Fatalf("selected %+v, want only photos\\right.jpg at the drive root", selected)
	}
}

func TestTrimArg(t *testing.T) {
	sep := string(filepath.Separator)
	cases := map[string]string{
		"link/":         "link",
		"link//":        "link",
		"link/.":        "link",
		"link/./":       "link",
		"link/./.":      "link",
		"a/../b.jpg":    "a/../b.jpg",
		"a/./b":         "a/./b",
		"./photos/":     "./photos",
		".":             ".",
		"./":            ".",
		"/":             "/",
		"///":           "/",
		"..":            "..",
		"../":           "..",
		"a" + sep + ".": "a",
	}
	for in, want := range cases {
		if got := trimArg(in); got != want {
			t.Errorf("trimArg(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlanCaseAliases: on a case-insensitive file system "a.jpg" and "A.JPG"
// are one file and must be uploaded once; on a case-sensitive one they are
// two files and both are kept.
func TestPlanCaseAliases(t *testing.T) {
	root := t.TempDir()
	lower := writeFile(t, root, "photo.jpg", 3)
	upper := filepath.Join(root, "PHOTO.JPG")
	_, err := os.Lstat(upper)
	insensitive := err == nil
	if !insensitive {
		writeFile(t, root, "PHOTO.JPG", 5)
	}
	plan, err := BuildPlan(context.Background(), []string{lower, upper, root}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	want := 2
	if insensitive {
		want = 1
	}
	if n := len(plan.Selected()); n != want {
		t.Fatalf("case-insensitive=%v: %d files planned, want %d: %+v", insensitive, n, want, plan.Items)
	}
}

func TestPlanUsageErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.jpg", 1)
	writeFile(t, root, "sub/b.jpg", 1)
	writeFile(t, root, "only-skipped/notes.bmp", 1)

	cases := []struct {
		name string
		args []string
		opts PlanOptions
	}{
		{"directory without -r", []string{root}, PlanOptions{}},
		{"missing path", []string{filepath.Join(root, "nope.jpg")}, PlanOptions{}},
		{"bad exclude pattern", []string{root}, PlanOptions{Recursive: true, Excludes: []string{"[a"}}},
		{"folder-id with subdirectories", []string{root}, PlanOptions{Recursive: true, FolderID: 7}},
	}
	for _, tc := range cases {
		_, err := BuildPlan(context.Background(), tc.args, tc.opts)
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%s: err = %v, want a UsageError", tc.name, err)
		}
	}

	// Skipped files in subdirectories do not trip the --folder-id rule.
	plan, err := BuildPlan(context.Background(), []string{filepath.Join(root, "a.jpg"), filepath.Join(root, "only-skipped")}, PlanOptions{Recursive: true, FolderID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].Dest != (Destination{ID: 7}) || plan.Items[1].Status != StatusSkipped {
		t.Fatalf("plan = %+v", plan.Items)
	}
}

func TestPlanFolderLimits(t *testing.T) {
	root := t.TempDir()
	deep := strings.Repeat("d/", 20) + "a.jpg" // 20 subdirectories
	writeFile(t, root, deep, 1)
	writeFile(t, root, "ok/b.jpg", 1)

	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{Folder: "Base", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(t, root, plan)
	if g := got[deep]; g.status != StatusSkipped || !strings.Contains(g.reason, "the limit is 20") {
		t.Errorf("deep file = %+v, want skipped for depth", g)
	}
	if got["ok/b.jpg"].dest != (Destination{Path: "Base/ok"}) {
		t.Errorf("ok/b.jpg = %+v", got["ok/b.jpg"])
	}
}

func TestNormalizeFolderPath(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"/", "", true},
		{"Campaigns", "Campaigns", true},
		{"/Campaigns/2026/", "Campaigns/2026", true},
		{" Campaigns / 2026 ", "Campaigns/2026", true},
		{"a//b", "", false},
		{"a/ /b", "", false},
		{strings.Repeat("x", 256), "", false},
		{strings.Repeat("é", 255), strings.Repeat("é", 255), true},
		{strings.TrimSuffix(strings.Repeat("a/", 20), "/"), strings.TrimSuffix(strings.Repeat("a/", 20), "/"), true},
		{strings.TrimSuffix(strings.Repeat("a/", 21), "/"), "", false},
	}
	for _, tc := range cases {
		got, err := NormalizeFolderPath(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NormalizeFolderPath(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestCheckFilename(t *testing.T) {
	ok := []string{"a.jpg", "A.JPG", "photo.final.WebP", "clip.3gp", "x.onetoc2", "data.json", strings.Repeat("n", 251) + ".jpg"}
	for _, n := range ok {
		if err := CheckFilename(n); err != nil {
			t.Errorf("CheckFilename(%q) = %v", n, err)
		}
	}
	bad := map[string]string{
		"page.html":                       "blocked file type .html",
		"app.JS":                          "blocked file type .js",
		"setup.exe":                       "blocked file type .exe",
		"image.bmp":                       "unsupported file type .bmp",
		"Makefile":                        "no file extension",
		strings.Repeat("n", 252) + ".jpg": "file name longer than 255 characters",
	}
	for n, want := range bad {
		err := CheckFilename(n)
		if err == nil || err.Error() != want {
			t.Errorf("CheckFilename(%q) = %v, want %q", n, err, want)
		}
	}
}

func TestCheckSize(t *testing.T) {
	if CheckSize(0) == nil || CheckSize(MaxFileSize+1) == nil {
		t.Fatal("bad sizes accepted")
	}
	if CheckSize(1) != nil || CheckSize(MaxFileSize) != nil {
		t.Fatal("good sizes refused")
	}
	if MaxFileSize != 104857600 {
		t.Fatalf("MaxFileSize = %d", MaxFileSize)
	}
}

func TestPlanStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	for i := range 20 {
		writeFile(t, root, filepath.Join("d", strings.Repeat("x", i+1)+".jpg"), 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildPlan(ctx, []string{root}, PlanOptions{Recursive: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
