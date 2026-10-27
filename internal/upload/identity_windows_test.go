package upload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtendedPath(t *testing.T) {
	long := strings.Repeat("d", 130)
	deep := long + `\` + long + `\a.jpg`
	cases := map[string]string{
		`C:\photos\a.jpg`:     `C:\photos\a.jpg`, // short: unchanged
		`C:\` + deep:          `\\?\C:\` + deep,
		`C:\x\..\` + deep:     `\\?\C:\` + deep, // cleaned first, as Windows would
		`\\srv\share\` + deep: `\\?\UNC\srv\share\` + deep,
		`\\?\C:\` + deep:      `\\?\C:\` + deep, // already extended
	}
	for in, want := range cases {
		if got := extendedPath(in); got != want {
			t.Errorf("extendedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlanLongWindowsPaths: a file deeper than MAX_PATH is planned, not
// failed for want of a file ID.
func TestPlanLongWindowsPaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, strings.Repeat("d", 130), strings.Repeat("e", 130))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), []string{root}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Status != StatusPlanned {
		t.Fatalf("plan = %+v", plan.Items)
	}
}
