package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestTidyGaps: the LaCie case. A plan that moves DAY 1 and DAY 2 into Footage but leaves DAY 4 at the project's root
// leaves the drive half tidied; the tree after the complete plan has every day in Footage.
func TestTidyGaps(t *testing.T) {
	m := t.TempDir()
	c := &Catalog{files: map[string]Rec{}}
	for _, f := range []string{"P/DAY 1/GIRATO/A004_C001.RDC/A004_C001_001.R3D", "P/DAY 2/A005_C001.RDC/A005_C001_001.R3D",
		"P/DAY 4/GIRATO/A007_C031.RDC/A007_C031_001.R3D", "P/PROJ/edit.prproj", "Q/DAY 9/clip.r3d"} {
		p := filepath.Join(m, filepath.FromSlash(f))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte("x"), 0o644))
		c.files[p] = Rec{Path: p}
	}
	plan := func(days ...string) []TidyMove {
		var ms []TidyMove
		for _, d := range days {
			ms = append(ms, TidyMove{From: "P/" + d, To: "P/Footage/" + d})
		}
		ms = validTidy(m, nil, ms)
		for i := range ms {
			ms[i].Kind = tidyKind(c, m, ms[i].From)
		}
		return ms
	}
	half := plan("DAY 1", "DAY 2")
	if g := tidyGaps(c, m, nil, half); len(g) != 1 || g[0].String() != "P/DAY 4 stays in P, while P/DAY 1 (also footage) moves to P/Footage" {
		t.Fatalf("gaps of the half plan: %q", g)
	}
	whole := plan("DAY 1", "DAY 2", "DAY 4")
	if g := tidyGaps(c, m, nil, whole); len(g) != 0 {
		t.Fatalf("gaps of the whole plan: %q", g)
	}
	tree := tidyTree(c, m, whole)["P"]
	if !slices.Contains(tree[0], "P/DAY 4/GIRATO") || !slices.Contains(tree[1], "P/Footage/DAY 4/GIRATO") ||
		slices.Contains(tree[1], "P/DAY 4") || slices.ContainsFunc(tree[1], func(d string) bool { return filepath.Ext(d) == ".RDC" }) {
		t.Fatalf("tree before %q after %q", tree[0], tree[1])
	}
	if _, ok := tidyTree(c, m, whole)["Q"]; ok {
		t.Fatal("a project the plan does not touch is in the tree")
	}
}
