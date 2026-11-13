package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.2.0", "1.1.9", true}, {"1.10.0", "1.9.0", true}, {"2.0.0", "1.99.99", true}, {"v1.0.1", "1.0.0", true},
		{"1.2.0", "1.2.0", false}, {"1.1.9", "1.2.0", false}, {"1.2.0", "dev", false}, {"dev", "1.0.0", false},
		{"1.2", "1.0.0", false}, {"1.2.x", "1.0.0", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// The site reads latest.json from the store (a fake S3 here), keeps it a few minutes and sends the apps to a signed link.
func TestRelease(t *testing.T) {
	asked := 0
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bucket/rushbin/releases/latest.json" || r.URL.Query().Get("X-Amz-Signature") == "" {
			http.NotFound(w, r)
			return
		}
		asked++
		w.Write([]byte(`{"version":"1.2.0","mac":{"file":"rushbin-1.2.0.dmg","sha256":"ab","size":3,"edSignature":"c2ln+/="}}`))
	}))
	defer fakeS3.Close()
	t.Setenv("RUSHBIN_RELEASES_S3_BUCKET", "") // no store of their own: under releases/ in the main one
	s := &Server{releases: releaseStore(&Store{Endpoint: fakeS3.URL, Region: "auto", Bucket: "bucket", Prefix: "rushbin/", KeyID: "k", Secret: "s"})}
	site := httptest.NewServer(s.routes())
	defer site.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for range 2 {
		var rel Release
		if err := (&Team{URL: site.URL}).call("GET", "/api/version", nil, &rel); err != nil || rel.Version != "1.2.0" || rel.Mac.Size != 3 {
			t.Fatalf("/api/version: %+v %v", rel, err)
		}
	}
	if asked != 1 {
		t.Errorf("the store was asked %d times, want 1 (cached)", asked)
	}
	res, err := noFollow.Get(site.URL + "/download/mac")
	if err != nil || res.StatusCode != http.StatusFound {
		t.Fatalf("/download/mac: %v %v", res.Status, err)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Host != strings.TrimPrefix(fakeS3.URL, "http://") || loc.Path != "/bucket/rushbin/releases/rushbin-1.2.0.dmg" ||
		loc.Query().Get("X-Amz-Expires") != "600" || !strings.Contains(loc.Query().Get("response-content-disposition"), "rushbin-1.2.0.dmg") {
		t.Errorf("/download/mac goes to %s", loc)
	}
	for _, p := range []string{"/download/windows", "/download/linux"} { // no Windows file in this release; no such system
		if res, _ := noFollow.Get(site.URL + p); res.StatusCode != 404 {
			t.Errorf("%s: %s, want 404", p, res.Status)
		}
	}

	// The appcast the Mac app's updater reads: this version, the signature, and a signed link to the dmg itself.
	var feed struct {
		Item struct {
			Version   string `xml:"version"`
			Enclosure struct {
				URL       string `xml:"url,attr"`
				Length    int64  `xml:"length,attr"`
				Signature string `xml:"edSignature,attr"`
			} `xml:"enclosure"`
		} `xml:"channel>item"`
	}
	res, err = http.Get(site.URL + "/appcast.xml")
	if err != nil || res.StatusCode != 200 || xml.NewDecoder(res.Body).Decode(&feed) != nil {
		t.Fatalf("/appcast.xml: %v %v", res.Status, err)
	}
	enc := feed.Item.Enclosure
	loc, _ = url.Parse(enc.URL)
	if feed.Item.Version != "1.2.0" || enc.Length != 3 || enc.Signature != "c2ln+/=" || loc.Path != "/bucket/rushbin/releases/rushbin-1.2.0.dmg" ||
		loc.Query().Get("X-Amz-Expires") != "3600" || loc.Query().Get("X-Amz-Signature") == "" {
		t.Errorf("/appcast.xml: %+v", feed.Item)
	}
	unsigned := &Server{releases: s.releases, rel: releases{rel: &Release{Version: "1.3.0", Mac: &ReleaseFile{File: "rushbin-1.3.0.dmg", Size: 3}}, when: time.Now()}}
	w := httptest.NewRecorder()
	if unsigned.appcast(w, httptest.NewRequest("GET", "/appcast.xml", nil)); w.Code != 404 {
		t.Errorf("/appcast.xml for a dmg not signed for Sparkle: %d, want 404", w.Code)
	}

	// Windows: the app tells the page about the update only when it is newer than itself and has a file for it.
	tm := &Team{URL: "https://rushbin.example.com"}
	a := &App{release: &Release{Version: "1.2.0", Mac: &ReleaseFile{File: "rushbin-1.2.0.dmg"}, Windows: &ReleaseFile{File: "rushbin-1.2.0-windows.zip"}}, relFrom: tm.URL}
	defer func(v string) { version = v }(version)
	version = "1.1.0"
	if u := a.update(tm); u == nil || u["url"] != tm.URL+"/download/windows" || u["version"] != "1.2.0" {
		t.Errorf("update: %v", u)
	}
	for _, v := range []string{"1.2.0", "dev"} {
		if version = v; a.update(tm) != nil {
			t.Errorf("version %s: update offered", v)
		}
	}
	version = "1.1.0"
	if a.release.Windows = nil; a.update(tm) != nil {
		t.Error("update offered with no Windows file")
	}
}

// The release store of its own, when set, wins over the main one, which it leaves as it is.
func TestReleaseStore(t *testing.T) {
	for k, v := range map[string]string{"ENDPOINT": "https://a.eu.r2.cloudflarestorage.com/", "BUCKET": "releases-bucket", "KEY_ID": "ro", "SECRET": "s"} {
		t.Setenv("RUSHBIN_RELEASES_S3_"+k, v)
	}
	main := &Store{Endpoint: "https://a.eu.r2.cloudflarestorage.com", Region: "auto", Bucket: "videos-bucket", Prefix: "rushbin/", KeyID: "rw", Secret: "s"}
	want := Store{Endpoint: "https://a.eu.r2.cloudflarestorage.com", Region: "auto", Bucket: "releases-bucket", KeyID: "ro", Secret: "s"}
	for _, m := range []*Store{main, nil} {
		if got := releaseStore(m); got == nil || *got != want {
			t.Errorf("releaseStore(%v) = %+v, want %+v", m, got, want)
		}
	}
	t.Setenv("RUSHBIN_RELEASES_S3_BUCKET", "")
	if got := releaseStore(main); got == nil || got.Bucket != "videos-bucket" || got.KeyID != "rw" || got.Prefix != "rushbin/releases/" || main.Prefix != "rushbin/" {
		t.Errorf("fallback: %+v, main %+v", got, main)
	}
	if got := releaseStore(nil); got != nil {
		t.Errorf("no store: %+v", got)
	}
}

// An app in no team asks no one for new versions until allowed, then Artide's site; in a team, the team's
// site. A release heard from one site is not offered once the app asks another. The Mac app's feed follows.
func TestUpdateSite(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.0.0"
	a := &App{dir: t.TempDir()}
	if a.updateSite() != nil || a.updatesAllowed() != nil || a.feed() != "" {
		t.Fatal("asks for new versions before it was allowed")
	}
	w := httptest.NewRecorder()
	a.setUpdates(w, httptest.NewRequest("POST", "/api/updates", strings.NewReader(`{"ask":true}`)))
	if s := a.updateSite(); w.Code != 200 || s == nil || s.URL != artideSite || s.Token != "" || a.feed() != artideSite+"/appcast.xml" {
		t.Fatalf("allowed: %d %+v %q", w.Code, s, a.feed())
	}
	a.release, a.relFrom = &Release{Version: "99.0.0", Windows: &ReleaseFile{File: "x.zip"}}, artideSite
	if u := a.update(a.updateSite()); u == nil || u["url"] != artideSite+"/download/windows" {
		t.Fatalf("update from Artide's site: %v", u)
	}
	a.team = &Team{URL: "https://team.example", Token: "t"}
	if s := a.updateSite(); s != a.team || a.update(s) != nil || a.feed() != "https://team.example/appcast.xml" {
		t.Fatal("in a team: the team's site, and not the release Artide's site gave")
	}
	a.team = nil
	a.setUpdates(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/updates", strings.NewReader(`{"ask":false}`)))
	if a.updateSite() != nil || a.feed() != "" {
		t.Fatal("still asks after being told no")
	}
}

// Visitors not signed in go to the sign-in page; the site asks search engines to index nothing, and has no
// pages of Artide's own site.
func TestVisitor(t *testing.T) {
	site := httptest.NewServer((&Server{}).routes())
	defer site.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.Get(site.URL + "/")
	if err != nil || res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
		t.Fatalf("/: %v %v", res, err)
	}
	res, err = noFollow.Get(site.URL + "/robots.txt")
	b, _ := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 || string(b) != "User-agent: *\nDisallow: /\n" {
		t.Errorf("/robots.txt: %v %q", res.StatusCode, b)
	}
	for _, p := range []string{"/legal/privacy.html", "/img/city.jpg", "/guides", "/sitemap.xml", "/it"} {
		if res, _ := noFollow.Get(site.URL + p); res.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, res.StatusCode)
		}
	}
}

// A site without releases of its own passes on its upstream's (Artide's by default): the version, the download and
// the appcast all lead to the upstream, and with RUSHBIN_UPDATES_FROM=off it offers none.
func TestUpdatesFrom(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"version":"1.4.0","mac":{"file":"rushbin-1.4.0.dmg","sha256":"ab","size":5,"edSignature":"c2ln"}}`))
	}))
	defer up.Close()
	site := httptest.NewServer((&Server{upstream: up.URL}).routes())
	defer site.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var rel Release
	if err := (&Team{URL: site.URL}).call("GET", "/api/version", nil, &rel); err != nil || rel.Version != "1.4.0" {
		t.Fatalf("/api/version: %+v %v", rel, err)
	}
	if res, err := noFollow.Get(site.URL + "/download/mac"); err != nil || res.StatusCode != 302 || res.Header.Get("Location") != up.URL+"/download/mac" {
		t.Errorf("/download/mac: %v %v", res, err)
	}
	res, err := http.Get(site.URL + "/appcast.xml")
	b, _ := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(b), `url="`+up.URL+`/download/mac"`) || !strings.Contains(string(b), `sparkle:edSignature="c2ln"`) {
		t.Errorf("/appcast.xml: %d %s", res.StatusCode, b)
	}
	none := httptest.NewServer((&Server{}).routes())
	defer none.Close()
	for _, p := range []string{"/api/version", "/appcast.xml", "/download/mac"} {
		if res, _ := noFollow.Get(none.URL + p); res.StatusCode != 404 {
			t.Errorf("no upstream, %s: %d, want 404", p, res.StatusCode)
		}
	}
	for v, want := range map[string]string{"": artideSite, "off": "", "https://updates.example.com/": "https://updates.example.com"} {
		if t.Setenv("RUSHBIN_UPDATES_FROM", v); updatesFrom() != want {
			t.Errorf("RUSHBIN_UPDATES_FROM=%q: %q, want %q", v, updatesFrom(), want)
		}
	}
}

// Every page and file of the site tells browsers to check it again, so an update shows at once.
func TestNoCache(t *testing.T) {
	s := &Server{}
	site := httptest.NewServer(s.guard(s.routes()))
	defer site.Close()
	for _, p := range []string{"/login", "/i18n.js", "/logo.svg", "/robots.txt"} {
		res, err := http.Get(site.URL + p)
		if err != nil || res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %v %v %q", p, res.StatusCode, err, res.Header.Get("Cache-Control"))
		}
	}
}
