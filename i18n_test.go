package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The pages' languages (web/i18n.js, web/i18n/*.js) and the Mac shell's (macos/*.lproj): every language has every
// key of English with the same {placeholders} and markup, the pages use only keys English has and English has no
// key they do not use, the server messages are still the words the Go code says, and no text has a long dash.

var (
	placeholderRE = regexp.MustCompile(`\{(\w+)\}`)
	tagRE         = regexp.MustCompile(`</?([a-z]+)`)
	dashes        = "–—―" // en dash, em dash, horizontal bar: never, in any language
)

// words reads web/i18n/<lang>.js: I18N.words({...JSON...});
func words(t *testing.T, lang string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("web", "i18n", lang+".js"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, "I18N.words("), strings.LastIndex(s, ")")
	if i < 0 || j < i {
		t.Fatalf("%s.js: no I18N.words(...)", lang)
	}
	var w map[string]any
	if err := json.Unmarshal([]byte(s[i+len("I18N.words("):j]), &w); err != nil {
		t.Fatalf("%s.js: %v", lang, err)
	}
	return w
}

// languages: the codes web/i18n.js offers.
func languages(t *testing.T) []string {
	b, err := os.ReadFile("web/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`NAMES = \{([^}]*)\}`).FindSubmatch(b)
	if names == nil {
		t.Fatal("web/i18n.js: no NAMES")
	}
	var langs []string
	for _, m := range regexp.MustCompile(`'?([a-zA-Z-]+)'?:`).FindAllSubmatch(names[1], -1) {
		langs = append(langs, string(m[1]))
	}
	return langs
}

func captures(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return out
}

// forms: a value's text, or its plural forms by category.
func forms(v any) map[string]string {
	switch v := v.(type) {
	case string:
		return map[string]string{"": v}
	case map[string]any:
		f := map[string]string{}
		for k, x := range v {
			s, _ := x.(string)
			f[k] = s
		}
		return f
	}
	return nil
}

func TestTranslations(t *testing.T) {
	en := words(t, "en")
	for _, lang := range languages(t) {
		t.Run(lang, func(t *testing.T) { translation(t, lang, en) })
	}
}

func translation(t *testing.T, lang string, en map[string]any) {
	plurals := []string{"zero", "one", "two", "few", "many", "other"}
	{
		w := words(t, lang)
		check := func(key string, want, got any) {
			ef, gf := forms(want), forms(got)
			if gf == nil {
				t.Errorf("%s: %q is missing", lang, key)
				return
			}
			if _, plural := ef["other"]; plural != (gf["other"] != "" || len(gf) > 1) {
				t.Errorf("%s: %q must be %s, as in English", lang, key, map[bool]string{true: "plural forms", false: "one text"}[plural])
				return
			}
			ref := ef[""] + ef["other"]
			for form, s := range gf {
				if form != "" && !slices.Contains(plurals, form) {
					t.Errorf("%s: %q has the plural form %q", lang, key, form)
				}
				if s == "" && ref != "" {
					t.Errorf("%s: %q is empty", lang, key)
				}
				want, have := captures(placeholderRE, ref), captures(placeholderRE, s)
				if form != "other" && form != "" && !slices.Contains(have, "n") { // "one" may say the number in words
					want = slices.DeleteFunc(slices.Clone(want), func(x string) bool { return x == "n" })
				}
				if !slices.Equal(want, have) {
					t.Errorf("%s: %q has placeholders %v, English %v: %q", lang, key, have, want, s)
				}
				if a, b := captures(tagRE, ref), captures(tagRE, s); !slices.Equal(a, b) {
					t.Errorf("%s: %q has markup %v, English %v", lang, key, b, a)
				}
				if strings.ContainsAny(s, dashes) {
					t.Errorf("%s: %q has a long dash: %q", lang, key, s)
				}
				if strings.Contains(s, `"`) { // the words also go into attributes: typographic quotes only
					t.Errorf("%s: %q has a straight double quote: %q", lang, key, s)
				}
			}
		}
		for key, v := range en {
			if key == "server" {
				ew, _ := v.(map[string]any)
				lw, _ := w["server"].(map[string]any)
				for k, x := range ew {
					check("server: "+k, x, lw[k])
				}
				for k := range lw {
					if _, ok := ew[k]; !ok {
						t.Errorf("%s: server %q is not in English", lang, k)
					}
				}
				continue
			}
			check(key, v, w[key])
		}
		for key := range w {
			if _, ok := en[key]; !ok {
				t.Errorf("%s: %q is not in English", lang, key)
			}
		}
	}
}

// The pages ask only for keys English has, and English has only keys the pages ask for.
func TestTranslationKeysUsed(t *testing.T) {
	en := words(t, "en")
	used := map[string]bool{}
	for _, f := range []string{"web/index.html", "web/login.html", "web/i18n.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// t('key'), t(a ? 'key' : 'other key') in the app, data-i18n="key" in the markup
		res := []string{`\bt\('([\w.]+)'`, `data-i18n(?:-[a-z-]+)?="([\w.]+)"`}
		if f == "web/index.html" {
			res = append(res, `[?:] '([a-z]+\.[\w.]+)'`)
		}
		for _, re := range res {
			for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(string(b), -1) {
				used[m[1]] = true
			}
		}
	}
	for k := range used {
		if _, ok := en[k]; !ok {
			t.Errorf("%q is used but not in en.js", k)
		}
	}
	for k := range en {
		if k != "server" && !used[k] {
			t.Errorf("%q is in en.js but no page uses it", k)
		}
	}
}

// The server messages are keyed by the English the Go code says: a reworded message must be reworded there too.
func TestServerMessagesInGo(t *testing.T) {
	var src strings.Builder
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			b, _ := os.ReadFile(f)
			src.Write(b)
		}
	}
	server, _ := words(t, "en")["server"].(map[string]any)
	for k := range server {
		for _, part := range placeholderRE.Split(k, -1) {
			if !strings.Contains(src.String(), part) {
				t.Errorf("server message %q: %q is in no Go file", k, part)
			}
		}
	}
}

// The Mac shell: each language's Localizable.strings has every NSLocalizedString of main.swift, with its format
// verbs, and InfoPlist.strings the permission prompts of Info.plist.
func TestMacStrings(t *testing.T) {
	swift, err := os.ReadFile("macos/main.swift")
	if err != nil {
		t.Fatal(err)
	}
	plist, _ := os.ReadFile("macos/Info.plist")
	keys := captures(regexp.MustCompile(`NSLocalizedString\("([^"]+)"`), string(swift))
	keys = slices.Compact(keys)
	prompts := captures(regexp.MustCompile(`<key>(NS\w+UsageDescription)</key>`), string(plist))
	for _, lang := range languages(t) {
		if lang == "en" {
			continue // the keys are the English
		}
		t.Run(lang, func(t *testing.T) { macStrings(t, lang, keys, prompts) })
	}
}

func macStrings(t *testing.T, lang string, keys, prompts []string) {
	line := regexp.MustCompile(`(?m)^"((?:[^"\\]|\\.)*)"\s*=\s*"((?:[^"\\]|\\.)*)";`)
	verbs := regexp.MustCompile(`%[@d]`)
	{
		for file, want := range map[string][]string{"Localizable.strings": keys, "InfoPlist.strings": prompts} {
			b, err := os.ReadFile(filepath.Join("macos", lang+".lproj", file))
			if err != nil {
				t.Error(err)
				continue
			}
			got := map[string]string{}
			for _, m := range line.FindAllStringSubmatch(string(b), -1) {
				got[m[1]] = m[2]
			}
			for _, k := range want {
				v, ok := got[k]
				switch {
				case !ok || v == "":
					t.Errorf("%s/%s: %q is missing", lang, file, k)
				case !slices.Equal(verbs.FindAllString(k, -1), verbs.FindAllString(v, -1)):
					t.Errorf("%s/%s: %q has other format verbs: %q", lang, file, k, v)
				case strings.ContainsAny(v, dashes):
					t.Errorf("%s/%s: %q has a long dash", lang, file, k)
				}
			}
			if len(got) != len(want) {
				t.Errorf("%s/%s: %d strings, want %d", lang, file, len(got), len(want))
			}
		}
	}
}
