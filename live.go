package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Live conversion for clips the browser cannot read (MXF, ProRes, ...). WebKit, the engine of the app window
// and of Safari, refuses an endless MP4 stream but plays HLS natively, so ffmpeg writes 2-second HLS segments
// into a temporary folder. After a 10-second head start it converts at real-time speed and played segments
// are deleted, so a session uses a few tens of MB whatever the clip length. Seeking starts a new session at
// that point (the page already does this). A session ends 30 seconds after its last request.

type live struct {
	dir  string
	cmd  *exec.Cmd
	last time.Time
}

type lives struct {
	mu   sync.Mutex
	dir  string
	list map[string]*live
}

var liveFile = regexp.MustCompile(`^(index\.m3u8|init\.mp4|s(\d{5})\.m4s)$`)

// At most 2 conversions at once: measured about 226 MB of memory each for a 4K MXF with 2 threads.
const maxLives = 2

func newLives(dir string) *lives {
	os.RemoveAll(dir) // leftovers of a previous run
	l := &lives{dir: dir, list: map[string]*live{}}
	go func() {
		for range time.Tick(10 * time.Second) {
			l.mu.Lock()
			for id, s := range l.list {
				if time.Since(s.last) > 30*time.Second {
					l.stop(id)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// stop ends a session; the caller holds l.mu.
func (l *lives) stop(id string) {
	s := l.list[id]
	s.cmd.Process.Kill()
	os.RemoveAll(s.dir)
	delete(l.list, id)
}

func (a *App) startLive(w http.ResponseWriter, r *http.Request, cl *Clip, t float64) {
	l := a.lives
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	dir := filepath.Join(l.dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	args := append([]string{"-nostdin", "-loglevel", "error", "-threads", "2"}, hwDecode()...)
	in, feed := input(cl, t, true, exec.Command)
	args = append(append(args, "-readrate", "1", "-readrate_initial_burst", "10"), in...)
	args = append(args, "-vf", "scale=-2:'min(1080,ih)'", "-pix_fmt", "yuv420p")
	args = append(args, videoCodec()...)
	cmd := exec.Command(tool("ffmpeg"), append(args, "-b:v", "6M", "-force_key_frames", "expr:gte(t,n_forced*2)",
		"-c:a", "aac", "-b:a", "160k",
		"-f", "hls", "-hls_time", "2", "-hls_list_size", "0", "-hls_playlist_type", "event", "-hls_segment_type", "fmp4",
		"-hls_segment_filename", filepath.Join(dir, "s%05d.m4s"), "-hls_flags", "independent_segments+temp_file",
		filepath.Join(dir, "index.m3u8"))...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	done, err := feed(cmd)
	if err == nil {
		if err = cmd.Start(); err != nil {
			done()
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "ffmpeg did not start", 500)
		return
	}
	go func() {
		err := cmd.Wait()
		done()
		if err != nil && !strings.Contains(err.Error(), "killed") {
			log.Printf("live %s: %v %s", cl.Path, err, stderr.String())
		}
	}()
	l.mu.Lock()
	for len(l.list) >= maxLives { // the oldest viewer gives way
		var oldest string
		for k, s := range l.list {
			if oldest == "" || s.last.Before(l.list[oldest].last) {
				oldest = k
			}
		}
		l.stop(oldest)
	}
	l.list[id] = &live{dir: dir, cmd: cmd, last: time.Now()}
	l.mu.Unlock()
	// Answer when the first segment exists, so the player never sees an empty playlist.
	for i := 0; i < 200; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "index.m3u8")); err == nil && strings.Contains(string(b), "#EXTINF") {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	http.Redirect(w, r, "/live/"+id+"/index.m3u8", http.StatusFound)
}

// serveLive answers the player inside a session. The random session ID in the path is the key: Apple's
// media stack may load segments without the page cookie.
func (a *App) serveLive(w http.ResponseWriter, r *http.Request) {
	l := a.lives
	id, file := r.PathValue("id"), r.PathValue("file")
	m := liveFile.FindStringSubmatch(file)
	l.mu.Lock()
	s := l.list[id]
	if s == nil || m == nil {
		l.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	s.last = time.Now()
	dir := s.dir
	if m[2] != "" { // delete segments already played, keeping a few for small rewinds
		n, _ := strconv.Atoi(m[2])
		for k := n - 6; k >= 0; k-- {
			if os.Remove(filepath.Join(dir, "s"+leftPad(k)+".m4s")) != nil {
				break
			}
		}
	}
	l.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if file == "index.m3u8" {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// Start at the beginning of the stream, not at its live edge.
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(strings.Replace(string(b), "#EXTM3U\n", "#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n", 1)))
		return
	}
	if strings.HasSuffix(file, ".m4s") {
		w.Header().Set("Content-Type", "video/iso.segment")
	} else {
		w.Header().Set("Content-Type", "video/mp4")
	}
	http.ServeFile(w, r, filepath.Join(dir, file))
}

func leftPad(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", max(0, 5-len(s))) + s
}
