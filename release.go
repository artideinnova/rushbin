package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// version is this build's, set by macos/build.sh and the Windows package step with -ldflags -X main.version=...
var version = "dev"

// Release is latest.json in the site's release store (releaseStore), written by whoever publishes the releases.
type Release struct {
	Version string       `json:"version"`
	Mac     *ReleaseFile `json:"mac,omitempty"`
	Windows *ReleaseFile `json:"windows,omitempty"`
}

type ReleaseFile struct {
	File   string `json:"file"` // next to latest.json, like rushbin-1.2.0.dmg
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// EdSignature is the Mac file's EdDSA signature by Sparkle's sign_update, with the key only the releasing
	// Mac's Keychain has: the Mac app installs an update only when it matches the public key in its Info.plist.
	EdSignature string `json:"edSignature,omitempty"`
}

// file is the download for one operating system ("darwin" or "windows"), nil when the release has none.
func (r *Release) file(goos string) *ReleaseFile {
	f := map[string]*ReleaseFile{"darwin": r.Mac, "windows": r.Windows}[goos]
	if f == nil || f.File == "" || strings.ContainsAny(f.File, "/\\") {
		return nil
	}
	return f
}

// newer tells whether version a is after b, both like 1.2.3. False when either is not a version ("dev").
// ponytail: no pre-release tags (1.2.0-beta); add them when releases use them.
func newer(a, b string) bool {
	pa, pb := semver(a), semver(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func semver(v string) []int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return nil
	}
	n := make([]int, 3)
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return nil
		}
		n[i] = x
	}
	return n
}

// releases keeps the store's latest.json for a few minutes: every app asks the site, the site asks the store rarely.
type releases struct {
	mu   sync.Mutex
	rel  *Release
	when time.Time
}

func (s *Server) latest() (*Release, error) {
	s.rel.mu.Lock()
	defer s.rel.mu.Unlock()
	if s.rel.rel != nil && time.Since(s.rel.when) < 5*time.Minute {
		return s.rel.rel, nil
	}
	from := s.upstream + "/api/version"
	if s.releases != nil {
		from = s.releases.presign("GET", "latest.json", time.Minute, time.Now())
	} else if s.upstream == "" {
		return nil, fmt.Errorf("this site keeps no releases")
	}
	res, err := client.Get(from)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("latest.json: %s", res.Status)
	}
	var r Release
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&r); err != nil || semver(r.Version) == nil {
		return nil, fmt.Errorf("latest.json is not a release: %v", err)
	}
	s.rel.rel, s.rel.when = &r, time.Now()
	return &r, nil
}

// serveVersion is GET /api/version, public: the latest release, for the apps.
func (s *Server) serveVersion(w http.ResponseWriter, r *http.Request) {
	rel, err := s.latest()
	if err != nil {
		http.Error(w, "No release on this site", 404)
		return
	}
	send(w, rel)
}

// download is GET /download/{os}, public: a short-lived link to the latest installer in the store.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	goos := map[string]string{"mac": "darwin", "windows": "windows"}[r.PathValue("os")]
	rel, err := s.latest()
	if goos == "" || err != nil || rel.file(goos) == nil {
		http.Error(w, "No release for this system on this site", 404)
		return
	}
	f := rel.file(goos)
	if s.releases == nil { // the upstream's own download, which goes on to its store
		http.Redirect(w, r, s.upstream+"/download/"+r.PathValue("os"), http.StatusFound)
		return
	}
	http.Redirect(w, r, s.releases.presign("GET", f.File, 10*time.Minute, time.Now(),
		[2]string{"response-content-disposition", `attachment; filename="` + f.File + `"`}), http.StatusFound)
}

// appcast is GET /appcast.xml, public: the latest Mac release as the Mac app's updater (Sparkle) reads it,
// with a link to the dmg in the store that works for an hour. 404 for a release not signed for Sparkle.
func (s *Server) appcast(w http.ResponseWriter, r *http.Request) {
	rel, err := s.latest()
	var f *ReleaseFile
	if err == nil {
		f = rel.file("darwin")
	}
	if f == nil || f.EdSignature == "" {
		http.Error(w, "No release for the Mac on this site", 404)
		return
	}
	e := func(v string) string { var b strings.Builder; xml.EscapeText(&b, []byte(v)); return b.String() }
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
<channel><title>rushbin</title>
<item><title>rushbin %[1]s</title><sparkle:version>%[1]s</sparkle:version><sparkle:shortVersionString>%[1]s</sparkle:shortVersionString>
<enclosure url="%[2]s" length="%[3]d" type="application/octet-stream" sparkle:edSignature="%[4]s"/></item>
</channel></rss>
`, e(rel.Version), e(s.dmgLink(f)), f.Size, e(f.EdSignature))
}

// dmgLink: where the updater downloads a Mac release, a link to the store that works for an hour, or the
// upstream's download. The signature checks the file whoever serves it: only Artide's key makes it valid.
func (s *Server) dmgLink(f *ReleaseFile) string {
	if s.releases == nil {
		return s.upstream + "/download/mac"
	}
	return s.releases.presign("GET", f.File, time.Hour, time.Now())
}

// updatesFrom: RUSHBIN_UPDATES_FROM, the site whose releases a site without its own store passes on to the apps of
// its teams; Artide's (rushbin.io) when not set, none with "off". Without it the apps of a self-hosted site would never
// learn of a new version. The apps then download it from that site, which sees their address (docs/self-host.md).
func updatesFrom() string {
	switch v := strings.TrimRight(os.Getenv("RUSHBIN_UPDATES_FROM"), "/"); v {
	case "":
		return artideSite
	case "off":
		return ""
	default:
		return v
	}
}

// artideSite is where Artide publishes rushbin: an app in no team asks it for new versions, only if allowed.
const artideSite = "https://rushbin.io"

// updatesAllowed is the choice of an app in no team about asking artideSite for new versions (updates.json):
// nil until the page asked.
func (a *App) updatesAllowed() *bool {
	var u struct{ Ask *bool }
	b, _ := os.ReadFile(filepath.Join(a.dir, "updates.json"))
	json.Unmarshal(b, &u)
	return u.Ask
}

func (a *App) setUpdates(w http.ResponseWriter, r *http.Request) {
	var b struct{ Ask bool }
	if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&b) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	j, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(a.dir, "updates.json"), j, 0o644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	send(w, map[string]bool{"ok": true})
}

// updateSite is the site the app asks for new versions: its team's, or artideSite when in no team and allowed.
// Nil: it asks no one.
func (a *App) updateSite() *Team {
	if t := a.teamConf(); t != nil {
		return t
	}
	if on(a.updatesAllowed()) {
		return &Team{URL: artideSite} // no token: /api/version is public, nothing about the computer is sent
	}
	return nil
}

// feed is where the Mac app's updater (Sparkle) asks for new versions: the update site's appcast, "" for no one.
func (a *App) feed() string {
	if t := a.updateSite(); t != nil {
		return t.URL + "/appcast.xml"
	}
	return ""
}

// feedLoop tells the Mac app its feed on out, at start and whenever it changes: a line "feed URL", or "feed"
// alone when it must ask no one. Every minute, so a Mac that joins a team, or allows asking, asks soon after.
func (a *App) feedLoop(out io.Writer) {
	last := "-"
	for ; ; time.Sleep(time.Minute) {
		if f := a.feed(); f != last {
			fmt.Fprintln(out, strings.TrimSpace("feed "+f))
			last = f
		}
	}
}

// updateLoop (Windows) asks the update site for the latest release, then every 6 hours, and at once when that
// site changes.
func (a *App) updateLoop() {
	var last time.Time
	var from string
	for ; ; time.Sleep(time.Minute) { // every minute, so a Mac that joins a team, or allows asking, asks soon after
		t := a.updateSite()
		if t == nil || t.URL == from && time.Since(last) < 6*time.Hour {
			continue
		}
		last, from = time.Now(), t.URL
		var rel Release
		if err := t.call("GET", "/api/version", nil, &rel); err != nil {
			log.Println("update:", err)
			continue
		}
		a.smu.Lock()
		a.release, a.relFrom = &rel, t.URL
		a.smu.Unlock()
	}
}

// update is what /api/me tells the page on Windows when the update site t has a newer version for it, nil
// otherwise. The Mac app updates itself (feed).
func (a *App) update(t *Team) map[string]string {
	a.smu.Lock()
	rel, from := a.release, a.relFrom
	a.smu.Unlock()
	if rel == nil || from != t.URL || !newer(rel.Version, version) || rel.file("windows") == nil {
		return nil
	}
	return map[string]string{"version": rel.Version, "url": t.URL + "/download/windows"}
}
