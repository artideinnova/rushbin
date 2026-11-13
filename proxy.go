package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Proxies are light copies of the clips (H.264, 1280 px on the longest side, about 3 Mbit/s, ready to
// stream) that the team watches from anywhere, with the drive unplugged. The Mac that has the drive makes
// them in the background and uploads them straight to the team's store; the site only signs the links.

// proxyNext makes and uploads one proxy the team lacks, among the clips on connected drives.
// It returns false when there is nothing to do (alone, a site without a store, or all done).
func (a *App) proxyNext() bool {
	t := a.teamConf()
	if t == nil {
		return false
	}
	var has struct{ Proxies []string }
	if err := t.call("GET", "/api/sync/have", nil, &has); err != nil || has.Proxies == nil {
		return false
	}
	done := set(has.Proxies)
	var todo []*Clip
	seen := map[string]bool{}
	c := a.catalog()
	for _, cl := range c.list {
		if !done[cl.Thumb] && !seen[cl.Thumb] && on(c.device(cl.Device).Copies) && !on(c.device(cl.Device).Local) && !a.failed[cl.Thumb] && fileExists(cl.Path) {
			seen[cl.Thumb] = true
			todo = append(todo, cl)
		}
	}
	a.setBackground("")
	if len(todo) == 0 {
		return false
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].Project+todo[i].ID < todo[j].Project+todo[j].ID })
	cl := todo[0]
	a.setBackground(fmt.Sprintf("Videos for the team: %d to go", len(todo)))
	if err := a.makeProxy(t, cl); err != nil {
		log.Printf("proxy %s: %v", cl.Path, err)
		a.setBackground(fmt.Sprintf("Videos for the team: could not make %s", cl.Name))
		if a.failed == nil {
			a.failed = map[string]bool{}
		}
		a.failed[cl.Thumb] = true // the others go on; this one is tried again after a restart
		return true
	}
	return true
}

func (a *App) setBackground(s string) {
	a.smu.Lock()
	a.background = s
	a.smu.Unlock()
}

// background runs a long job at macOS's background priority (taskpolicy -b): it uses what the Mac leaves
// free, so the app and everything else stay responsive.
func background(name string, args ...string) *exec.Cmd {
	if runtime.GOOS == "darwin" && fileExists("/usr/sbin/taskpolicy") {
		return exec.Command("/usr/sbin/taskpolicy", append([]string{"-b", name}, args...)...)
	}
	cmd := exec.Command(name, args...)
	lowPriority(cmd) // Windows: idle priority
	return cmd
}

// hwDecode: decode with the GPU when it can (ffmpeg falls back to software when it cannot): VideoToolbox on
// the Mac, D3D11/DXVA2 on Windows. Measured on a 4K MXF of the T9: 20 s of CPU per minute of video instead of 73.
func hwDecode() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"-hwaccel", "videotoolbox"}
	case "windows":
		return []string{"-hwaccel", "auto"}
	}
	return nil
}

var codecOnce sync.Once
var codec []string

// videoCodec is the first H.264 hardware encoder that works on this computer (the Mac's VideoToolbox; on
// Windows the NVIDIA, Intel or AMD one), tried once on a tiny clip; else the CPU: x264, or OpenH264 in the
// LGPL builds of FFmpeg that the apps bundle.
func videoCodec() []string {
	codecOnce.Do(func() {
		var try []string
		switch runtime.GOOS {
		case "darwin":
			try = []string{"h264_videotoolbox"}
		case "windows":
			try = []string{"h264_nvenc", "h264_qsv", "h264_amf", "h264_mf"} // mf: Media Foundation, on every Windows
		}
		for _, e := range append(try, "libx264", "libopenh264") {
			if exec.Command(tool("ffmpeg"), "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=s=256x256:d=0.2",
				"-pix_fmt", "yuv420p", "-c:v", e, "-f", "null", "-").Run() == nil {
				codec = []string{"-c:v", e}
				if e == "libx264" {
					codec = append(codec, "-preset", "veryfast")
				}
				return
			}
		}
		codec = []string{"-c:v", "libx264", "-preset", "veryfast"} // none works: ffmpeg will say why
	})
	return codec
}

func (a *App) makeProxy(t *Team, cl *Clip) error {
	cache, _ := os.UserCacheDir()
	dir := filepath.Join(cache, "rushbin", "proxy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, cl.Thumb+".mp4")
	defer os.Remove(tmp)
	in, feed := input(cl, 0, true, background)
	args := append([]string{"-nostdin", "-loglevel", "error", "-y", "-threads", "4"}, hwDecode()...)
	args = append(append(args, in...), "-vf", fit(1280, cl.Alpha), "-pix_fmt", "yuv420p")
	args = append(args, videoCodec()...)
	args = append(args, "-b:v", "3M", "-maxrate", "4M", "-bufsize", "8M")
	args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-movflags", "+faststart", "-f", "mp4", tmp)
	ff := background(tool("ffmpeg"), args...)
	done, err := feed(ff)
	if err != nil {
		return err
	}
	out, err := ff.CombinedOutput()
	done()
	if err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var up struct{ URL string }
	if err := t.call("POST", "/api/sync/proxy/"+cl.Thumb, nil, &up); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	req, _ := http.NewRequest("PUT", up.URL, f)
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "video/mp4")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("upload: %s", res.Status)
	}
	return t.call("POST", "/api/sync/proxy/"+cl.Thumb+"/done", map[string]int64{"size": st.Size()}, nil)
}

// proxyPlay gives the page a link to the team's proxy, for clips whose drive is not connected.
func (a *App) proxyPlay(w http.ResponseWriter, r *http.Request) {
	t := a.teamConf()
	if t == nil {
		http.Error(w, "Connect the drive to watch this clip", 404)
		return
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := t.call("GET", "/api/proxy/"+r.PathValue("thumb"), nil, &out); err != nil {
		http.Error(w, "No video of this clip for the team yet: connect the drive", 404)
		return
	}
	send(w, out)
}
