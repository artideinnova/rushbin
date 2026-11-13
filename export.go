package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// exportFormat is the version of the export's layout, in its manifest.json: it goes up when a file changes meaning.
const exportFormat = 1

const exportReadme = `rushbin: everything the team site holds of your team
=====================================================

This archive was made by the team site on the day in manifest.json. manifest.json lists every
file with what it holds; this is the short version.

drives/<drive>/      One folder per drive the team's computers sent: device.json (its name, size,
                     file system, folders skipped, when it was cataloged), files.jsonl (one line per
                     file: path, size, dates, duration, codec, frame size, and the hash that
                     identifies its content) and fullhash.tsv (full SHA-256 of files checked byte
                     for byte), exactly as the computer sent them.
previews/<thumb>/    The 5 preview frames of each clip (JPEG). <thumb> names a content: "c:<hash>"
                     of files.jsonl with every character other than a letter or digit as "_".
decisions.jsonl      Every decision (keep, discard, showreel, copy to keep), who took it and when.
looks.jsonl          What Apple Vision saw in the previews: labels, words on screen, contrast.
speech.jsonl         What is said in the clips, with start and end in seconds.
frames.jsonl         The search vectors of the previews (768 numbers each).
originals.jsonl      The originals the team asked for, who asked, and what became of them.
removed.jsonl        Clips whose data was removed on request, by whom and when.
members.jsonl        The members and their roles. No passwords and no sessions.
light-copies.jsonl   The light copies (H.264 video): they are not in this archive, which would be
                     too big; each line has a link to download it, valid 7 days from the export.

Files ending in .jsonl hold one JSON object per line. Times are UTC, in ISO 8601.
`

// exportTeam streams the team's data to its owner as a ZIP, made as it is sent: nothing of it is copied to disk.
// An error once the download has started aborts it, so a broken archive never looks complete.
func (s *Server) exportTeam(w http.ResponseWriter, r *http.Request, u *User) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="rushbin-export-`+time.Now().UTC().Format("2006-01-02")+`.zip"`)
	if err := s.export(r.Context(), u, w, time.Now()); err != nil {
		log.Printf("export of team %d: %v", u.Team, err)
		panic(http.ErrAbortHandler)
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9-]`)

type exported struct {
	Path  string `json:"path"`
	About string `json:"about"`
	Count int    `json:"count,omitempty"` // files of this pattern, or lines of this file
}

func (s *Server) export(ctx context.Context, u *User, w io.Writer, now time.Time) error {
	z := zip.NewWriter(w)
	var files []exported
	create := func(path, about string, method uint16) (io.Writer, int, error) { // the writer, and the file's place in files
		f, err := z.CreateHeader(&zip.FileHeader{Name: path, Method: method, Modified: now})
		files = append(files, exported{Path: path, About: about})
		return f, len(files) - 1, err
	}
	f, _, err := create("README.txt", "This archive in short, in plain English.", zip.Deflate)
	if err == nil {
		_, err = io.WriteString(f, exportReadme)
	}
	if err != nil {
		return err
	}

	// Drives: their catalog files, one drive at a time (a catalog can be a few hundred MB).
	type drive struct {
		Folder string `json:"folder"`
		UUID   string `json:"uuid"`
		Name   string `json:"name"`
	}
	drives := []drive{}
	err = s.inTeam(ctx, u.Team, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, "select uuid, device::text, files, fullhash from drive where team = $1 order by uuid", u.Team)
		for rows.Next() {
			var id, dev string
			var files, full []byte
			if err := rows.Scan(&id, &dev, &files, &full); err != nil {
				return err
			}
			var d struct{ Name string }
			json.Unmarshal([]byte(dev), &d)
			folder := "drives/" + unsafeName.ReplaceAllString(id, "_") // a uuid is whatever a computer sent
			drives = append(drives, drive{folder, id, d.Name})
			for name, b := range map[string][]byte{"device.json": []byte(dev), "files.jsonl": files, "fullhash.tsv": full} {
				f, err := z.CreateHeader(&zip.FileHeader{Name: folder + "/" + name, Method: zip.Deflate, Modified: now})
				if err == nil {
					_, err = f.Write(b)
				}
				if err != nil {
					return err
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	files = append(files,
		exported{"drives/<drive>/device.json", "The drive as its computer described it: uuid, name, mount, file system, bus, media, size, free, scanned (when), excluded (folders skipped), and the choices made for it. <drive> is its uuid; the list is under drives here.", len(drives)},
		exported{"drives/<drive>/files.jsonl", "One line per file of the drive, as the computer sent it: Path, Ext, Size, Mtime, Dur (seconds), W, H, Codec, FPS, Creation, Bitrate, PixFmt, Span (a clip split in files: the size of all of them) and Hash, the sampled hash that identifies the content (c:<Hash> in decisions.jsonl and originals.jsonl).", len(drives)},
		exported{"drives/<drive>/fullhash.tsv", "The full SHA-256 of the files checked byte for byte, one per line: SHA-256, path, size, modification time (Unix seconds), separated by tabs.", len(drives)})

	// The team's rows, one JSON object per line, as Postgres writes them.
	for _, t := range []struct{ path, about, q string }{
		{"members.jsonl", "The team's members: person (their number on this site), name, email, role (owner, editor or viewer), joined. No password hashes and no sessions.",
			"select p.id as person, p.name, p.email, m.role, m.created as joined from member m join person p on p.id = m.person where m.team = $1 order by p.id"},
		{"decisions.jsonl", "Every decision, oldest first: id, t (when), content (c:<Hash> of files.jsonl), kind (vote, whose value is keep, discard, showreel or empty for none; or copy, whose value is the copy to keep as <drive uuid>|<path>), value, by (the name of who took it, at the time), person (their number in members.jsonl; empty when their account is gone).",
			"select id, t, content, kind, value, by_name as by, person from decision where team = $1 order by id"},
		{"looks.jsonl", "What Apple Vision saw, one line per content: thumb (its folder under previews), frames (one per preview: contrast, labels with their confidence from 0 to 1, text read on screen), generator (the model) and ai_generated (true: made by an AI model, see docs/ai.md).",
			"select thumb, frames, '" + lookGenerator + "' as generator, true as ai_generated from look where team = $1 order by thumb"},
		{"speech.jsonl", "What is said, one line per content: thumb, segs (s and e: start and end in seconds, t: the words), generator (the model) and ai_generated (true: an automatic transcript, it may contain errors).",
			"select thumb, segs, '" + speechGenerator + "' as generator, true as ai_generated from speech where team = $1 order by thumb"},
		{"frames.jsonl", "The search vectors of the previews (SigLIP 2), one line per preview: thumb, k (the preview, 1 to 5), v (768 numbers, kept as 16-bit floats).",
			"select thumb, k, v::text::json as v from frame where team = $1 order by thumb, k"},
		{"originals.jsonl", "Originals asked for: id, device (the drive's uuid), path (on the drive), content, size (bytes), by, person, state (waiting, sending, ready, failed or expired), sha256 (as the computer that sent it computed it), verified, note, created, updated. The files are not here: the store keeps each for 7 days.",
			"select id, device, rel as path, content, size, by_name as by, person, state, sha256, verified, note, created, updated from request where team = $1 order by id"},
		{"removed.jsonl", "Contents whose data was removed on request: thumb, by (who removed it), person, t (when). The site keeps this record so that no computer sends that data again.",
			"select thumb, by_name as by, person, t from forgotten where team = $1 order by thumb"},
	} {
		f, i, err := create(t.path, t.about, zip.Deflate)
		if err != nil {
			return err
		}
		err = s.inTeam(ctx, u.Team, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, "select row_to_json(x)::text from ("+t.q+") x", u.Team)
			for rows.Next() {
				var line []byte
				if err := rows.Scan(&line); err != nil {
					return err
				}
				if _, err := f.Write(append(line, '\n')); err != nil {
					return err
				}
				files[i].Count++
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
	}

	// Light copies: a link each, not the videos.
	f, i, err := create("light-copies.jsonl", "The light copies (H.264, 1280 px, about 3 Mbit/s), one line per content: thumb, size (bytes), created, url (to download it, valid until expires), expires.", zip.Deflate)
	if err != nil {
		return err
	}
	until := now.Add(7 * 24 * time.Hour) // the longest a signed link can last
	err = s.inTeam(ctx, u.Team, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, "select thumb, size, created from proxy where team = $1 order by thumb", u.Team)
		for rows.Next() {
			var p struct {
				Thumb   string    `json:"thumb"`
				Size    int64     `json:"size"`
				Created time.Time `json:"created"`
				URL     string    `json:"url,omitempty"`
				Expires time.Time `json:"expires"`
			}
			if err := rows.Scan(&p.Thumb, &p.Size, &p.Created); err != nil {
				return err
			}
			if s.store != nil {
				p.URL, p.Expires = s.store.presign("GET", proxyKey(u.Team, p.Thumb), until.Sub(now), now), until.UTC()
			}
			line, _ := json.Marshal(p)
			if _, err := f.Write(append(line, '\n')); err != nil {
				return err
			}
			files[i].Count++
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}

	// Previews, from disk; JPEGs are compressed already.
	n := 0
	es, _ := os.ReadDir(filepath.Join(s.dir, "teams", strconv.FormatInt(u.Team, 10), "thumbs"))
	for _, d := range es {
		for k := 1; k <= 5; k++ {
			name := fmt.Sprintf("%d.jpg", k)
			src, err := os.Open(filepath.Join(s.thumbDir(u.Team, d.Name()), name))
			if err != nil {
				continue // not sent yet, or removed meanwhile
			}
			f, err := z.CreateHeader(&zip.FileHeader{Name: "previews/" + d.Name() + "/" + name, Method: zip.Store, Modified: now})
			if err == nil {
				_, err = io.Copy(f, src)
			}
			src.Close()
			if err != nil {
				return err
			}
			n++
		}
	}
	files = append(files, exported{"previews/<thumb>/<1 to 5>.jpg", "The 5 previews of each content, at 10, 30, 50, 70 and 90% of the clip, 480 px on the longest side. <thumb> is c:<Hash> of files.jsonl with every character other than a letter or digit as _.", n})

	f, _, err = create("manifest.json", "This list.", zip.Deflate)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"format": exportFormat, "team": u.TeamName, "made": now.UTC(), "drives": drives, "files": files}); err != nil {
		return err
	}
	return z.Close()
}
