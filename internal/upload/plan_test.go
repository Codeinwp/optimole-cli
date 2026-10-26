package upload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		rel, err := filepath.Rel(root, it.Path)
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
