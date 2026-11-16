package main

import (
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// The emails: every language of the pages has every text of English with the same {placeholders}, no long dash and
// no emoji; the language follows the Accept-Language of the request that makes the email (onboarding_test.go
// checks which request that is for each email).
func TestMailWords(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(mailWords)), slices.Sorted(slices.Values(languages(t))); !slices.Equal(got, want) {
		t.Fatalf("emails in %v, pages in %v", got, want)
	}
	en := mailWords["en"]
	for lang, w := range mailWords {
		if got, want := slices.Sorted(maps.Keys(w)), slices.Sorted(maps.Keys(en)); !slices.Equal(got, want) {
			t.Errorf("%s: keys %v, English %v", lang, got, want)
		}
		for key, s := range w {
			if have, want := captures(placeholderRE, s), captures(placeholderRE, en[key]); s == "" || !slices.Equal(have, want) {
				t.Errorf("%s: %q has placeholders %v, English %v: %q", lang, key, have, want, s)
			}
			if strings.ContainsAny(s, dashes) || strings.ContainsFunc(s, func(r rune) bool { return r >= 0x1F000 || unicode.Is(unicode.So, r) }) {
				t.Errorf("%s: %q has a long dash or an emoji: %q", lang, key, s)
			}
		}
		for _, kind := range []string{"signup", "forgot", "reset", "invite"} {
			l := letter(lang, kind, "https://team.example.com/login#x", "name", "Ann", "team", "Studio", "role", w["role.editor"])
			if page := l.html() + l.plain(); strings.Contains(page, "{") || !strings.Contains(page, `<html lang="`+lang+`">`) ||
				!strings.Contains(page, strings.ReplaceAll(w["sent"], "{host}", "team.example.com")) {
				t.Errorf("%s %s: %s", lang, kind, page)
			}
		}
	}
	if l := (Letter{Link: "https://x.example.com"}); !strings.Contains(l.html(), `<html lang="en">`) {
		t.Error("a letter without a language is not in English")
	}
}

func TestMailLang(t *testing.T) {
	for accept, want := range map[string]string{
		"":                           "en",
		"it-IT,it;q=0.9,en-US;q=0.8": "it",
		"en-US,en;q=0.9,it;q=0.8":    "en",
		"pt-PT,pt;q=0.9":             "pt-BR",
		"pt-br":                      "pt-BR",
		"zh-CN,ja;q=0.7,de;q=0.6":    "ja", // the first language the emails have
		"fr;q=0.5, es":               "es", // by weight, not by order
		"de;q=0.8,fr;q=0.8":          "de", // equal weights: the browser's order
		"ko,*;q=0.5":                 "en",
		"it;q=0,fr":                  "fr", // q=0: not acceptable
		"es;q=abc,FR-ca":             "fr",
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Accept-Language", accept)
		if got := mailLang(r); got != want {
			t.Errorf("Accept-Language %q: %s, want %s", accept, got, want)
		}
	}
	for chosen, want := range map[string]string{"ja": "ja", "pt-BR": "pt-BR", "xx": "it", "": "it"} { // the page's choice first
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Accept-Language", "it")
		r.Header.Set("X-Rushbin-Lang", chosen)
		if got := mailLang(r); got != want {
			t.Errorf("chosen %q on the page: %s, want %s", chosen, got, want)
		}
	}
}
