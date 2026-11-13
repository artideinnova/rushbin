package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Search by content: one search box finds clips by their names and folders, by what Apple Vision read on
// screen and recognized, by what is said (Whisper), and by what they look like (SigLIP 2: the Macs embed
// the previews, the site embeds the words of the search with the same model). The lists are merged with
// reciprocal rank fusion: a clip found by words counts as a first place; one also seen counts twice.

// Hit is a content found by how it looks, with the similarity of its best frame.
type Hit struct {
	Thumb string
	Score float64
}

const rrfK = 60 // the usual constant of reciprocal rank fusion: places far down the list weigh little

// Search merges the clips found by words (q.Words, all of them, in any field) with the ones found by looks.
func (c *Catalog) Search(q Query, seen []Hit) []Row {
	score := map[string]float64{}
	var order []string
	add := func(content string, s float64) {
		if _, ok := score[content]; !ok {
			order = append(order, content)
		}
		score[content] += s
	}
	rows := map[string]Row{}
	if len(q.Words) > 0 {
		for _, r := range c.Clips(q) {
			rows[r.Content] = r
			add(r.Content, 1.0/rrfK)
		}
	}
	filters := q
	filters.Words = nil
	byThumb := map[string]string{}
	for _, cl := range c.list {
		byThumb[cl.Thumb] = cl.Content
	}
	for i, h := range seen {
		content, ok := byThumb[h.Thumb]
		if !ok {
			continue
		}
		if _, ok := rows[content]; !ok {
			best := c.bestCopy(content, filters)
			if best == nil {
				continue
			}
			r := c.row(best, nil)
			r.Seen = true
			rows[content] = r
		} else {
			r := rows[content]
			r.Seen = true
			rows[content] = r
		}
		add(content, 1.0/float64(rrfK+i+1))
	}
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	out := make([]Row, 0, len(order))
	for _, content := range order {
		if r, ok := rows[content]; ok {
			out = append(out, r)
		}
	}
	if len(out) > 300 {
		out = out[:300]
	}
	return out
}

// bestCopy is the copy of a content shown in results (the one to keep, else the shortest path), among
// the copies that pass the filters.
func (c *Catalog) bestCopy(content string, q Query) *Clip {
	var best *Clip
	for _, cl := range c.byCont[content] {
		if c.match(cl, q) && (best == nil || c.preferred(cl, best)) {
			best = cl
		}
	}
	return best
}

// textEncoder turns the words of a search into a SigLIP 2 vector (deploy/text: the text half of the model,
// on the server's CPU, about 20 ms a search).
func (s *Server) embedText(ctx context.Context, q string) ([]float32, error) {
	if s.textURL == "" {
		return nil, nil
	}
	b, _ := json.Marshal(map[string]string{"q": q})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.textURL+"/embed", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct{ V []float32 }
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || len(out.V) != embDim {
		return nil, fmt.Errorf("text encoder: bad answer (%v)", err)
	}
	return out.V, nil
}

const embDim = 768

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// looksLike finds the contents whose frames are nearest to the search, best frame per content.
func (s *Server) looksLike(ctx context.Context, team int64, q string) ([]Hit, error) {
	v, err := s.embedText(ctx, q)
	if err != nil || v == nil {
		return nil, err
	}
	var hits []Hit
	err = s.inTeam(ctx, team, func(tx pgx.Tx) error {
		// With the HNSW index, the team filter applies after it: without iterative scans a query would see only
		// the nearest 40 frames of all teams (hnsw.ef_search) and keep its team's among them. Today Postgres
		// sorts the team's frames instead (EXPLAIN on 12 November 2026); this keeps 300 when it switches.
		if _, err := tx.Exec(ctx, "set local hnsw.iterative_scan = strict_order"); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `select thumb, 1 - (v <=> $2::halfvec) from frame where team = $1
			order by v <=> $2::halfvec limit 300`, team, vectorLiteral(v))
		seen := map[string]bool{}
		for rows.Next() {
			var h Hit
			if err := rows.Scan(&h.Thumb, &h.Score); err != nil {
				return err
			}
			if !seen[h.Thumb] && len(hits) < 100 {
				seen[h.Thumb] = true
				hits = append(hits, h)
			}
		}
		return rows.Err()
	})
	return hits, err
}

// search answers the search box on the site: words and looks together.
func (s *Server) search(w http.ResponseWriter, r *http.Request, u *User) {
	c, err := s.catalog(r.Context(), u.Team)
	if err != nil {
		fail(w, err)
		return
	}
	q := queryOf(r)
	seen, err := s.looksLike(r.Context(), u.Team, r.URL.Query().Get("q"))
	if err != nil {
		log.Printf("search by looks: %v", err) // words still work
	}
	send(w, c.Search(q, seen))
}

func queryOf(r *http.Request) Query {
	q := r.URL.Query()
	return Query{Words: strings.Fields(q.Get("q")), Project: q.Get("project"), Device: q.Get("device"), Camera: q.Get("camera"),
		Folder: q.Get("folder"), Label: q.Get("label"), Flags: q["flag"], Sort: q.Get("sort"), All: q.Get("all") == "1"}
}

// half decodes IEEE 754 binary16, the format of emb.bin.
func half(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h & 0x3ff)
	switch {
	case exp == 0 && frac == 0:
		return math.Float32frombits(sign)
	case exp == 0: // subnormal
		for frac&0x400 == 0 {
			frac <<= 1
			exp--
		}
		exp++
		frac &= 0x3ff
	case exp == 0x1f:
		return math.Float32frombits(sign | 0xff<<23 | frac<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | frac<<13)
}

// frames splits emb.bin into one vector per frame.
func frames(b []byte) ([][]float32, error) {
	if len(b) == 0 || len(b)%(embDim*2) != 0 {
		return nil, fmt.Errorf("embeddings of %d bytes", len(b))
	}
	var out [][]float32
	for off := 0; off < len(b); off += embDim * 2 {
		v := make([]float32, embDim)
		for i := range v {
			v[i] = half(binary.LittleEndian.Uint16(b[off+2*i:]))
		}
		out = append(out, v)
	}
	return out, nil
}

// putEmb stores the frame embeddings a Mac made, one line per content: {"thumb", "emb": base64 of emb.bin}.
func (s *Server) putEmb(w http.ResponseWriter, r *http.Request, u *User) {
	ok := s.syncRows(w, r, u, "bad embedding line", func(dec *json.Decoder, b *pgx.Batch) error {
		var v struct{ Thumb, Emb string }
		if err := dec.Decode(&v); err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(v.Emb)
		if err != nil || !thumbName.MatchString(v.Thumb) {
			return errBadLine
		}
		fs, err := frames(raw)
		if err != nil || len(fs) > 5 {
			return errBadLine
		}
		b.Queue("delete from frame where team = $1 and thumb = $2", u.Team, v.Thumb)
		for k, f := range fs {
			b.Queue(`insert into frame (team, thumb, k, v) select $1, $2, $3, $4::halfvec
				where not exists (select 1 from forgotten where team = $1 and thumb = $2)`, u.Team, v.Thumb, k+1, vectorLiteral(f))
		}
		return nil
	})
	if ok {
		send(w, map[string]bool{"ok": true})
	}
}

// search in the app: with a team, the site answers (it has every Mac's embeddings); alone, the same search
// here: words, and looks with the text model of this Mac (text.go; on Windows words only for now).
func (a *App) search(w http.ResponseWriter, r *http.Request) {
	if t := a.teamConf(); t != nil {
		var rows []Row
		if err := t.call("GET", "/api/search?"+r.URL.RawQuery, nil, &rows); err == nil {
			send(w, rows)
			return
		}
	}
	c, embed := a.catalog(), a.text
	if embed == nil {
		embed = a.embedLocal
	}
	var seen []Hit
	if q := r.URL.Query().Get("q"); strings.TrimSpace(q) != "" {
		v, err := embed(q)
		if err != nil {
			log.Printf("search by looks: %v", err) // words still work
		} else if v != nil {
			seen = c.nearest(filepath.Join(a.dir, "thumbs"), v)
		}
	}
	send(w, c.Search(queryOf(r), seen))
}
