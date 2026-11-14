package main

import (
	"fmt"
	"testing"
)

// TestSameProject: one production under two names on two drives is one project; a few files in common are not.
func TestSameProject(t *testing.T) {
	c := &Catalog{byCont: map[string][]*Clip{}}
	add := func(dev, proj, content string, size int64) {
		cl := &Clip{Device: dev, Project: proj, Content: content, Size: size}
		c.list = append(c.list, cl)
		c.byCont[content] = append(c.byCont[content], cl)
	}
	add("T9", "Sanremo2026", "c:red", 10) // the same clip under two names that hold each other
	add("LaCie", "BILLBOARD - SANREMO 2026", "c:red", 10)
	add("LaCie", "BILLBOARD - SANREMO 2026", "c:only", 100)
	add("LaCie", "AwLab", "c:spot", 1) // two files also in Sanremo2026: a piece of its own
	add("T9", "Sanremo2026", "c:spot", 1)
	add("LaCie", "AwLab", "c:recap", 1)
	add("T9", "Sanremo2026", "c:recap", 1)
	for i := range sameShared { // names apart, but sameShared contents in common
		add("A", "Shoot", fmt.Sprint("c:s", i), 5)
		add("B", "Backup B", fmt.Sprint("c:s", i), 5)
	}
	add("B", "Backup B", "c:extra", 50)
	c.sameProject()
	want := map[string]string{"T9 c:red": "Sanremo2026", "LaCie c:red": "Sanremo2026", "LaCie c:only": "Sanremo2026",
		"LaCie c:spot": "AwLab", "A c:s0": "Backup B", "B c:s0": "Backup B"}
	for _, cl := range c.list {
		if w, ok := want[cl.Device+" "+cl.Content]; ok && cl.Project != w {
			t.Errorf("%s %s: project %q, want %q", cl.Device, cl.Content, cl.Project, w)
		}
	}
}
