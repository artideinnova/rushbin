package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Originals from anywhere: a member asks for the original of a clip whose drive is connected to another
// team Mac; that Mac uploads it straight to the team's store, hashing it as it reads, and the site gives a
// download link for 7 days. Each Mac tells the site every 15 seconds which drives it has connected.

const (
	onlineFor    = 45 * time.Second    // a drive counts as connected this long after its Mac last said so
	originalKeep = 7 * 24 * time.Hour  // then the copy in the store is deleted
	requestKeep  = 90 * 24 * time.Hour // then the request itself, and anything of it still in the store
	maxObject    = 5 << 40             // R2's largest object, 5 TiB
	maxParts     = 10000               // R2's most parts in one multipart upload
)

// One PUT up to 5 GiB (R2's limit); over it, a multipart upload of parts of partSize, more when a file
// needs over 10,000 parts. R2 wants every part but the last the same size, from 5 MiB to 5 GiB
// (developers.cloudflare.com/r2/objects/multipart-objects). Variables so the tests can use small files.
var maxPut, partSize int64 = 5 << 30, 64 << 20

// partsOf: how many parts a file of size bytes is sent in, and the size of each but the last.
func partsOf(size int64) (n, each int64) {
	each = max(partSize, (size+maxParts-1)/maxParts)
	return (size + each - 1) / each, each
}

// online remembers, per team, which Mac has which drive connected.
type online struct {
	mu   sync.Mutex
	seen map[int64]map[string]onlineMac // team → drive → Mac
}

type onlineMac struct {
	name string
	at   time.Time
}

func (o *online) set(team int64, mac string, drives []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[int64]map[string]onlineMac{}
	}
	if o.seen[team] == nil {
		o.seen[team] = map[string]onlineMac{}
	}
	for _, d := range drives {
		o.seen[team][d] = onlineMac{mac, time.Now()}
	}
}

// at: the drives of a team connected now, and the Mac each is connected to.
func (o *online) at(team int64) map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := map[string]string{}
	for d, v := range o.seen[team] {
		if time.Since(v.at) < onlineFor {
			m[d] = v.name
		}
	}
	return m
}

// setOnlineAt shows, in the catalog the site serves, where each drive is connected.
func (c *Catalog) setOnlineAt(m map[string]string) {
	c.dmu.Lock()
	c.onlineAt = m
	c.dmu.Unlock()
}

func (c *Catalog) macOf(device string) string {
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	return c.onlineAt[device]
}

// putOnline: a Mac says which drives it has connected.
func (s *Server) putOnline(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct{ Drives []string }
	if json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&b) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.online.set(u.Team, u.Device, b.Drives)
	send(w, map[string]bool{"ok": true})
}

// Request is one original asked for.
type Request struct {
	ID     int64  `json:"id"`
	Device string `json:"device"`
	Rel    string `json:"id_in_drive"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	By     string `json:"by"`
	State  string `json:"state"`            // waiting | sending | ready | failed | expired
	SHA256 string `json:"sha256,omitempty"` // as the Mac that sent it computed it
	Note   string `json:"note,omitempty"`
	URL    string `json:"url,omitempty"` // the download link, when ready
	Mac    string `json:"mac,omitempty"` // where the drive is connected now
}

func originalKey(team, id int64, name string) string {
	return fmt.Sprintf("teams/%d/originals/%d/%s", team, id, url.PathEscape(name))
}

// askOriginal: a member asks for the original of one copy.
func (s *Server) askOriginal(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct{ Device, ID string }
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	c, err := s.catalog(r.Context(), u.Team)
	if err != nil {
		fail(w, err)
		return
	}
	cl := c.byKey[b.Device+"|"+b.ID]
	if cl == nil || s.store == nil {
		http.Error(w, "This clip is not in the catalog, or the site keeps no files", 404)
		return
	}
	if cl.Size > maxObject {
		http.Error(w, "Originals over 5 TB cannot be sent", 400)
		return
	}
	var id int64
	err = s.syncing(r.Context(), u.Team, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `insert into request (team, device, rel, content, size, person, by_name)
			select $1, $2, $3, $4, $5, $6, $7 where not exists (select 1 from forgotten where team = $1 and thumb = $8) returning id`,
			u.Team, cl.Device, cl.ID, cl.Content, cl.Size, u.ID, u.Name, cl.Thumb).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "This clip's data was removed from the team site on request", 410)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]int64{"id": id})
}

func (s *Server) requests(r *http.Request, team int64, where string, args ...any) ([]Request, error) {
	var out []Request
	err := s.inTeam(r.Context(), team, func(tx pgx.Tx) error {
		rows, _ := tx.Query(r.Context(), `select id, device, rel, size, by_name, state, coalesce(sha256, ''), coalesce(note, '')
			from request where team = $1 and `+where+` order by id`, append([]any{team}, args...)...)
		for rows.Next() {
			var q Request
			if err := rows.Scan(&q.ID, &q.Device, &q.Rel, &q.Size, &q.By, &q.State, &q.SHA256, &q.Note); err != nil {
				return err
			}
			q.Name = filepath.Base(q.Rel)
			out = append(out, q)
		}
		return rows.Err()
	})
	return out, err
}

// showOriginal: where a request is; when ready, with its download link (one hour, renewed on each look).
func (s *Server) showOriginal(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	qs, err := s.requests(r, u.Team, "id = $2", id)
	if err != nil || len(qs) == 0 {
		http.Error(w, "No such request", 404)
		return
	}
	q := qs[0]
	q.Mac = s.online.at(u.Team)[q.Device]
	if q.State == "ready" {
		q.URL = s.store.presign("GET", originalKey(u.Team, q.ID, q.Name), time.Hour, time.Now(),
			[2]string{"response-content-disposition", `attachment; filename="` + strings.ReplaceAll(q.Name, `"`, "") + `"`})
	}
	send(w, q)
}

// pendingFor: the requests a Mac can serve now, for the drives it has connected.
func (s *Server) pendingFor(w http.ResponseWriter, r *http.Request, u *User) {
	drives := strings.Split(r.URL.Query().Get("drives"), ",")
	qs, err := s.requests(r, u.Team, "state = 'waiting' and device = any($2)", drives)
	if err != nil {
		fail(w, err)
		return
	}
	if qs == nil {
		qs = []Request{}
	}
	send(w, qs)
}

// Upload is where a Mac sends one original: one link, or the multipart upload and one link per part.
type Upload struct {
	URL      string   `json:"url,omitempty"`
	UploadID string   `json:"upload_id,omitempty"`
	Parts    []string `json:"parts,omitempty"`
}

// claim: one Mac takes a request (once) and gets where to upload it. Starting the multipart upload is
// part of taking it: when the store does not start it, the request stays waiting.
func (s *Server) claim(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var up Upload
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		var rel string
		var size int64
		if err := tx.QueryRow(r.Context(), `update request set state = 'sending', updated = now() where team = $1 and id = $2 and state = 'waiting' returning rel, size`,
			u.Team, id).Scan(&rel, &size); err != nil {
			return err
		}
		key := originalKey(u.Team, id, filepath.Base(rel))
		if size <= maxPut {
			up.URL = s.store.presign("PUT", key, 6*time.Hour, time.Now())
			return nil
		}
		var err error
		if up.UploadID, err = s.store.createMultipart(r.Context(), key); err != nil {
			return err
		}
		n, _ := partsOf(size)
		for i := range n {
			// ponytail: every part link is signed now for 24 hours; a line that needs longer fails the
			// last parts, and links signed on demand would lift it.
			up.Parts = append(up.Parts, s.store.presign("PUT", key, 24*time.Hour, time.Now(),
				[2]string{"partNumber", fmt.Sprint(i + 1)}, [2]string{"uploadId", up.UploadID}))
		}
		return nil
	})
	if err != nil {
		http.Error(w, "Already taken, or no such request, or the store did not start the upload", 409)
		return
	}
	send(w, up)
}

// sent: the Mac uploaded it (or failed); the site joins the parts of a multipart upload, or drops them on a
// failure, and checks the size of the object in the store against the catalog's. The hash is the Mac's
// word, so it is shown and not checked: R2 keeps no SHA-256 of a whole multipart object, only
// "COMPOSITE" ones of the parts, and HeadObject returns no checksum (developers.cloudflare.com/r2/api/s3/api).
func (s *Server) sent(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var b struct {
		SHA256   string
		Error    string
		UploadID string   `json:"upload_id"`
		ETags    []string `json:"etags"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b)
	qs, err := s.requests(r, u.Team, "id = $2", id)
	if err != nil || len(qs) == 0 || qs[0].State != "sending" {
		if len(qs) == 1 && qs[0].State == "expired" { // removed on request while it was being sent: what arrived goes
			if b.UploadID != "" {
				s.store.abortMultipart(r.Context(), originalKey(u.Team, id, qs[0].Name), b.UploadID)
			}
			s.deleteObjects(r.Context(), []string{originalKey(u.Team, id, qs[0].Name)})
		}
		http.Error(w, "No such request being sent", 404)
		return
	}
	key := originalKey(u.Team, id, qs[0].Name)
	if b.UploadID != "" && b.Error == "" {
		if err := s.store.completeMultipart(r.Context(), key, b.UploadID, b.ETags); err != nil {
			b.Error = "The store did not join the parts: " + err.Error()
		}
	}
	if b.UploadID != "" && b.Error != "" {
		s.store.abortMultipart(r.Context(), key, b.UploadID)
	}
	state, note := "ready", b.Error
	if b.Error != "" {
		state = "failed"
	} else {
		res, err := s.store.do(r.Context(), "HEAD", key, nil)
		if err == nil {
			res.Body.Close()
		}
		if err != nil || res.ContentLength != qs[0].Size {
			state, note = "failed", "The file in the store is not complete"
		}
	}
	err = s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `update request set state = $3, sha256 = nullif($4, ''), note = nullif($5, ''), updated = now()
			where team = $1 and id = $2`, u.Team, id, state, b.SHA256, note)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	send(w, map[string]string{"state": state})
}

// expire applies the site's terms every hour (sweep).
func (s *Server) expire() {
	for {
		time.Sleep(time.Hour)
		s.sweep(context.Background(), time.Now())
	}
}

// sweep deletes what is past its term at now, each counted from its last use: browser sessions after 30 days and
// computers' tokens after 90 (deviceSession), invite and password links after 7 (password links sent by email
// after an hour, mailedLink), pairing codes after 10 minutes (pairTTL), sign-ups not confirmed after 7, an original in the store 7 days
// after its request last moved (also when the upload failed or never ended) and the request itself after 90
// (requestKeep), with whatever of it is still in the store; failed sign-ins after 15 minutes; tidy-ups with AI
// counted in a past month. Deleted, not just ignored: these are the privacy policy's terms.
func (s *Server) sweep(ctx context.Context, now time.Time) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	links := ago(linkDays * 24 * time.Hour)
	for q, at := range map[string][]any{
		"delete from session where seen < case when device = '' then $1::timestamptz else $2::timestamptz end": {ago(webSession), ago(deviceSession)},
		"delete from invite where created < $1": {links},
		"delete from reset where created < case when mailed then $1::timestamptz else $2::timestamptz end": {ago(mailedLink), links},
		"delete from pairing where created < $1":                                                           {ago(pairTTL)},
		"delete from tidy_run where t < date_trunc('month', $1::timestamptz)":                              {now}, // only this month's count matters
	} {
		if _, err := s.admin.Exec(ctx, q, at...); err != nil {
			log.Println("sweep:", err)
		}
	}
	s.fmu.Lock()
	keys := make([]string, 0, len(s.fails))
	for k := range s.fails {
		keys = append(keys, k)
	}
	s.fmu.Unlock()
	s.limited(math.MaxInt, keys...) // drops what is older than 15 minutes
	var objects []string
	for q, at := range map[string]time.Time{
		"update request set state = 'expired' where state in ('ready', 'failed', 'sending') and updated < $1 returning team, id, rel": ago(originalKeep),
		"delete from request where updated < $1 returning team, id, rel":                                                              ago(requestKeep),
	} {
		rows, _ := s.admin.Query(ctx, q, at)
		for rows.Next() {
			var team, id int64
			var rel string
			if rows.Scan(&team, &id, &rel) == nil {
				objects = append(objects, originalKey(team, id, filepath.Base(rel)))
			}
		}
		if rows.Err() != nil {
			log.Println("sweep:", rows.Err())
		}
	}
	if err := s.deleteObjects(ctx, objects); err != nil {
		log.Println("sweep:", err)
	}
}

// --- On the Mac ---

// serveOriginals tells the site which drives are connected here, not those kept on this computer, and sends the
// originals asked for them.
func (a *App) serveOriginals(t *Team) {
	c := a.catalog()
	a.smu.Lock()
	drives := []string{}
	for id := range a.mounted {
		if !on(c.device(id).Local) {
			drives = append(drives, id)
		}
	}
	a.smu.Unlock()
	if t.call("POST", "/api/sync/online", map[string][]string{"drives": drives}, nil) != nil || len(drives) == 0 {
		return
	}
	var qs []Request
	if t.call("GET", "/api/sync/requests?drives="+url.QueryEscape(strings.Join(drives, ",")), nil, &qs) != nil {
		return
	}
	for _, q := range qs {
		var up Upload
		if t.call("POST", fmt.Sprintf("/api/sync/requests/%d/claim", q.ID), nil, &up) != nil {
			continue // another Mac took it
		}
		sum, etags, err := a.upload(q, up)
		msg := ""
		if err != nil {
			log.Printf("original %s: %v", q.Rel, err)
			msg = err.Error()
		}
		t.call("POST", fmt.Sprintf("/api/sync/requests/%d/sent", q.ID), map[string]any{"sha256": sum, "error": msg, "upload_id": up.UploadID, "etags": etags}, nil)
	}
}

// upload sends one original, in one PUT or part after part, hashing it on the way, and checks it is the
// file that was cataloged. It returns the hash and, for a multipart upload, the ETag of each part. A part
// that fails fails the upload: the member can ask again.
func (a *App) upload(q Request, up Upload) (string, []string, error) {
	c := a.catalog()
	cl := c.byKey[q.Device+"|"+q.Rel]
	if cl == nil {
		return "", nil, fmt.Errorf("not in this Mac's catalog")
	}
	f, err := os.Open(cl.Path)
	if err != nil {
		return "", nil, fmt.Errorf("the drive is not connected any more")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() != cl.Size {
		return "", nil, fmt.Errorf("the file changed after the scan: scan the drive again")
	}
	n, each := partsOf(st.Size())
	if up.UploadID != "" && int64(len(up.Parts)) != n {
		return "", nil, fmt.Errorf("the file changed after the scan: scan the drive again")
	}
	a.setBackground("Sending " + cl.Name + " to the team")
	defer a.setBackground("")
	h := sha256.New()
	put := func(dest string, off, size int64) (string, error) { // streams from the drive, no part in memory
		req, _ := http.NewRequest("PUT", dest, io.TeeReader(io.NewSectionReader(f, off, size), h))
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		res, err := uploadClient.Do(req)
		if err != nil {
			return "", err
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			return "", fmt.Errorf("upload: %s", res.Status)
		}
		return res.Header.Get("ETag"), nil
	}
	var etags []string
	if up.UploadID == "" {
		_, err = put(up.URL, 0, st.Size())
	}
	for i, dest := range up.Parts {
		off := int64(i) * each
		var etag string
		if etag, err = put(dest, off, min(each, st.Size()-off)); err != nil {
			err = fmt.Errorf("part %d of %d: %w", i+1, len(up.Parts), err)
			break
		}
		etags = append(etags, etag)
	}
	if err != nil {
		return "", nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if cl.FullHash != "" && !strings.EqualFold(cl.FullHash, sum) {
		return sum, nil, fmt.Errorf("the file read now differs from the one cataloged: scan the drive again")
	}
	return sum, etags, nil
}

// uploadClient has no overall timeout: an original can take hours on a slow line.
var uploadClient = &http.Client{}
