package obsidian

import (
	"strings"
	"testing"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
)

func sampleNote() *granola.Note {
	return &granola.Note{
		ID:            "f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c",
		Title:         "Quarterly budget review",
		CreatedAt:     time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 11, 18, 0, 45, 0, 0, time.UTC),
		Attendees:     []string{"Oat Benson", `Raisin "Ray" Patel`},
		NotesMarkdown: "- push back on the forecast\n",
		Panels:        []granola.Panel{{Title: "Summary", Markdown: "### Decisions\n\n- Cut spend"}},
		Transcript: []granola.Segment{
			{Start: time.Date(2026, 11, 17, 23, 30, 5, 0, time.UTC), Source: "microphone", Text: "Shall we start?"},
			{Start: time.Date(2026, 11, 17, 23, 30, 9, 0, time.UTC), Source: "microphone", Text: "I have *two* items."},
			{Start: time.Date(2026, 11, 17, 23, 31, 2, 0, time.UTC), Source: "system", Text: "Go ahead."},
			{Source: "", Text: "Untimed remark"},
		},
	}
}

func TestRender(t *testing.T) {
	got := Render(sampleNote(), Options{IncludeTranscript: true, Location: time.UTC})
	want := `---
title: "Quarterly budget review"
date: 2026-11-17
created: 2026-11-17T23:30:00
updated: 2026-11-18T00:45:00
granola_id: f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c
granola_url: https://notes.granola.ai/d/f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c
attendees:
  - "Oat Benson"
  - "Raisin \"Ray\" Patel"
tags:
  - granola
---

## Summary

### Decisions

- Cut spend

## My notes

- push back on the forecast

> [!quote]- Transcript
> **Me** 23:30: Shall we start? I have \*two\* items.
>
> **Them** 23:31: Go ahead.
>
> **Speaker**: Untimed remark
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderUsesTheGivenTimeZone(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*3600)
	got := Render(sampleNote(), Options{IncludeTranscript: true, Location: tokyo})
	for _, want := range []string{"date: 2026-11-18\n", "created: 2026-11-18T08:30:00\n", "updated: 2026-11-18T09:45:00\n", "> **Me** 08:30: "} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestRenderOmitsWhatIsAbsent(t *testing.T) {
	n := sampleNote()
	without := Render(n, Options{IncludeTranscript: false, Location: time.UTC})
	if strings.Contains(without, "Transcript") || strings.Contains(without, "Shall we start") {
		t.Errorf("transcript present although excluded:\n%s", without)
	}

	bare := Render(&granola.Note{ID: "odd id: 1", NotesMarkdown: "just this"}, Options{IncludeTranscript: true, Location: time.UTC})
	want := `---
title: "Untitled"
granola_id: "odd id: 1"
granola_url: https://notes.granola.ai/d/odd%20id:%201
tags:
  - granola
---

## My notes

just this
`
	if bare != want {
		t.Errorf("got:\n%s\nwant:\n%s", bare, want)
	}
}

func TestYAMLString(t *testing.T) {
	cases := map[string]string{
		`plain`:                 `"plain"`,
		`key: value # comment`:  `"key: value # comment"`,
		`say "hi" \ bye`:        `"say \"hi\" \\ bye"`,
		"two\nlines\tand tab":   `"two\nlines\tand tab"`,
		"bell\a and sep\u2028":  `"bell\u0007 and sep\u2028"`,
		"- [[link]] & {braces}": `"- [[link]] & {braces}"`,
		"émoji 🎉 and 日本語 stay":  `"émoji 🎉 and 日本語 stay"`,
	}
	for in, want := range cases {
		if got := yamlString(in); got != want {
			t.Errorf("yamlString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseIDRoundTrip(t *testing.T) {
	for _, id := range []string{
		"f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c",
		"not_1d3tmYTlCICgjy",
		"12345",
		`odd "id" with \ and: colon`,
		"line\nbreak",
	} {
		content := Render(&granola.Note{ID: id, Title: "t", NotesMarkdown: "x"}, Options{Location: time.UTC})
		got, ok := ParseID([]byte(content))
		if !ok || got != id {
			t.Errorf("id %q read back as %q (found %v)", id, got, ok)
		}
	}
}

func TestParseID(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"unquoted", "---\ntitle: x\ngranola_id: abc-123\n---\nbody", "abc-123"},
		{"reordered by Obsidian", "---\ntags:\n  - granola\n  - work\ngranola_id: abc-123\ntitle: x\n---\n", "abc-123"},
		{"single quoted", "---\ngranola_id: 'it''s'\n---\n", "it's"},
		{"double quoted", "---\ngranola_id: \"abc 123\"\n---\n", "abc 123"},
		{"windows line endings", "---\r\ngranola_id: abc-123\r\n---\r\n", "abc-123"},
		{"byte order mark", "\xef\xbb\xbf---\ngranola_id: abc-123\n---\n", "abc-123"},
		{"trailing space", "---\ngranola_id: abc-123  \n---\n", "abc-123"},
		{"no frontmatter", "# Heading\ngranola_id: abc-123\n", ""},
		{"id only in body", "---\ntitle: x\n---\ngranola_id: abc-123\n", ""},
		{"nested key is not the property", "---\nmeta:\n  granola_id: abc-123\n---\n", ""},
		{"empty value", "---\ngranola_id:\n---\n", ""},
		{"empty file", "", ""},
		{"unterminated frontmatter", "---\ntitle: x\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseID([]byte(c.content))
			if got != c.want || ok != (c.want != "") {
				t.Errorf("ParseID = %q, %v; want %q", got, ok, c.want)
			}
		})
	}
}

func TestSanitizeTitle(t *testing.T) {
	cases := map[string]string{
		"Quarterly budget review":      "Quarterly budget review",
		"Q3: Budget/Plan <draft>":      "Q3 Budget-Plan draft",
		`1:1 Oat | Raisin "sync"?`:     "11 Oat - Raisin sync",
		"Design [v2] #urgent ^block":   "Design v2 urgent block",
		"  spaced \t out\nlines  ":     "spaced out lines",
		"...hidden.":                   "hidden",
		"a\\b*c":                       "a-bc",
		"":                             "Untitled",
		"???":                          "Untitled",
		"/":                            "Untitled",
		"日本語のタイトル":                     "日本語のタイトル",
		"Café ☕ catch-up":              "Café ☕ catch-up",
		"bell\a and null\x00 stripped": "bell and null stripped",
	}
	for in, want := range cases {
		if got := SanitizeTitle(in); got != want {
			t.Errorf("SanitizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeTitleLength(t *testing.T) {
	if got := SanitizeTitle(strings.Repeat("word ", 60)); len([]rune(got)) > 100 || strings.HasSuffix(got, " ") {
		t.Errorf("long title not cut cleanly: %d runes, %q", len([]rune(got)), got)
	}
	wide := SanitizeTitle(strings.Repeat("語", 150))
	if len(wide) > 180 || !strings.HasPrefix(wide, "語") || strings.ContainsRune(wide, '\uFFFD') {
		t.Errorf("wide title is %d bytes or was cut inside a character", len(wide))
	}
}

func TestBaseName(t *testing.T) {
	n := sampleNote()
	if got := BaseName(n, time.UTC); got != "2026-11-17 Quarterly budget review" {
		t.Errorf("BaseName = %q", got)
	}
	if got := BaseName(n, time.FixedZone("JST", 9*3600)); got != "2026-11-18 Quarterly budget review" {
		t.Errorf("BaseName in Tokyo = %q, want the local date", got)
	}
	n.CreatedAt = time.Time{}
	if got := BaseName(n, time.UTC); got != "2026-11-18 Quarterly budget review" {
		t.Errorf("BaseName without a creation time = %q, want the update date", got)
	}
	n.UpdatedAt = time.Time{}
	if got := BaseName(n, time.UTC); got != "Quarterly budget review" {
		t.Errorf("BaseName without any time = %q", got)
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("f3e45e0f-24cc-480b", 8); got != "f3e45e0f" {
		t.Errorf("ShortID = %q", got)
	}
	if got := ShortID("a/b:c é", 8); got != "abc" {
		t.Errorf("ShortID = %q, want only ASCII letters and digits", got)
	}
}
