package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tidyDrive(t *testing.T) string {
	t.Helper()
	m := filepath.Join(t.TempDir(), "drive")
	for _, f := range []string{"A/C0001.MP4", "A/C0002.MP4", "B/x.wav", trash + "/old.mov", "loose.mov"} {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(m, f)), 0o755))
		must(t, os.WriteFile(filepath.Join(m, f), []byte("SECRETCONTENT"), 0o644))
	}
	return m
}

func TestValidTidy(t *testing.T) {
	m := tidyDrive(t)
	out := t.TempDir()
	if runtime.GOOS != "windows" {
		must(t, os.Symlink(out, filepath.Join(m, "L")))
	}
	ms := validTidy(m, []TidyMove{
		{From: "A", To: "Projects/2026-11-06/A"},             // 0 overlaps 11
		{From: "../etc", To: "X"},                            // 1 outside
		{From: "B", To: "/tmp/B"},                            // 2 absolute
		{From: "B", To: "B/inner"},                           // 3 into itself
		{From: "B", To: trash + "/B"},                        // 4 into the trash
		{From: trash + "/old.mov", To: "old.mov"},            // 5 out of the trash
		{From: "loose.mov", To: "B/x.wav"},                   // 6 something there
		{From: "gone.mov", To: "Q/gone.mov"},                 // 7 source missing
		{From: "loose.mov", To: "loose.mov"},                 // 8 onto itself
		{From: "B/x.wav", To: "Audio/x.wav"},                 // 9 and 10: same destination
		{From: "loose.mov", To: "Audio/x.wav"},               // 10
		{From: "A/C0001.MP4", To: "Cam/C0001.MP4"},           // 11 overlaps 0
		{From: ".Spotlight-V100", To: "Sys"},                 // hidden
		{From: "loose.mov", To: "L/loose.mov"},               // 12 through a symlink out of the drive
		{From: "B", To: "Projects/2026-11-06/A/B", OK: true}, // 13 into a folder that also moves
	})
	for i, m := range ms { // all refused; move 0 alone would pass but overlaps move 11
		if m.OK || m.Why == "" {
			t.Errorf("move %d %s -> %s: ok %v (%s)", i, m.From, m.To, m.OK, m.Why)
		}
	}
	if got := validTidy(m, []TidyMove{{From: "A", To: "Projects/2026-11-06/A"}}); !got[0].OK {
		t.Errorf("a plain move refused: %s", got[0].Why)
	}
	if runtime.GOOS != "windows" {
		if got := validTidy(m, []TidyMove{{From: "loose.mov", To: "L/loose.mov"}}); got[0].Why != "Path outside the drive" {
			t.Errorf("through a symlink out of the drive: %q", got[0].Why)
		}
	}
}

func TestTidyApplyUndo(t *testing.T) {
	m, dir := tidyDrive(t), t.TempDir()
	ms := validTidy(m, []TidyMove{{From: "A", To: "P/2026/CamA"}, {From: "loose.mov", To: "P/2026/loose.mov"}})
	for _, x := range ms {
		if !x.OK {
			t.Fatalf("refused: %+v", x)
		}
	}
	if n, err := tidyApply(dir, "D", m, ms); n != 2 || err != nil {
		t.Fatalf("apply = %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(m, "P/2026/CamA/C0001.MP4")); err != nil {
		t.Fatal("not moved:", err)
	}
	u, ok := readUndo(dir)
	if !ok || u.UUID != "D" || len(u.Made) != 2 {
		t.Fatalf("undo log = %+v", u)
	}
	if n, err := tidyRevert(dir, m, u); n != 2 || err != nil {
		t.Fatalf("undo = %d %v", n, err)
	}
	for _, f := range []string{"A/C0001.MP4", "loose.mov"} {
		if _, err := os.Stat(filepath.Join(m, f)); err != nil {
			t.Fatal("not put back:", f)
		}
	}
	if _, err := os.Stat(filepath.Join(m, "P")); err == nil {
		t.Fatal("folders made by the tidy are still there")
	}
	if _, ok := readUndo(dir); ok {
		t.Fatal("undo log still there")
	}
}

// The whole flow of an app alone against a fake Anthropic: its own AI and key, consent, proposal, apply by index, undo.
func TestTidyFlow(t *testing.T) {
	m, cat := tidyDrive(t), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(m, "R"), 0o755))
	for i := range 13 {
		must(t, os.WriteFile(filepath.Join(m, "R", fmt.Sprintf("A001_C%04d.mov", i)), nil, 0o644))
	}
	const key = "sk-ant-test-key"
	var sent string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		if r.URL.Path != "/messages" || r.Header.Get("x-api-key") != key || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(401)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn",
			"content":[{"type":"thinking","thinking":"","signature":"x"},{"type":"text","text":"{\"moves\":[{\"from\":\"A\",\"to\":\"Shoot/2026-11-06/CamA\",\"reason\":\"camera card folder\"},{\"from\":\"B\",\"to\":\"../outside\",\"reason\":\"bad\"}]}"}]}`)
	}))
	defer fake.Close()

	app := &App{dir: cat, token: "secret", mounted: map[string]string{"D": m}, seen: map[string]string{},
		ignored: map[string]bool{}, queue: make(chan string, 64), cat: &Catalog{}}
	srv := httptest.NewServer(app.guard(app.routes()))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	c.Get(srv.URL + "/?k=secret")
	post := func(path, body string, v any) int {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Rushbin", "1")
		r, err := c.Do(req)
		must(t, err)
		defer r.Body.Close()
		if v != nil && r.StatusCode == 200 {
			must(t, json.NewDecoder(r.Body).Decode(v))
		}
		return r.StatusCode
	}

	if code := post("/api/tidy/propose", `{"uuid":"D","consent":true,"via":"key"}`, nil); code != 400 {
		t.Fatalf("without a key: %d, want 400", code)
	}
	state := func() string {
		t.Helper()
		r, err := c.Get(srv.URL + "/api/tidy")
		must(t, err)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if s := state(); !strings.Contains(s, `"site":""`) || !strings.Contains(s, `"ready":false`) {
		t.Fatalf("an app in no team, with no AI: %s", s)
	}
	if code := post("/api/tidy/ai", `{"provider":"compatible","model":"m"}`, nil); code != 400 {
		t.Fatalf("an OpenAI-compatible server without its URL: %d, want 400", code)
	}
	if code := post("/api/tidy/ai", `{"provider":"anthropic","url":"`+fake.URL+`","key":"`+key+`"}`, nil); code != 200 {
		t.Fatalf("saving the AI: %d", code)
	}
	if st, err := os.Stat(filepath.Join(cat, tidyAIFile)); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("AI file: %v %v", st, err)
	}
	if s := state(); strings.Contains(s, key) || !strings.Contains(s, `"model":"claude-opus-5-5"`) || !strings.Contains(s, `"to":"Anthropic (Claude)"`) || !strings.Contains(s, `"key":true`) {
		t.Fatalf("state with an AI: %s", s)
	}
	if code := post("/api/tidy/propose", `{"uuid":"D","via":"Anthropic (Claude)"}`, nil); code != 409 {
		t.Fatalf("before consent: %d, want 409", code)
	}
	if code := post("/api/tidy/propose", `{"uuid":"D","consent":true,"via":"site"}`, nil); code != 409 {
		t.Fatalf("consent to the team site's way, alone: %d, want 409", code)
	}
	if sent != "" {
		t.Fatal("something was sent before consent")
	}
	var ms []TidyMove
	if code := post("/api/tidy/propose", `{"uuid":"D","consent":true,"via":"Anthropic (Claude)"}`, &ms); code != 200 {
		t.Fatalf("propose: %d", code)
	}
	if len(ms) != 2 || !ms[0].OK || ms[1].OK || ms[0].Reason != "camera card folder" {
		t.Fatalf("plan = %+v", ms)
	}
	if !strings.Contains(sent, `"claude-opus-5-5"`) || !strings.Contains(sent, `"output_config":{"format":{"schema"`) || !strings.Contains(sent, "C0001.MP4") || !strings.Contains(sent, "A###_C####.mov 13") ||
		strings.Contains(sent, "SECRETCONTENT") || strings.Contains(sent, key) || strings.Contains(sent, "old.mov") {
		t.Fatalf("request body: %s", sent)
	}
	sent = ""
	if code := post("/api/tidy/propose", `{"uuid":"D","via":"Anthropic (Claude)"}`, &ms); code != 200 || sent == "" {
		t.Fatalf("asking again for a consented drive: %d", code)
	}
	// another provider: the Anthropic key stays behind, and consent is asked again for the new receiver
	if code := post("/api/tidy/ai", `{"provider":"openai","url":"`+fake.URL+`"}`, nil); code != 200 || strings.Contains(state(), `"key":true`) {
		t.Fatalf("switching to OpenAI kept the Anthropic key: %d %s", code, state())
	}
	if code := post("/api/tidy/ai", `{"provider":"openai","url":"`+fake.URL+`","key":"sk-openai"}`, nil); code != 200 {
		t.Fatalf("saving OpenAI: %d", code)
	}
	if code := post("/api/tidy/propose", `{"uuid":"D","via":"OpenAI"}`, nil); code != 409 {
		t.Fatalf("a new receiver without consent: %d, want 409", code)
	}
	if code := post("/api/tidy/ai", `{"provider":"anthropic","url":"`+fake.URL+`","key":"`+key+`"}`, nil); code != 200 {
		t.Fatalf("back to Anthropic: %d", code)
	}
	if code := post("/api/tidy/propose", `{"uuid":"D","via":"Anthropic (Claude)"}`, &ms); code != 200 {
		t.Fatalf("propose again: %d", code)
	}
	var res struct{ Moved, Skipped int }
	if code := post("/api/tidy/apply", `{"uuid":"D","moves":[0,1]}`, &res); code != 200 || res.Moved != 1 || res.Skipped != 1 {
		t.Fatalf("apply: %d %+v", code, res)
	}
	if _, err := os.Stat(filepath.Join(m, "Shoot/2026-11-06/CamA/C0002.MP4")); err != nil {
		t.Fatal("not moved")
	}
	if code := post("/api/tidy/apply", `{"uuid":"D","moves":[0]}`, nil); code != 409 {
		t.Fatalf("applying the same plan twice: %d, want 409", code)
	}
	if code := post("/api/tidy/undo", `{}`, &res); code != 200 || res.Moved != 1 {
		t.Fatalf("undo: %d %+v", code, res)
	}
	if _, err := os.Stat(filepath.Join(m, "A/C0002.MP4")); err != nil {
		t.Fatal("not put back")
	}
	if _, err := os.Stat(filepath.Join(m, "Shoot")); err == nil {
		t.Fatal("Shoot still there after the undo")
	}
}

// Each provider against a fake of its API: the request it sends, the plan read from its answer, and clear errors
// that never carry the key.
func TestAIProviders(t *testing.T) {
	const key = "sk-test-secret-key"
	const plan = `{\"moves\":[{\"from\":\"A\",\"to\":\"B/A\",\"reason\":\"r\"}]}`
	for _, c := range []struct {
		provider, path, auth, body, answer string
	}{
		{"anthropic", "/messages", "X-Api-Key", `"output_config":{"format":{"schema"`,
			`{"stop_reason":"end_turn","content":[{"type":"text","text":"` + plan + `"}]}`},
		{"openai", "/chat/completions", "Authorization", `"response_format":{"json_schema":{"name":"tidy_plan","schema"`,
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"` + plan + `"}}]}`},
		{"gemini", "/models/m:generateContent", "X-Goog-Api-Key", `"generationConfig":{"responseFormat":{"text":{"mimeType":"APPLICATION_JSON"`,
			`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"…","thought":true},{"text":"` + plan + `"}]}}]}`},
		{"compatible", "/chat/completions", "Authorization", `"messages":[{"content":"You reorganize`, // the prompt alone
			`{"choices":[{"finish_reason":"stop","message":{"content":"Here it is:\n` + "```json\\n" + plan + "\\n```" + `"}}]}`},
	} {
		answer, status := c.answer, 200
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if r.URL.Path != c.path || !strings.Contains(r.Header.Get(c.auth), key) || !strings.Contains(string(b), c.body) ||
				!strings.Contains(string(b), "Drive: 1 files") || strings.Contains(string(b), key) {
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"error":{"message":"unexpected request %s %s"}}`, r.URL.Path, b)
				return
			}
			w.WriteHeader(status)
			io.WriteString(w, answer)
		}))
		ai, err := newAI(c.provider, "m", fake.URL, key)
		must(t, err)
		ms, err := propose(t.Context(), ai, "Drive: 1 files")
		if err != nil || len(ms) != 1 || ms[0].To != "B/A" {
			t.Fatalf("%s: %+v %v", c.provider, ms, err)
		}
		for _, bad := range []struct {
			status    int
			answer, e string
		}{
			{200, strings.ReplaceAll(c.answer, plan, "Sorry, no."), "did not answer with a plan of moves"},
			{200, `{}`, "did not answer with a plan of moves"},
			{429, `{"error":{"message":"slow down, ` + key + `"}}`, "answered 429 slow down, (key)"},
			{401, `{"error":{"message":"bad key ` + key + `"}}`, "does not accept this API key"},
		} {
			answer, status = bad.answer, bad.status
			_, err := propose(t.Context(), ai, "Drive: 1 files")
			if err == nil || !strings.Contains(err.Error(), bad.e) || strings.Contains(err.Error(), key) {
				t.Fatalf("%s, answer %d %s: %v", c.provider, bad.status, bad.answer, err)
			}
		}
		fake.Close()
	}
}

// The site's AI from its environment.
func TestSiteAI(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string // provider, or the error's start
	}{
		{map[string]string{}, ""},
		{map[string]string{"RUSHBIN_ANTHROPIC_KEY": "k"}, "anthropic"},
		{map[string]string{"RUSHBIN_AI_PROVIDER": "openai", "RUSHBIN_AI_KEY": "k"}, "openai"},
		{map[string]string{"RUSHBIN_AI_PROVIDER": "openai", "RUSHBIN_ANTHROPIC_KEY": "k"}, "RUSHBIN_AI_KEY is needed"},
		{map[string]string{"RUSHBIN_AI_PROVIDER": "compatible", "RUSHBIN_AI_URL": "http://ollama:11434/v1", "RUSHBIN_AI_MODEL": "m"}, "compatible"},
		{map[string]string{"RUSHBIN_AI_PROVIDER": "gemini", "RUSHBIN_AI_KEY": "k"}, "gemini"},
		{map[string]string{"RUSHBIN_AI_PROVIDER": "nope", "RUSHBIN_AI_KEY": "k"}, "RUSHBIN_AI_*"},
	} {
		for _, k := range []string{"RUSHBIN_AI_PROVIDER", "RUSHBIN_AI_KEY", "RUSHBIN_AI_MODEL", "RUSHBIN_AI_URL", "RUSHBIN_ANTHROPIC_KEY"} {
			t.Setenv(k, c.env[k])
		}
		ai, err := siteAI()
		if got := ai.Provider; err != nil && !strings.HasPrefix(err.Error(), c.want) || err == nil && got != c.want {
			t.Fatalf("%v: %+v %v, want %q", c.env, ai, err, c.want)
		}
	}
}
