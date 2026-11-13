package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// BenchmarkScale measures the catalog and the app's pages on a synthetic archive of 100,000 clips on 40 drives,
// written like the real catalog folder: about a fifth of the contents on more than one drive, some decisions
// and looks. go test -run '^$' -bench Scale -benchmem
func BenchmarkScale(b *testing.B) {
	dir := b.TempDir()
	const drives, perDrive = 40, 2500
	writeArchive(b, dir, drives, perDrive)
	b.Run("loadCatalog", func(b *testing.B) {
		for b.Loop() {
			if _, err := loadCatalog(dir, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	app := &App{dir: dir, mounted: map[string]string{}, seen: map[string]string{}, ignored: map[string]bool{}}
	if err := app.reload(); err != nil {
		b.Fatal(err)
	}
	if n := len(app.catalog().list); n != drives*perDrive {
		b.Fatalf("%d clips in the catalog, want %d", n, drives*perDrive)
	}
	h := app.routes()
	for _, path := range []string{"/api/overview", "/api/clips", "/api/clips?sort=size", "/api/clips?flag=undecided",
		"/api/search?q=interview", "/api/dups", "/api/compare?a=drive-00&b=drive-01"} {
		b.Run(strings.TrimPrefix(path, "/api/"), func(b *testing.B) {
			for b.Loop() {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != 200 {
					b.Fatalf("%s: %d %s", path, w.Code, w.Body)
				}
			}
		})
	}
}

// writeArchive writes device.json and files.jsonl per drive, decisions.jsonl and vision.jsonl, like a real catalog.
func writeArchive(b *testing.B, dir string, drives, perDrive int) {
	r := rand.New(rand.NewPCG(1, 2))
	projects := make([]string, 80)
	for i := range projects {
		projects[i] = fmt.Sprintf("%s_%02d", []string{"Wedding", "Commercial", "Interview", "Documentary", "Event"}[i%5], i)
	}
	cams := []struct{ dir, ext, codec string }{{"A_CAM", ".mov", "prores"}, {"B_CAM", ".mp4", "h264"}, {"DRONE", ".mp4", "hevc"},
		{"FX6", ".mxf", "xavc"}, {"GOPRO", ".mp4", "h264"}}
	var shared []Rec // contents copied to other drives
	var hashes []string
	day := time.Date(2020, 1, 1, 9, 0, 0, 0, time.UTC)
	for d := range drives {
		uuid := fmt.Sprintf("drive-%02d", d)
		mount := "/Volumes/" + uuid
		folder := filepath.Join(dir, "disks", uuid)
		check(b, os.MkdirAll(folder, 0o755))
		dev, _ := json.Marshal(Device{UUID: uuid, Name: fmt.Sprintf("Drive %02d", d), Mount: mount, FS: "exfat", Size: 4 << 40,
			Scanned: day.AddDate(0, 0, d).Format(time.RFC3339)})
		check(b, os.WriteFile(filepath.Join(folder, "device.json"), dev, 0o644))
		f, err := os.Create(filepath.Join(folder, "files.jsonl"))
		check(b, err)
		w := bufio.NewWriter(f)
		enc := json.NewEncoder(w)
		for i := range perDrive {
			var rec Rec
			if len(shared) > 0 && r.IntN(5) == 0 { // a copy of a clip from an earlier drive, in another folder
				rec = shared[r.IntN(len(shared))]
				rec.Path = filepath.Join(mount, "Backup", rec.Path[len("/Volumes/drive-00"):])
			} else {
				p, c := projects[r.IntN(len(projects))], cams[r.IntN(len(cams))]
				t := day.Add(time.Duration(r.IntN(6*365*24)) * time.Hour)
				dur := 2 + r.Float64()*600
				rec = Rec{Path: filepath.Join(mount, p, t.Format("2006-01-02"), c.dir, fmt.Sprintf("C%04d%s", i, c.ext)), Ext: c.ext,
					Hash: fmt.Sprintf("%016x", r.Uint64()), Size: int64(dur * float64(5+r.IntN(80)) * 1e6 / 8), Mtime: t.Format(time.RFC3339),
					Dur: dur, W: 3840, H: 2160, Codec: c.codec, FPS: "25/1", Creation: t.Format(time.RFC3339Nano), PixFmt: "yuv422p10le"}
				if r.IntN(3) == 0 {
					rec.W, rec.H = 1920, 1080
				}
				hashes = append(hashes, rec.Hash)
				if r.IntN(4) == 0 {
					shared = append(shared, rec)
				}
			}
			check(b, enc.Encode(rec))
		}
		check(b, w.Flush())
		check(b, f.Close())
	}
	lines := func(name string, n int, line func(i int) any) {
		f, err := os.Create(filepath.Join(dir, name))
		check(b, err)
		w := bufio.NewWriter(f)
		enc := json.NewEncoder(w)
		for i := range n {
			check(b, enc.Encode(line(i)))
		}
		check(b, w.Flush())
		check(b, f.Close())
	}
	lines("decisions.jsonl", len(hashes)/10, func(i int) any {
		return DecisionLine{T: day.Format(time.RFC3339), Content: "c:" + hashes[r.IntN(len(hashes))], Kind: "vote",
			Value: []string{"keep", "discard", "showreel"}[r.IntN(3)]}
	})
	labels := []string{"person", "sky", "car", "building", "tree", "water", "stage", "crowd"}
	lines("vision.jsonl", len(hashes)/3, func(i int) any {
		fr := []Frame{{Contrast: r.Float64() * 0.2, Labels: map[string]float64{labels[r.IntN(len(labels))]: r.Float64(), labels[r.IntN(len(labels))]: r.Float64()}}}
		return map[string]any{"thumb": notAlnum.ReplaceAllString("c:"+hashes[i*3], "_"), "frames": fr}
	})
}

func check(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}

// BenchmarkScaleSite measures the site building a team's catalog of the same archive from Postgres
// (RUSHBIN_TEST_DB, as for TestServer).
func BenchmarkScaleSite(b *testing.B) {
	admin := os.Getenv("RUSHBIN_TEST_DB")
	if admin == "" {
		b.Skip("RUSHBIN_TEST_DB not set")
	}
	dir := b.TempDir()
	writeArchive(b, dir, 40, 2500)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	check(b, err)
	name := "rushbin_scale_" + token()[:8]
	_, err = conn.Exec(ctx, "create database "+name)
	check(b, err)
	defer func() { conn.Exec(ctx, "drop database "+name+" with (force)"); conn.Close(ctx) }()
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	b.Setenv("RUSHBIN_APP_PASSWORD", token())
	s, err := openServer(ctx, u.String(), b.TempDir())
	check(b, err)
	defer s.db.Close()
	defer s.admin.Close()
	_, err = newTeam(ctx, s, "Scale", "s@scale.test")
	check(b, err)
	var team int64
	check(b, s.admin.QueryRow(ctx, "select id from team where name = 'Scale'").Scan(&team))
	folders, _ := filepath.Glob(filepath.Join(dir, "disks", "*"))
	check(b, s.inTeam(ctx, team, func(tx pgx.Tx) error {
		for _, f := range folders {
			dev, _ := os.ReadFile(filepath.Join(f, "device.json"))
			files, _ := os.ReadFile(filepath.Join(f, "files.jsonl"))
			if _, err := tx.Exec(ctx, "insert into drive (team, uuid, device, files, fullhash) values ($1, $2, $3, $4, '')",
				team, filepath.Base(f), dev, files); err != nil {
				return err
			}
		}
		return nil
	}))
	for b.Loop() {
		s.mu.Lock()
		delete(s.cats, team)
		s.mu.Unlock()
		c, err := s.catalog(ctx, team)
		check(b, err)
		if len(c.list) != 100000 {
			b.Fatalf("%d clips", len(c.list))
		}
	}
}
