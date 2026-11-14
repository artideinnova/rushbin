package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestListNames: every name is listed, a numbered sequence of 4 or more as its first and last.
func TestListNames(t *testing.T) {
	got := listNames([]string{"576_0019.MXF", "576_0017.MXF", "576_0018.MXF", "576_0020.MXF", "Nayt v2.mp4", "Nayt v1.mp4", "SSS.prproj"})
	if want := "576_0017.MXF … 576_0020.MXF (4), Nayt v1.mp4, Nayt v2.mp4, SSS.prproj"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

// TestTidyKeepsChecks: a tidy-up moving a folder keeps the byte-for-byte checks of the files in it, under their
// new path, and its undo keeps them too.
func TestTidyKeepsChecks(t *testing.T) {
	dir, mount := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "disks", "D"), 0o755))
	must(t, os.MkdirAll(filepath.Join(mount, "Arisa"), 0o755))
	clip := filepath.Join(mount, "Arisa", "576_0090.MXF")
	must(t, os.WriteFile(clip, []byte("x"), 0o644))
	hashes := filepath.Join(dir, "disks", "D", "fullhash.tsv")
	must(t, os.WriteFile(hashes, []byte("abc\t"+clip+"\t1\t100\n"), 0o644))
	if _, err := tidyApply(dir, "D", mount, []TidyMove{{From: "Arisa", To: "Artisti/Arisa"}}); err != nil {
		t.Fatal(err)
	}
	valid, err := validHashes(hashes)
	must(t, err)
	if !valid[stamp(filepath.Join(mount, "Artisti", "Arisa", "576_0090.MXF"), 1, 100)] {
		t.Fatalf("check lost by the move: %v", valid)
	}
	u, _ := readUndo(dir)
	if _, err := tidyRevert(dir, mount, u); err != nil {
		t.Fatal(err)
	}
	if valid, _ = validHashes(hashes); !valid[stamp(clip, 1, 100)] {
		t.Fatalf("check lost by the undo: %v", valid)
	}
}

// TestTidyLimits: a move in a folder the catalog skips, or one that renames a file, is refused.
func TestTidyLimits(t *testing.T) {
	m := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(m, "poc", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(m, "Nayt v2.mp4"), []byte("x"), 0o644))
	got := validTidy(m, []string{"poc"}, []TidyMove{{From: "poc/x", To: "Archive/x"}, {From: "Nayt v2.mp4", To: "Export/Nayt v3.mp4"}})
	ok := validTidy(m, []string{"poc"}, []TidyMove{{From: "Nayt v2.mp4", To: "Export/Nayt v2.mp4"}})
	if got[0].Why != "In a folder this drive's catalog skips" || got[1].Why != "A file keeps its name" || !ok[0].OK {
		t.Fatalf("%+v", got)
	}
}
