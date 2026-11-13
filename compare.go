package main

import (
	"net/http"
	"sort"
)

// "Do these two drives have the same footage?" Compares two drives by content (sampled hash), not by name:
// what is only on one, what both have, and how much of it is verified byte for byte. It also says, for each
// drive, how much of it exists nowhere else in the archive: what would be lost with that drive.

type Side struct {
	Clips int   `json:"clips"`
	Bytes int64 `json:"bytes"`
	List  []Row `json:"list"` // the biggest first, up to 200
}

type Comparison struct {
	A        *DeviceInfo `json:"a"`
	B        *DeviceInfo `json:"b"`
	OnlyA    Side        `json:"only_a"`
	OnlyB    Side        `json:"only_b"`
	Both     Side        `json:"both"`
	Verified int         `json:"verified"` // contents on both whose copies all have the same full hash
	UniqueA  Side        `json:"unique_a"` // on A and on no other drive at all: lost if A is lost
	UniqueB  Side        `json:"unique_b"`
}

func (c *Catalog) Compare(a, b string) Comparison {
	cmp := Comparison{A: c.device(a), B: c.device(b)}
	on := func(content, dev string) *Clip {
		for _, cl := range c.byCont[content] {
			if cl.Device == dev {
				return cl
			}
		}
		return nil
	}
	elsewhere := func(content, dev string) bool {
		for _, cl := range c.byCont[content] {
			if cl.Device != dev {
				return true
			}
		}
		return false
	}
	add := func(s *Side, cl *Clip) {
		s.Clips++
		s.Bytes += cl.Size
		s.List = append(s.List, c.row(cl, nil))
	}
	done := map[string]bool{}
	for _, cl := range c.list {
		if done[cl.Content] || (cl.Device != a && cl.Device != b) {
			continue
		}
		done[cl.Content] = true
		ca, cb := on(cl.Content, a), on(cl.Content, b)
		switch {
		case ca != nil && cb != nil:
			add(&cmp.Both, ca)
			if c.group(cl.Content).Verified {
				cmp.Verified++
			}
		case ca != nil:
			add(&cmp.OnlyA, ca)
		default:
			add(&cmp.OnlyB, cb)
		}
		if ca != nil && !elsewhere(cl.Content, a) {
			add(&cmp.UniqueA, ca)
		}
		if cb != nil && !elsewhere(cl.Content, b) {
			add(&cmp.UniqueB, cb)
		}
	}
	for _, s := range []*Side{&cmp.OnlyA, &cmp.OnlyB, &cmp.Both, &cmp.UniqueA, &cmp.UniqueB} {
		sort.Slice(s.List, func(i, j int) bool { return s.List[i].Size > s.List[j].Size })
		if len(s.List) > 200 {
			s.List = s.List[:200]
		}
		if s.List == nil {
			s.List = []Row{}
		}
	}
	return cmp
}

func serveCompare(w http.ResponseWriter, r *http.Request, c *Catalog) {
	a, b := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if a == "" || b == "" || a == b || c.device(a).UUID == "" || c.device(b).UUID == "" {
		http.Error(w, "Choose two different drives", 400)
		return
	}
	send(w, c.Compare(a, b))
}
