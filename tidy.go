package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tidy up with AI: a compact description of a connected drive (names, counts, sizes, dates, codecs; no file
// contents, no frames) goes to an AI, through the team site when it has one of its own, else with the provider,
// model and key the user picked in the app. The AI answers a plan of moves as JSON text, the app validates each
// move against the drive, the user ticks what to do, and the moves are same-volume renames with an undo log.
// Nothing is deleted.

// TidyMove is one proposed move, paths relative to the drive with forward slashes.
type TidyMove struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	OK     bool   `json:"ok"`
	Why    string `json:"why,omitempty"`
	Kind   string `json:"kind,omitempty"`    // what moves, from what made it (tidyKind): export, audio, footage, project, graphics, other
	Proj   string `json:"project,omitempty"` // the project it belongs to, as the Archive names it: the person may approve one project only
}

const (
	tidyAIFile      = "tidy_ai.json"  // in the catalog dir, 0600, only on this computer: the AI and its key
	tidyOldKeyFile  = "anthropic_key" // what earlier versions saved: an Anthropic key alone
	tidyConsentFile = "tidy_consent.json"
	tidyUndoFile    = "tidy_undo.json"
	tidyMaxDesc     = 60_000 // bytes of description sent at most
	tidyMaxFolders  = 600
	tidyNames       = 200 // names listed per folder at most, a numbered camera sequence counting as one
	tidyDepth       = 4   // deeper folders are summed into their ancestor at this depth
)

// AI is the way to a model: the provider, its model, the key (only ever in a request header to that provider) and
// the API base, the provider's own when empty (an OpenAI-compatible server always has one).
type AI struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	URL      string `json:"url,omitempty"`
	Key      string `json:"key,omitempty"`
}

// providers: the name the confirmation shows, the API base and the default model (each provider's current
// recommended general model; the user can change it).
var providers = map[string]struct{ Name, URL, Model string }{
	"anthropic":  {"Anthropic (Claude)", "https://api.anthropic.com/v1", "claude-opus-5-5"},
	"openai":     {"OpenAI", "https://api.openai.com/v1", "gpt-6-astra"},
	"gemini":     {"Google (Gemini)", "https://generativelanguage.googleapis.com/v1beta", "gemini-4-carbon"},
	"compatible": {"", "", ""}, // OpenAI-compatible (Mistral, Ollama, LM Studio...): base URL and model from the user
}

// newAI checks a choice of AI, from the app's page or the site's environment.
func newAI(provider, model, base, key string) (AI, error) {
	p, ok := providers[provider]
	if !ok {
		return AI{}, fmt.Errorf("unknown AI provider %q", provider)
	}
	ai := AI{Provider: provider, Model: cmp.Or(strings.TrimSpace(model), p.Model), URL: strings.TrimRight(strings.TrimSpace(base), "/"), Key: strings.TrimSpace(key)}
	if u, err := url.Parse(ai.URL); ai.URL != "" && (err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "") {
		return AI{}, errors.New("the base URL must start with https:// or http://")
	}
	if provider == "compatible" && (ai.URL == "" || ai.Model == "") {
		return AI{}, errors.New("an OpenAI-compatible server needs its base URL and a model")
	}
	return ai, nil
}

// To is who receives the description: the provider's name, or the host of an OpenAI-compatible server.
func (ai AI) To() string {
	if n := providers[ai.Provider].Name; n != "" {
		return n
	}
	if u, err := url.Parse(ai.URL); err == nil {
		return u.Host
	}
	return ai.URL
}

// siteAI is a team site's own AI, from its environment: RUSHBIN_AI_PROVIDER (anthropic when not set), _MODEL, _URL
// and _KEY, or RUSHBIN_ANTHROPIC_KEY for Anthropic; none without a provider or a key.
func siteAI() (AI, error) {
	p, key := cmp.Or(os.Getenv("RUSHBIN_AI_PROVIDER"), "anthropic"), os.Getenv("RUSHBIN_AI_KEY")
	if p == "anthropic" {
		key = cmp.Or(key, os.Getenv("RUSHBIN_ANTHROPIC_KEY"))
	}
	if os.Getenv("RUSHBIN_AI_PROVIDER") == "" && key == "" {
		return AI{}, nil
	}
	ai, err := newAI(p, os.Getenv("RUSHBIN_AI_MODEL"), os.Getenv("RUSHBIN_AI_URL"), key)
	switch {
	case err != nil:
		return AI{}, fmt.Errorf("RUSHBIN_AI_*: %w", err)
	case !ai.ready():
		return AI{}, fmt.Errorf("RUSHBIN_AI_KEY is needed for %s", p)
	}
	return ai, nil
}

const tidySystem = `You reorganize a video production drive the way a good assistant editor or DIT would. You get a description of its folders: per folder the files, size, types, every file name directly in it (a numbered sequence, like a camera's clips, shown as first … last with its count), what made its files when known ("made": camera and model, sound recorder, export from an editing app, preview of an editing app; a "?" means deduced from names and folders, not read from the files), clip durations, codecs and dates.
How footage is kept in post-production, which your moves follow:
1. A camera card or a clip is never split. A folder of camera originals moves whole, with what sits next to its clips (XML sidecars, .rtn, the .wav of a RED clip, .RDC folders, BPAV, CLIP, PRIVATE, DCIM). Never move single files out of it.
2. Each project keeps apart, in folders with the standard post-production names used in every country: Footage (camera originals, by camera or by day), Audio (sound recordings), Project (editing project files with their autosaves and render previews), Graphics (stills, titles, logos) and Export (what the editing apps exported). If the project already has a folder for one of these under another name (XPORT, Esportati, Audio, RAW), keep using it.
3. Inside Export, each piece has its own folder named after it, holding all its versions and its subtitle files: Export/Sayf/ for Sayf.mp4, Sayf v2.mp4, Sayf v4.mp4. The description lists these "series" (versions of one piece, found by rushbin from their names): follow them. A piece with a single file still gets its own folder only when other pieces do.
4. Editing projects (.prproj, .drp, .fcpbundle, .avb, .aep) find their media by path: moving original footage they use breaks their links. Prefer moving exports and loose files; move originals only when clearly misplaced, and then say in the reason that the editing project will need relinking.
5. Never change a file's name, only its folder.
6. Keep what is already clear where it is: no move without a reason a producer would agree with.
Rules: never delete anything; paths are relative to the drive root with forward slashes; every move takes an existing path listed in the description; never move into or out of _rushbin_trash; never move a folder into itself; no two moves to the same destination; do not move both a folder and something inside it. Give each move a one-line reason. Answer with only a JSON object {"moves": [{"from": "...", "to": "...", "reason": "..."}]}, with an empty list if the drive is already tidy.`

// tidySchema is the plan's shape, enforced by the providers that take a JSON schema (OpenAI-compatible servers
// get the prompt alone); the plan is read and checked the same way whoever answers.
var tidySchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"moves"},
	"properties": map[string]any{"moves": map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"from", "to", "reason"},
		"properties": map[string]any{
			"from":   map[string]any{"type": "string", "description": "Existing path relative to the drive root"},
			"to":     map[string]any{"type": "string", "description": "New path relative to the drive root"},
			"reason": map[string]any{"type": "string", "description": "One line: why this move"},
		}}}},
}

// propose asks the AI for the moves: each provider only turns the description into the model's text.
func propose(ctx context.Context, ai AI, desc string) ([]TidyMove, error) {
	var text string
	var err error
	switch ai.Provider {
	case "anthropic":
		text, err = askAnthropic(ctx, ai, desc)
	case "gemini":
		text, err = askGemini(ctx, ai, desc)
	default:
		text, err = askOpenAI(ctx, ai, desc)
	}
	if err != nil {
		return nil, err
	}
	var p struct{ Moves []TidyMove }
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}") // the object, also when wrapped in a code block
	if i < 0 || j < i || json.Unmarshal([]byte(text[i:j+1]), &p) != nil || p.Moves == nil {
		return nil, fmt.Errorf("%s did not answer with a plan of moves: try again, or another model", ai.To())
	}
	return p.Moves, nil
}

// aiCall posts one request and decodes the answer. The key goes only in the headers; an error never carries it,
// not even when the provider repeats it.
func aiCall(ctx context.Context, ai AI, endpoint string, h http.Header, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", cmp.Or(ai.URL, providers[ai.Provider].URL)+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = h
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("%s cannot be reached: check the connection", ai.To())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 200 && json.Unmarshal(b, out) == nil {
		return nil
	}
	var e struct{ Error struct{ Message string } }
	json.Unmarshal(b, &e)
	msg := fmt.Sprintf("%s answered %d %s", ai.To(), resp.StatusCode, e.Error.Message)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		msg = ai.To() + " does not accept this API key"
	}
	if ai.Key != "" {
		msg = strings.ReplaceAll(msg, ai.Key, "(key)")
	}
	return errors.New(strings.TrimSpace(msg))
}

const (
	errDeclined = "%s declined to propose a plan for this drive"
	errTooLong  = "the plan was too long for one answer: try again on a smaller drive"
)

// askAnthropic: Messages API with JSON outputs (output_config.format). A refusal is retried server side on a model
// chosen by category (fallbacks).
func askAnthropic(ctx context.Context, ai AI, desc string) (string, error) {
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct{ Type, Text string }
	}
	err := aiCall(ctx, ai, "/messages", http.Header{"X-Api-Key": {ai.Key}, "Anthropic-Version": {"2023-06-01"},
		"Anthropic-Beta": {"server-side-fallback-2026-07-01"}}, map[string]any{
		"model": ai.Model, "max_tokens": 16000, "system": tidySystem, "fallbacks": "default",
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": tidySchema}},
		"messages":      []any{map[string]any{"role": "user", "content": desc}},
	}, &out)
	switch {
	case err != nil:
		return "", err
	case out.StopReason == "refusal":
		return "", fmt.Errorf(errDeclined, ai.To())
	case out.StopReason == "max_tokens":
		return "", errors.New(errTooLong)
	}
	var s strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			s.WriteString(c.Text)
		}
	}
	return s.String(), nil
}

// askOpenAI: Chat Completions, which OpenAI-compatible servers speak too; OpenAI itself enforces the schema.
func askOpenAI(ctx context.Context, ai AI, desc string) (string, error) {
	in := map[string]any{"model": ai.Model, "messages": []any{
		map[string]any{"role": "system", "content": tidySystem}, map[string]any{"role": "user", "content": desc}}}
	if ai.Provider == "openai" {
		in["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "tidy_plan", "strict": true, "schema": tidySchema}}
	}
	h := http.Header{}
	if ai.Key != "" { // a local server may need none
		h.Set("Authorization", "Bearer "+ai.Key)
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct{ Content, Refusal string }
		}
	}
	if err := aiCall(ctx, ai, "/chat/completions", h, in, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	switch c := out.Choices[0]; {
	case c.Message.Refusal != "" || c.FinishReason == "content_filter":
		return "", fmt.Errorf(errDeclined, ai.To())
	case c.FinishReason == "length":
		return "", errors.New(errTooLong)
	default:
		return c.Message.Content, nil
	}
}

// askGemini: generateContent with a JSON response format.
func askGemini(ctx context.Context, ai AI, desc string) (string, error) {
	var out struct {
		PromptFeedback struct{ BlockReason string }
		Candidates     []struct {
			FinishReason string
			Content      struct {
				Parts []struct {
					Text    string
					Thought bool
				}
			}
		}
	}
	err := aiCall(ctx, ai, "/models/"+url.PathEscape(ai.Model)+":generateContent", http.Header{"X-Goog-Api-Key": {ai.Key}}, map[string]any{
		"system_instruction": map[string]any{"parts": []any{map[string]any{"text": tidySystem}}},
		"contents":           []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": desc}}}},
		"generationConfig":   map[string]any{"responseFormat": map[string]any{"text": map[string]any{"mimeType": "APPLICATION_JSON", "schema": tidySchema}}},
	}, &out)
	if err != nil {
		return "", err
	}
	if out.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf(errDeclined, ai.To())
	}
	if len(out.Candidates) == 0 {
		return "", nil
	}
	c := out.Candidates[0]
	switch c.FinishReason {
	case "STOP", "":
	case "MAX_TOKENS":
		return "", errors.New(errTooLong)
	default: // SAFETY, PROHIBITED_CONTENT, BLOCKLIST...
		return "", fmt.Errorf(errDeclined, ai.To())
	}
	var s strings.Builder
	for _, p := range c.Content.Parts {
		if !p.Thought {
			s.WriteString(p.Text)
		}
	}
	return s.String(), nil
}

// describe builds the compact description of a drive: folder tree with per-folder counts, sizes, types and
// name patterns from the drive itself, durations, codecs and dates from the catalog. Capped in size.
func describe(c *Catalog, uuid, mount string) (string, error) {
	type agg struct {
		files    int
		size     int64
		types    map[string]int
		names    []string // the files directly in it
		pieces   []string // those of them that are exports (or say nothing of what made them): grouped in series
		made     map[string]int
		dur      float64
		codecs   map[string]int
		from, to time.Time
		projects map[string]bool
	}
	folders := map[string]*agg{}
	skip := map[string]bool{} // the folders the drive's catalog skips: their names never reach the AI, nothing in them moves
	for _, x := range c.device(uuid).Excluded {
		skip[x] = true
	}
	get := func(rel string) *agg {
		if parts := strings.Split(rel, "/"); len(parts) > tidyDepth {
			rel = strings.Join(parts[:tidyDepth], "/")
		}
		g := folders[rel]
		if g == nil {
			g = &agg{types: map[string]int{}, codecs: map[string]int{}, projects: map[string]bool{}, made: map[string]int{}}
			folders[rel] = g
		}
		return g
	}
	var total int64
	var files int
	err := filepath.WalkDir(mount, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: left out
		}
		if p != mount && (strings.HasPrefix(d.Name(), ".") || d.Name() == trash || d.IsDir() && skip[d.Name()]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(mount, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			get(rel)
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		g := get(path.Dir(rel))
		g.files++
		g.size += info.Size()
		g.types[strings.ToLower(strings.TrimPrefix(path.Ext(rel), "."))]++
		if path.Dir(rel) == folderOf(rel) { // directly in the folder listed, not summed from deeper ones
			g.names = append(g.names, d.Name())
			if k := fileKind(p, c.files[p].Made); k == "export" || k == "other" {
				g.pieces = append(g.pieces, d.Name())
			}
		}
		if m := c.files[p].Made; m != "" {
			g.made[m]++
		}
		total += info.Size()
		files++
		return nil
	})
	if err != nil {
		return "", err
	}
	var first, last time.Time
	for _, cl := range c.list {
		if cl.Device != uuid {
			continue
		}
		g := get(path.Dir(cl.ID))
		g.dur += cl.Dur
		if cl.Codec != "" {
			g.codecs[cl.Codec]++
		}
		if cl.Project != "" {
			g.projects[cl.Project] = true
		}
		if t := cl.Created; t != nil {
			if g.from.IsZero() || t.Before(g.from) {
				g.from = *t
			}
			if t.After(g.to) {
				g.to = *t
			}
			if first.IsZero() || t.Before(first) {
				first = *t
			}
			if t.After(last) {
				last = *t
			}
		}
	}
	top := func(m map[string]int, k int) string {
		keys := make([]string, 0, len(m))
		for x := range m {
			keys = append(keys, x)
		}
		sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || m[keys[i]] == m[keys[j]] && keys[i] < keys[j] })
		var s []string
		for i, x := range keys {
			if i == k {
				s = append(s, fmt.Sprintf("+%d more", len(keys)-k))
				break
			}
			s = append(s, fmt.Sprintf("%s %d", x, m[x]))
		}
		return strings.Join(s, ", ")
	}
	day := func(t time.Time) string { return t.UTC().Format("2006-01-02") }
	var b strings.Builder
	fmt.Fprintf(&b, "Drive: %d files, %s. ", files, mb(total))
	if !first.IsZero() {
		fmt.Fprintf(&b, "Clips recorded %s to %s.", day(first), day(last))
	}
	fmt.Fprintf(&b, "\nFolders deeper than %d levels are summed into their ancestor at that depth. One line per folder, files counted in that folder only:\n", tidyDepth)
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range folders {
			if !yield(k) {
				return
			}
		}
	})
	for i, name := range names {
		g := folders[name]
		if name == "." {
			name = "(drive root)"
		}
		line := fmt.Sprintf("%s/ files %d, %s", name, g.files, mb(g.size))
		if g.files > 0 {
			line += "; types: " + top(g.types, 5)
			if len(g.names) > 0 {
				line += "; files: " + listNames(g.names)
			}
			if s := series(g.pieces); s != "" {
				line += "; series: " + s
			}
			if len(g.made) > 0 {
				line += "; made: " + top(g.made, 6)
			}
		}
		if g.dur > 0 {
			line += fmt.Sprintf("; footage %.0f min", g.dur/60)
		}
		if len(g.codecs) > 0 {
			line += "; codecs: " + top(g.codecs, 3)
		}
		if !g.from.IsZero() {
			line += "; dates " + day(g.from) + " to " + day(g.to)
		}
		if len(g.projects) > 0 {
			line += "; projects: " + strings.Join(slices.Sorted(func(y func(string) bool) {
				for p := range g.projects {
					if !y(p) {
						return
					}
				}
			}), ", ")
		}
		if i == tidyMaxFolders || b.Len()+len(line) > tidyMaxDesc {
			fmt.Fprintf(&b, "(%d more folders not listed)\n", len(names)-i)
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}

var digits = regexp.MustCompile(`[0-9]+`)

// languageNames: the page's languages, named for the AI that writes the reasons of its moves.
var languageNames = map[string]string{"en": "English", "it": "Italian", "es": "Spanish", "fr": "French", "de": "German",
	"pt-BR": "Brazilian Portuguese", "ja": "Japanese"}

// tidyKind is what a move takes, from what made its files (made.go) and their types: export, audio, footage,
// project, graphics or other. A folder is the kind most of its files are. The review groups and approves by it.
func tidyKind(c *Catalog, mount, from string) string {
	abs := filepath.Join(mount, filepath.FromSlash(from))
	st, err := os.Lstat(abs)
	if err != nil {
		return "other"
	}
	if !st.IsDir() {
		return fileKind(abs, c.files[abs].Made)
	}
	n := map[string]int{}
	filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			n[fileKind(p, c.files[p].Made)]++
		}
		return nil
	})
	best := "other"
	for k, v := range n {
		if k != "other" && (best == "other" || v > n[best] || v == n[best] && k < best) {
			best = k
		}
	}
	return best
}

func fileKind(p, made string) string {
	switch madeKind(made) {
	case "export", "preview":
		return "export"
	case "recorder":
		return "audio"
	case "camera":
		return "footage"
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".wav", ".mp3", ".aif", ".aiff", ".m4a", ".bwf", ".flac":
		return "audio"
	case ".prproj", ".prin", ".drp", ".drt", ".aep", ".fcpbundle", ".fcpxml", ".avb", ".avp", ".edl", ".otio":
		return "project"
	case ".png", ".jpg", ".jpeg", ".psd", ".ai", ".svg", ".tif", ".tiff", ".eps", ".mogrt", ".pdf":
		return "graphics"
	case ".ttml", ".srt", ".vtt", ".scc":
		return "export" // subtitles go with their export
	case ".r3d", ".nev", ".braw", ".ari", ".mxf", ".rtn", ".xml", ".rmd":
		return "footage" // camera formats and their sidecars
	}
	return "other"
}

var (
	nameSep     = regexp.MustCompile(`[\s_\-.()\[\]]+`)
	versionMark = regexp.MustCompile(`(?i)^(v|ver|version|versione|rev)?\d+[a-z]?$|^[a-z]$|^\d{3,4}[px]\d{0,4}$|^(def|definitiv[oa]|final[e]?|master|prova|bozza|draft|rough|premontato|subtitle|sub)(v?\d+)?$`)
)

// piece is the name of the piece a file is a version of: its name without the marks of a version (v2, v04, DEF,
// Prova, (1), 1080p, a lone letter or number). "" when nothing is left, as for a camera's numbered clips.
func piece(name string) string {
	var words []string
	for _, w := range nameSep.Split(strings.TrimSuffix(name, path.Ext(name)), -1) {
		if w != "" && !versionMark.MatchString(w) {
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

// series lists the pieces with more than one file among names: "Sayf (Sayf.mp4, Sayf v2.mp4, Sayf v4.mp4)".
func series(names []string) string {
	by, order := map[string][]string{}, []string{}
	for _, n := range slices.Sorted(slices.Values(names)) {
		p := piece(n)
		k := strings.ToLower(p)
		if p == "" {
			continue
		}
		if by[k] == nil {
			order = append(order, p)
		}
		by[k] = append(by[k], n)
	}
	var out []string
	for _, p := range order {
		if fs := by[strings.ToLower(p)]; len(fs) > 1 {
			out = append(out, p+" ("+strings.Join(fs, ", ")+")")
		}
	}
	return strings.Join(out, "; ")
}

// folderOf is the folder a file is listed under in the description: its own, or its ancestor at tidyDepth.
func folderOf(rel string) string {
	dir := path.Dir(rel)
	if parts := strings.Split(dir, "/"); len(parts) > tidyDepth {
		return strings.Join(parts[:tidyDepth], "/")
	}
	return dir
}

// listNames lists file names, sorted, a numbered sequence of 4 or more (576_0017.MXF, 576_0018.MXF …) as
// "576_0017.MXF … 576_0070.MXF (54)", at most tidyNames entries.
func listNames(names []string) string {
	names = slices.Clone(names)
	slices.Sort(names)
	groups := map[string][]string{}
	var order []string
	for _, n := range names {
		k := digits.ReplaceAllString(n, "#")
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], n)
	}
	var out []string
	for _, k := range order {
		g := groups[k]
		if len(g) >= 4 && strings.Contains(k, "#") {
			out = append(out, fmt.Sprintf("%s … %s (%d)", g[0], g[len(g)-1], len(g)))
		} else {
			out = append(out, g...)
		}
	}
	if len(out) > tidyNames {
		out = append(out[:tidyNames], fmt.Sprintf("+%d more", len(out)-tidyNames))
	}
	return strings.Join(out, ", ")
}

func mb(n int64) string { return fmt.Sprintf("%d MB", (n+999_999)/1_000_000) }

// validTidy checks every move against the drive as it is now. A move is refused when it leaves the drive,
// touches the trash, lands on itself or inside itself, collides with a file or another move, or its source
// is gone. Moves are renames, so they never cross volumes (a rename across volumes fails, nothing is copied).
// skip: the folder names the drive's catalog skips (Device.Excluded): nothing in them moves.
func validTidy(mount string, skip []string, ms []TidyMove) []TidyMove {
	root, err := filepath.EvalSymlinks(mount)
	if err != nil {
		root = mount
	}
	clean := func(p string) (string, bool) {
		p = strings.ReplaceAll(p, `\`, "/")
		if p == "" || strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" {
			return "", false
		}
		p = path.Clean(p)
		return p, p != "." && p != ".." && !strings.HasPrefix(p, "../")
	}
	within := func(p, dir string) bool { // drives are mostly case-insensitive (APFS, exFAT): so is this
		p, dir = strings.ToLower(p), strings.ToLower(dir)
		return p == dir || strings.HasPrefix(p, dir+"/")
	}
	inDrive := func(rel string) bool { // the nearest existing ancestor, symlinks resolved, is on the drive
		for p := filepath.Join(mount, filepath.FromSlash(rel)); ; p = filepath.Dir(p) {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				rr, err := filepath.Rel(root, r)
				return err == nil && rr != ".." && !strings.HasPrefix(rr, ".."+string(filepath.Separator))
			}
			if p == filepath.Dir(p) {
				return false
			}
		}
	}
	out := make([]TidyMove, len(ms))
	for i, m := range ms {
		m.OK, m.Why = false, ""
		from, ok1 := clean(m.From)
		to, ok2 := clean(m.To)
		m.From, m.To = from, to
		switch {
		case !ok1 || !ok2:
			m.Why = "Path outside the drive"
		case strings.HasPrefix("/"+from, "/.") || strings.Contains(from, "/.") || strings.HasPrefix("/"+to, "/.") || strings.Contains(to, "/."):
			m.Why = "Hidden system files stay where they are"
		case within(from, trash) || within(to, trash):
			m.Why = "Into or out of the files to delete"
		case from == to:
			m.Why = "Onto itself"
		case within(to, from):
			m.Why = "Into its own folder"
		case !inDrive(path.Dir(from)) || !inDrive(path.Dir(to)):
			m.Why = "Path outside the drive"
		case slices.ContainsFunc(skip, func(x string) bool {
			return slices.Contains(strings.Split(from, "/"), x) || slices.Contains(strings.Split(to, "/"), x)
		}):
			m.Why = "In a folder this drive's catalog skips"
		}
		if m.Why == "" {
			if st, err := os.Lstat(filepath.Join(mount, filepath.FromSlash(from))); err != nil {
				m.Why = "No longer in that place"
			} else if !st.IsDir() && path.Base(from) != path.Base(to) {
				m.Why = "A file keeps its name"
			} else if _, err := os.Lstat(filepath.Join(mount, filepath.FromSlash(to))); err == nil {
				m.Why = "Something is already there"
			}
		}
		out[i] = m
	}
	for i := range out { // moves that step on each other: none of them runs
		for j := range out {
			if i == j || out[i].From == "" || out[j].From == "" {
				continue
			}
			x, y := out[i], out[j]
			switch {
			case x.To == y.To || within(x.To, y.To) || within(y.To, x.To):
				out[i].Why = cond(out[i].Why, "Same destination as another move")
			case within(x.From, y.From) || within(y.From, x.From):
				out[i].Why = cond(out[i].Why, "Overlaps another move")
			case within(x.To, y.From) || within(y.To, x.From):
				out[i].Why = cond(out[i].Why, "Into a folder that also moves")
			}
		}
	}
	for i := range out {
		out[i].OK = out[i].Why == ""
	}
	return out
}

// tidyGaps finds where a plan would leave a drive half tidied: something moves, and things of the same kind next to
// it stay behind (DAY 1 into Footage, DAY 4 left at the project's root). The AI gets them back once to complete its
// plan; what is still left shows with the plan. Side by side at the drive's root are projects: no gap there.
type TidyGap struct {
	Path string `json:"path"` // stays where it is
	Like string `json:"like"` // something of the same kind next to it that moves
	To   string `json:"to"`   // where that one goes
	Kind string `json:"kind"`
}

func (g TidyGap) String() string {
	return fmt.Sprintf("%s stays in %s, while %s (also %s) moves to %s", g.Path, path.Dir(g.Path), g.Like, g.Kind, g.To)
}

func tidyGaps(c *Catalog, mount string, skip []string, ms []TidyMove) []TidyGap {
	within := func(p, dir string) bool {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
		return p == dir || strings.HasPrefix(p, dir+"/")
	}
	moved := map[string]bool{}
	for _, m := range ms {
		if m.OK {
			moved[strings.ToLower(m.From)] = true
		}
	}
	seen := map[string]bool{}
	var gaps []TidyGap
	for _, m := range ms {
		src, dst := path.Dir(m.From), path.Dir(m.To)
		if !m.OK || m.Kind == "" || m.Kind == "other" || src == "." {
			continue
		}
		es, _ := os.ReadDir(filepath.Join(mount, filepath.FromSlash(src)))
		for _, e := range es {
			p := src + "/" + e.Name()
			if strings.HasPrefix(e.Name(), ".") || e.Name() == trash || slices.Contains(skip, e.Name()) || moved[strings.ToLower(p)] ||
				seen[p] || within(dst, p) || tidyKind(c, mount, p) != m.Kind {
				continue
			}
			seen[p] = true
			gaps = append(gaps, TidyGap{p, m.From, dst, m.Kind})
		}
	}
	return gaps
}

// tidyTree: the folders of the projects a plan touches, before and after it, four levels deep and without what is
// inside camera cards (.RDC, BPAV, CLIP …): what the person approves, project by project.
func tidyTree(c *Catalog, mount string, ms []TidyMove) map[string][2][]string {
	card := regexp.MustCompile(`(?i)\.(rdc|rdm)$|^(bpav|clip|private|dcim|xdroot|sub|thmbnl|general)$`)
	tops := map[string]bool{}
	for _, m := range ms {
		if m.OK {
			tops[strings.SplitN(m.From, "/", 2)[0]], tops[strings.SplitN(m.To, "/", 2)[0]] = true, true
		}
	}
	dirs := func(files []string) []string {
		set := map[string]bool{}
		for _, f := range files {
			parts := strings.Split(path.Dir(f), "/")
			for i := range min(len(parts), 4) {
				if card.MatchString(parts[i]) || parts[i] == "." {
					break
				}
				set[strings.Join(parts[:i+1], "/")] = true
			}
		}
		return slices.Sorted(maps.Keys(set))
	}
	var before []string
	for p := range c.files {
		if rel, err := filepath.Rel(mount, p); err == nil && !strings.HasPrefix(rel, "..") && tops[strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]] {
			before = append(before, filepath.ToSlash(rel))
		}
	}
	after := make([]string, len(before))
	for i, f := range before {
		after[i] = f
		for _, m := range ms {
			if rest, ok := strings.CutPrefix(f, m.From); ok && m.OK && (rest == "" || strings.HasPrefix(rest, "/")) {
				after[i] = m.To + rest
				break
			}
		}
	}
	out := map[string][2][]string{}
	for top := range tops {
		var b, a []string
		for _, d := range dirs(before) {
			if strings.SplitN(d, "/", 2)[0] == top {
				b = append(b, d)
			}
		}
		for _, d := range dirs(after) {
			if strings.SplitN(d, "/", 2)[0] == top {
				a = append(a, d)
			}
		}
		out[top] = [2][]string{b, a}
	}
	return out
}

func cond(old, s string) string {
	if old != "" {
		return old
	}
	return s
}

type tidyUndo struct {
	UUID  string     `json:"uuid"`
	At    string     `json:"at"`
	Moves []TidyMove `json:"moves"` // done, in order
	Made  []string   `json:"made"`  // folders created for them, relative, parents first
}

// tidyApply renames the moves and writes the undo log; it stops at the first failure, the log keeps what was done.
func tidyApply(dir, uuid, mount string, ms []TidyMove) (int, error) {
	u := tidyUndo{UUID: uuid, At: time.Now().UTC().Format(time.RFC3339)}
	if prev, ok := readUndo(dir, uuid); ok { // a second tidy-up on the drive: Undo puts back both
		u.Moves, u.Made = prev.Moves, prev.Made
	}
	var err error
	done := 0 // moves of this tidy-up
	for _, m := range ms {
		var made []string
		for p := path.Dir(m.To); p != "."; p = path.Dir(p) {
			if _, e := os.Lstat(filepath.Join(mount, filepath.FromSlash(p))); e == nil {
				break
			}
			made = append([]string{p}, made...)
		}
		if err = os.MkdirAll(filepath.Join(mount, filepath.FromSlash(path.Dir(m.To))), 0o755); err != nil {
			break
		}
		u.Made = append(u.Made, made...)
		if err = os.Rename(filepath.Join(mount, filepath.FromSlash(m.From)), filepath.Join(mount, filepath.FromSlash(m.To))); err != nil {
			break
		}
		u.Moves = append(u.Moves, m)
		done++
	}
	if e := carryHashes(dir, uuid, mount, u.Moves[len(u.Moves)-done:], false); e != nil && err == nil {
		err = e
	}
	j, _ := json.MarshalIndent(u, "", "  ")
	os.MkdirAll(filepath.Dir(undoFile(dir, uuid)), 0o755) // the drive's folder in the catalog, there after its first scan
	if e := os.WriteFile(undoFile(dir, uuid), j, 0o600); e != nil && err == nil {
		err = e
	}
	return done, err
}

// tidyRevert puts back the last tidy, newest move first, and removes the folders it created if empty.
func tidyRevert(dir, mount string, u tidyUndo) (int, error) {
	n := 0
	for i := len(u.Moves) - 1; i >= 0; i-- {
		m := u.Moves[i]
		from, to := filepath.Join(mount, filepath.FromSlash(m.From)), filepath.Join(mount, filepath.FromSlash(m.To))
		var err error
		if _, e := os.Lstat(from); e == nil {
			err = fmt.Errorf("%s is in the way of the undo", m.From)
		} else if err = os.MkdirAll(filepath.Dir(from), 0o755); err == nil {
			err = os.Rename(to, from)
		}
		if err != nil { // the log keeps what is still to put back
			u.Moves = u.Moves[:i+1]
			j, _ := json.MarshalIndent(u, "", "  ")
			os.WriteFile(undoFile(dir, u.UUID), j, 0o600)
			return n, err
		}
		n++
	}
	carryHashes(dir, u.UUID, mount, u.Moves, true)
	for i := len(u.Made) - 1; i >= 0; i-- {
		os.Remove(filepath.Join(mount, filepath.FromSlash(u.Made[i]))) // only if empty
	}
	os.Remove(filepath.Join(dir, tidyUndoFile)) // as earlier versions kept it, when it was this drive's
	return n, os.Remove(undoFile(dir, u.UUID))
}

// carryHashes keeps the byte-for-byte checks of the files the moves renamed (fullhash.tsv): a rename changes their
// path, not their bytes, size or modification time, so their hash is written again under the new path. back: the
// moves are being undone, newest first.
func carryHashes(dir, uuid, mount string, ms []TidyMove, back bool) error {
	file := filepath.Join(dir, "disks", uuid, "fullhash.tsv")
	b, err := os.ReadFile(file)
	if err != nil || len(ms) == 0 {
		return nil // nothing checked on this drive yet
	}
	var add strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		c := strings.Split(line, "\t")
		if len(c) != 4 {
			continue
		}
		p := c[1]
		for i := range ms {
			m := ms[i]
			from, to := m.From, m.To
			if back {
				m = ms[len(ms)-1-i]
				from, to = m.To, m.From
			}
			old := filepath.Join(mount, filepath.FromSlash(from))
			if p == old || strings.HasPrefix(p, old+string(filepath.Separator)) {
				p = filepath.Join(mount, filepath.FromSlash(to)) + p[len(old):]
			}
		}
		if p != c[1] {
			fmt.Fprintf(&add, "%s\t%s\t%s\t%s\n", c[0], p, c[2], c[3])
		}
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(add.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// undoFile: each drive keeps the log of its last tidy-up, so tidying several drives keeps each one's undo.
func undoFile(dir, uuid string) string {
	return filepath.Join(dir, "disks", filepath.Base(uuid), tidyUndoFile)
}

func readUndo(dir, uuid string) (tidyUndo, bool) {
	var u tidyUndo
	b, err := os.ReadFile(undoFile(dir, uuid))
	if err != nil { // earlier versions kept one, for the last drive tidied, in the catalog folder
		b, err = os.ReadFile(filepath.Join(dir, tidyUndoFile))
	}
	return u, err == nil && json.Unmarshal(b, &u) == nil && u.UUID == uuid && len(u.Moves) > 0
}

func (a *App) tidyMount(uuid string) string {
	a.smu.Lock()
	defer a.smu.Unlock()
	return a.mounted[uuid]
}

// tidySite is the team site when it tidies with an AI of its own (it says so in /api/site), with who receives the
// description; nil otherwise, and for a drive kept on this computer (Local), which never reaches the site: then the
// app uses its own AI.
func (a *App) tidySite(uuid string) (*Team, string) {
	t := a.teamConf()
	if on(a.catalog().device(uuid).Local) {
		return nil, ""
	}
	var site struct {
		Tidy bool
		AI   string
	}
	if t == nil || t.call("GET", "/api/site", nil, &site) != nil || !site.Tidy {
		return nil, ""
	}
	return t, cmp.Or(site.AI, providers["anthropic"].Name) // a site from before the choice of AI has only Anthropic
}

// tidyAI is the AI this computer uses alone, as saved under Advanced; earlier versions saved an Anthropic key alone.
func (a *App) tidyAI() AI {
	var f AI
	if b, err := os.ReadFile(filepath.Join(a.dir, tidyAIFile)); err == nil {
		json.Unmarshal(b, &f)
	} else if k, err := os.ReadFile(filepath.Join(a.dir, tidyOldKeyFile)); err == nil {
		f = AI{Provider: "anthropic", Key: string(k)}
	}
	ai, _ := newAI(f.Provider, f.Model, f.URL, f.Key) // checked again: none when the file is not a valid choice
	return ai
}

// ready: an AI is chosen and has a key, unless it is an OpenAI-compatible server (a local one may need none).
func (ai AI) ready() bool { return ai.Provider != "" && (ai.Key != "" || ai.Provider == "compatible") }

// tidyRoutes: the app's side. The key never leaves this computer except to the provider the user chose.
func (a *App) tidyRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/tidy", func(w http.ResponseWriter, r *http.Request) {
		ai := a.tidyAI()
		res := map[string]any{"providers": providers, "site": "", "to": ai.To(), "ready": ai.ready(), // to: who receives a description
			"ai": map[string]any{"provider": ai.Provider, "model": ai.Model, "url": ai.URL, "key": ai.Key != ""}} // never the key itself
		if t, to := a.tidySite(r.URL.Query().Get("uuid")); t != nil { // without a drive: as for one sent to the team
			res["site"], res["to"] = strings.TrimPrefix(strings.TrimPrefix(t.URL, "https://"), "http://"), to
		}
		undo := []map[string]any{} // the last tidy-up of each drive that can still be undone
		for _, d := range a.catalog().Devices {
			if u, ok := readUndo(a.dir, d.UUID); ok {
				undo = append(undo, map[string]any{"uuid": u.UUID, "moves": len(u.Moves), "at": u.At})
			}
		}
		res["undo"] = undo
		send(w, res)
	})
	// ai saves the choice; an empty key keeps the saved one only for the same provider at the same address, and no
	// provider removes it all.
	m.HandleFunc("POST /api/tidy/ai", func(w http.ResponseWriter, r *http.Request) {
		var b AI
		json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b)
		f := filepath.Join(a.dir, tidyAIFile)
		os.Remove(filepath.Join(a.dir, tidyOldKeyFile))
		if b.Provider == "" {
			os.Remove(f)
			send(w, map[string]any{})
			return
		}
		ai, err := newAI(b.Provider, b.Model, b.URL, b.Key)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if old := a.tidyAI(); ai.Key == "" && old.Provider == ai.Provider && old.URL == ai.URL {
			ai.Key = old.Key
		}
		j, _ := json.Marshal(ai)
		if err := os.WriteFile(f, j, 0o600); err != nil || os.Chmod(f, 0o600) != nil {
			http.Error(w, "the choice of AI cannot be saved", 500)
			return
		}
		send(w, map[string]any{"ready": ai.ready()})
	})
	// propose: consent is per drive and per way to the AI ("site", or who receives it with the user's own AI); the
	// page says which one the user agreed to.
	m.HandleFunc("POST /api/tidy/propose", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			UUID, Via, Lang string
			Consent         bool
		}
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
		mount := a.tidyMount(b.UUID)
		if mount == "" {
			http.Error(w, "drive not connected", 404)
			return
		}
		site, to := a.tidySite(b.UUID)
		ai, via, ck := a.tidyAI(), "site", ""
		if site != nil {
			ck = b.UUID + " via " + site.URL + " to " + to
		} else if ai.ready() {
			via, ck = ai.To(), b.UUID+" to "+ai.To()
		} else {
			http.Error(w, "choose an AI and add its API key first, in Advanced", 400)
			return
		}
		consented := map[string]bool{}
		cf := filepath.Join(a.dir, tidyConsentFile)
		if j, err := os.ReadFile(cf); err == nil {
			json.Unmarshal(j, &consented)
		}
		if !consented[ck] {
			if !b.Consent || b.Via != via {
				http.Error(w, "consent needed", 409)
				return
			}
			consented[ck] = true
			j, _ := json.Marshal(consented)
			os.WriteFile(cf, j, 0o600)
		}
		desc, err := describe(a.catalog(), b.UUID, mount)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		desc += "\nWrite every reason in " + cmp.Or(languageNames[b.Lang], "English") + ", the language of the person reading them."
		ask := func(desc string) (ms []TidyMove, err error) {
			if site != nil {
				ts := *site
				ts.wait = 10 * time.Minute // the AI may take minutes, as with the user's own
				err = ts.call("POST", "/api/sync/tidy", map[string]string{"desc": desc}, &ms)
			} else {
				ms, err = propose(r.Context(), ai, desc)
			}
			return ms, err
		}
		c, skip := a.catalog(), a.catalog().device(b.UUID).Excluded
		proj := map[string]string{} // top folder on this drive → its project (joined across drives, renamed)
		for _, cl := range c.list {
			if cl.Device == b.UUID {
				proj[part(cl.ID, 0)] = cl.Project
			}
		}
		check := func(ms []TidyMove) ([]TidyMove, []TidyGap) {
			ms = validTidy(mount, skip, ms)
			for i := range ms {
				ms[i].Kind = tidyKind(c, mount, ms[i].From)
				if top, _, in := strings.Cut(ms[i].From, "/"); in { // a file at the top of the drive is in no project
					ms[i].Proj = cmp.Or(proj[top], top)
				}
			}
			return ms, tidyGaps(c, mount, skip, ms)
		}
		ms, err := ask(desc)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		ms, gaps := check(ms)
		if len(gaps) > 0 { // once more, with what rushbin found: the AI completes its own plan
			var prev []map[string]string
			for _, m := range ms {
				if m.OK {
					prev = append(prev, map[string]string{"from": m.From, "to": m.To})
				}
			}
			j, _ := json.Marshal(prev)
			if again, err := ask(desc + "\n\nYour plan was:\n" + string(j) + "\nrushbin checked it on the drive: it leaves the drive half tidied.\n- " +
				strings.Join(func() (l []string) {
					for _, g := range gaps {
						l = append(l, g.String())
					}
					return
				}(), "\n- ") + "\nAnswer with the whole plan again, complete: the same treatment for everything of the same kind."); err == nil {
				if ms2, gaps2 := check(again); len(gaps2) <= len(gaps) {
					ms, gaps = ms2, gaps2
				}
			}
		}
		a.smu.Lock()
		if a.tidy == nil {
			a.tidy = map[string][]TidyMove{}
		}
		a.tidy[b.UUID] = ms
		a.smu.Unlock()
		send(w, map[string]any{"moves": ms, "gaps": gaps, "tree": tidyTree(c, mount, ms)})
	})
	// apply takes indexes into the plan this engine holds: no path comes from the page. Checked again now.
	m.HandleFunc("POST /api/tidy/apply", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			UUID  string
			Moves []int
		}
		json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b)
		a.wmu.Lock()
		defer a.wmu.Unlock()
		mount := a.tidyMount(b.UUID)
		a.smu.Lock()
		plan := a.tidy[b.UUID]
		delete(a.tidy, b.UUID)
		a.smu.Unlock()
		if mount == "" || plan == nil {
			http.Error(w, "no plan for a connected drive: ask again", 409)
			return
		}
		var pick []TidyMove
		for _, i := range b.Moves {
			if i >= 0 && i < len(plan) && !slices.ContainsFunc(pick, func(m TidyMove) bool { return m == plan[i] }) {
				pick = append(pick, plan[i])
			}
		}
		var ok []TidyMove
		for _, m := range validTidy(mount, a.catalog().device(b.UUID).Excluded, pick) {
			if m.OK {
				ok = append(ok, m)
			}
		}
		n, err := tidyApply(a.dir, b.UUID, mount, ok)
		a.smu.Lock() // enqueue keeps its queue under smu
		a.enqueue(mount)
		a.smu.Unlock()
		res := map[string]any{"moved": n, "skipped": len(pick) - len(ok)}
		if err != nil {
			res["error"] = err.Error()
		}
		send(w, res)
	})
	m.HandleFunc("POST /api/tidy/undo", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ UUID string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
		a.wmu.Lock()
		defer a.wmu.Unlock()
		u, ok := readUndo(a.dir, b.UUID)
		mount := a.tidyMount(u.UUID)
		if !ok || mount == "" {
			http.Error(w, "nothing to undo on a connected drive", 409)
			return
		}
		n, err := tidyRevert(a.dir, mount, u)
		a.smu.Lock() // enqueue keeps its queue under smu
		a.enqueue(mount)
		a.smu.Unlock()
		res := map[string]any{"moved": n}
		if err != nil {
			res["error"] = err.Error()
		}
		send(w, res)
	})
}

// tidy (on the team site): the app sends the description of a drive, the site asks its own AI and
// answers the proposed moves as they are: the app checks each against the drive, as with its own AI. The site
// never sees the drive, moves nothing, keeps nothing of the description and logs no names. A team runs at most
// tidyCap a calendar month (each request to the AI counts); at most 10 from one address in 15 minutes.
func (s *Server) tidy(w http.ResponseWriter, r *http.Request, u *User) {
	if !s.ai.ready() {
		http.Error(w, "This team site has no AI for tidy-ups: use your own in the app, under Advanced", 404)
		return
	}
	var b struct{ Desc string }
	if json.NewDecoder(io.LimitReader(r.Body, 2*tidyMaxDesc)).Decode(&b) != nil || strings.TrimSpace(b.Desc) == "" {
		http.Error(w, "bad request", 400)
		return
	}
	ip := "tidy:" + s.clientIP(r)
	if s.limited(10, ip) {
		http.Error(w, "Too many tidy-ups from here: try again in 15 minutes", 429)
		return
	}
	err := s.inTeam(r.Context(), u.Team, func(tx pgx.Tx) error {
		var n int // the team's row locked: two tidy-ups at once cannot both take the last one
		if _, err := tx.Exec(r.Context(), "select 1 from team where id = $1 for update", u.Team); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), "select count(*) from tidy_run where team = $1 and t >= date_trunc('month', now())", u.Team).Scan(&n); err != nil {
			return err
		}
		if n >= s.tidyCap {
			return httpError{429, fmt.Sprintf("Your team has used its %d folder tidy-ups for this month: more from the first of next month", s.tidyCap)}
		}
		_, err := tx.Exec(r.Context(), "insert into tidy_run (team) values ($1)", u.Team)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.failed(ip)
	ms, err := propose(r.Context(), s.ai, b.Desc)
	if err != nil {
		log.Printf("tidy: team %d: %v", u.Team, err) // the error, never the description
		http.Error(w, err.Error(), 502)
		return
	}
	send(w, ms)
}
