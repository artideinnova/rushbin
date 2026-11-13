package main

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Search by looks on a Mac in no team: the same as the site (search.go looksLike), here. The text half of
// SigLIP 2 runs in rushbin-vision (Core ML, on the Neural Engine); the words are turned into its token ids
// here (the Gemma tokenizer of SigLIP 2, testdata/siglip-text.json holds the ids of the site's transformers);
// the nearest frames are a plain scan of the previews' emb.bin (1.4 ms over the 2440 frames of the T9).
// In a team the site answers, with every Mac's frames. On Windows, alone, words only for this beta.

// siglipText is downloaded with the speech models on a Mac in no team (macos/siglip/convert-text.py made it
// from google/siglip2-base-patch16-256, Apache-2.0), pinned to the commit of the upload on Hugging Face.
var siglipText = []model{
	{"SigLIP2Text.mlmodelc.zip", "https://huggingface.co/artideinnova/siglip2-base-patch16-256-text-coreml/resolve/90d41162d15a9f0ccf8ca340bf0cb55bd5d0fb28/SigLIP2Text.mlmodelc.zip",
		"3ecdb11c635acf687d1400958be31ee20c146525616dd4b5c3a374c1cc1e4a7e", 530503967},
	{"siglip2-tokenizer.model", "https://huggingface.co/artideinnova/siglip2-base-patch16-256-text-coreml/resolve/90d41162d15a9f0ccf8ca340bf0cb55bd5d0fb28/tokenizer.model",
		"61a7b147390c64585d6c3543dd6fc636906c9af3865a5548f27f31aee1d4c8e2", 4241003},
}

// wantText: this Mac searches by looks itself (no team, search by content on for a drive, the helper there).
func (a *App) wantText() bool {
	if runtime.GOOS != "darwin" || a.teamConf() != nil {
		return false
	}
	if _, err := exec.LookPath(tool("rushbin-vision")); err != nil {
		return false
	}
	for _, d := range a.catalog().Devices {
		if on(d.Content) {
			return true
		}
	}
	return false
}

// textEncoder is rushbin-vision in text mode, kept running between searches (the model is mapped from the
// file: 18 MB of memory of its own, measured with time -l).
type textEncoder struct {
	tok *tokenizer
	cmd *exec.Cmd
	in  io.Writer
	out *bufio.Reader
}

// embedLocal turns the words of a search into a SigLIP 2 vector on this Mac; nil while the model is missing.
func (a *App) embedLocal(q string) ([]float32, error) {
	dir := filepath.Join(a.dir, "models")
	if runtime.GOOS != "darwin" || !siglipText[0].here(dir) || !siglipText[1].here(dir) {
		return nil, nil
	}
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.tenc == nil {
		tok, err := loadTokenizer(siglipText[1].path(dir))
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(tool("rushbin-vision"), "text", siglipText[0].path(dir))
		cmd.Stderr = log.Writer()
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		a.tenc = &textEncoder{tok, cmd, in, bufio.NewReader(out)}
	}
	e := a.tenc
	ids := e.tok.ids(q)
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.Itoa(int(id))
	}
	var v []float32
	_, err := io.WriteString(e.in, strings.Join(s, " ")+"\n")
	if err == nil {
		var line []byte
		if line, err = e.out.ReadBytes('\n'); err == nil {
			err = json.Unmarshal(line, &v)
		}
	}
	if err != nil || len(v) != embDim {
		e.cmd.Process.Kill() // the next search starts it again
		e.cmd.Wait()
		a.tenc = nil
		return nil, fmt.Errorf("text encoder: %d numbers (%v)", len(v), err)
	}
	return v, nil
}

// nearest finds the contents whose frames are nearest to v, best frame per content, like the site: the 300
// nearest frames by cosine, at most 100 contents. The frames are read once per catalog (reload makes a new one).
func (c *Catalog) nearest(thumbs string, v []float32) []Hit {
	c.embOnce.Do(func() {
		seen := map[string]bool{}
		for _, cl := range c.list {
			if seen[cl.Thumb] {
				continue
			}
			seen[cl.Thumb] = true
			b, err := os.ReadFile(filepath.Join(thumbs, cl.Thumb, "emb.bin"))
			if err != nil {
				continue
			}
			fs, err := frames(b)
			if err != nil {
				log.Printf("%s/emb.bin: %v", cl.Thumb, err)
				continue
			}
			for _, f := range fs {
				c.embThumb, c.embV = append(c.embThumb, cl.Thumb), append(c.embV, unit(f))
			}
		}
	})
	v = unit(v)
	score := make([]float32, len(c.embV))
	order := make([]int, len(c.embV))
	for i, f := range c.embV {
		var d float32
		for j, x := range f {
			d += x * v[j]
		}
		score[i], order[i] = d, i
	}
	sort.Slice(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	var hits []Hit
	got := map[string]bool{}
	for _, i := range order[:min(300, len(order))] {
		if th := c.embThumb[i]; !got[th] && len(hits) < 100 {
			got[th] = true
			hits = append(hits, Hit{th, float64(score[i])})
		}
	}
	return hits
}

func unit(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / math.Sqrt(n))
	}
	return out
}

// tokenizer is SigLIP 2's: Gemma's SentencePiece BPE (tokenizer.model), as transformers applies it: spaces
// become ▁ (no prefix, no lower case), its control and user-defined pieces (runs of spaces, <eos>…) are taken
// whole, leftmost and longest first; the rest is merged by BPE, highest score first; a character outside the
// vocabulary becomes its UTF-8 bytes.
type tokenizer struct {
	normal map[string]int32 // pieces BPE merges into
	added  map[string]int32 // pieces taken whole
	score  []float32
	bytes  [256]int32
}

// loadTokenizer reads a SentencePiece model: a protobuf whose field 1 repeats the pieces (1 text, 2 score, 3 type).
func loadTokenizer(file string) (*tokenizer, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	t := &tokenizer{normal: map[string]int32{}, added: map[string]int32{}}
	for len(b) > 0 {
		num, data, rest, err := pbField(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		b = rest
		if num != 1 {
			continue
		}
		id := int32(len(t.score))
		var piece string
		var score float32
		kind := uint64(1) // NORMAL, when not said
		for len(data) > 0 {
			n, d, r, err := pbField(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", file, err)
			}
			data = r
			switch n {
			case 1:
				piece = string(d)
			case 2:
				score = math.Float32frombits(binary.LittleEndian.Uint32(d))
			case 3:
				kind, _ = binary.Uvarint(d)
			}
		}
		t.score = append(t.score, score)
		switch kind {
		case 1:
			t.normal[piece] = id
		case 2, 3, 4: // unknown, control, user-defined: what transformers lists as added tokens
			t.added[piece] = id
		case 6: // <0xAB>
			if x, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(piece, "<0x"), ">"), 16, 8); err == nil {
				t.bytes[x] = id
			}
		}
	}
	if len(t.normal) == 0 || t.added["<eos>"] != 1 || t.added["<pad>"] != 0 {
		return nil, fmt.Errorf("%s: not the SigLIP 2 tokenizer", file)
	}
	return t, nil
}

// pbField reads one protobuf field: its number and its bytes (the varint's own bytes for a varint).
func pbField(b []byte) (num int, data, rest []byte, err error) {
	key, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, nil, nil, errors.New("bad protobuf")
	}
	b = b[n:]
	switch key & 7 {
	case 0:
		_, m := binary.Uvarint(b)
		if m <= 0 {
			return 0, nil, nil, errors.New("bad protobuf")
		}
		return int(key >> 3), b[:m], b[m:], nil
	case 1, 5:
		size := map[uint64]int{1: 8, 5: 4}[key&7]
		if len(b) < size {
			return 0, nil, nil, errors.New("bad protobuf")
		}
		return int(key >> 3), b[:size], b[size:], nil
	case 2:
		l, m := binary.Uvarint(b)
		if m <= 0 || uint64(len(b)-m) < l {
			return 0, nil, nil, errors.New("bad protobuf")
		}
		return int(key >> 3), b[m : m+int(l)], b[m+int(l):], nil
	}
	return 0, nil, nil, errors.New("bad protobuf")
}

// ids is the model's input, as deploy/text/server.py makes it: the first 300 characters, then <eos>, padded
// with <pad> to 64 (cut to 63 before <eos>).
func (t *tokenizer) ids(q string) []int32 {
	if r := []rune(q); len(r) > 300 {
		q = string(r[:300])
	}
	ids := t.encode(q)
	ids = append(ids[:min(len(ids), 63)], t.added["<eos>"])
	for len(ids) < 64 {
		ids = append(ids, t.added["<pad>"])
	}
	return ids
}

func (t *tokenizer) encode(q string) []int32 {
	s := strings.ReplaceAll(q, " ", "▁")
	var ids []int32
	for s != "" {
		at, piece := len(s), ""
		for i := range s { // the leftmost added piece, the longest there
			for p := range t.added {
				if len(p) > len(piece) && strings.HasPrefix(s[i:], p) {
					at, piece = i, p
				}
			}
			if piece != "" {
				break
			}
		}
		ids = append(ids, t.bpe(s[:at])...)
		if piece == "" {
			break
		}
		ids = append(ids, t.added[piece])
		s = s[at+len(piece):]
	}
	return ids
}

// bpe merges the characters of s, the pair making the piece with the highest score first (the leftmost on a tie).
// ponytail: quadratic in the characters; fine for 300.
func (t *tokenizer) bpe(s string) []int32 {
	syms := make([]string, 0, utf8.RuneCountInString(s))
	for _, r := range s {
		syms = append(syms, string(r))
	}
	for {
		at, best := -1, float32(math.Inf(-1))
		for i := 0; i+1 < len(syms); i++ {
			if id, ok := t.normal[syms[i]+syms[i+1]]; ok && t.score[id] > best {
				at, best = i, t.score[id]
			}
		}
		if at < 0 {
			break
		}
		syms[at] += syms[at+1]
		syms = append(syms[:at+1], syms[at+2:]...)
	}
	var ids []int32
	for _, p := range syms {
		if id, ok := t.normal[p]; ok {
			ids = append(ids, id)
			continue
		}
		for _, c := range []byte(p) {
			ids = append(ids, t.bytes[c])
		}
	}
	return ids
}

// unzip unpacks a downloaded model folder into dst, all or nothing.
func unzip(file, dst string) error {
	r, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer r.Close()
	tmp := dst + ".part"
	os.RemoveAll(tmp)
	top := filepath.Base(dst) + "/" // the archive holds the folder itself
	for _, f := range r.File {
		rel, ok := strings.CutPrefix(f.Name, top)
		if !ok || rel != "" && !filepath.IsLocal(rel) {
			return fmt.Errorf("%s: unexpected %q", filepath.Base(file), f.Name)
		}
		p := filepath.Join(tmp, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(p)
		if err == nil {
			_, err = io.Copy(out, in)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		in.Close()
		if err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}
