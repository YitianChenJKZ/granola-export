package prosemirror

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestGolden(t *testing.T) {
	inputs, err := filepath.Glob("testdata/*.json")
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no golden inputs found (err %v)", err)
	}
	for _, input := range inputs {
		name := strings.TrimSuffix(filepath.Base(input), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ToMarkdown(raw)
			if err != nil {
				t.Fatal(err)
			}
			got += "\n"
			golden := strings.TrimSuffix(input, ".json") + ".md"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

func text(s string, marks ...string) Node {
	n := Node{Type: "text", Text: s}
	for _, m := range marks {
		n.Marks = append(n.Marks, Mark{Type: m})
	}
	return n
}

func para(nodes ...Node) Node { return Node{Type: "paragraph", Content: nodes} }

func TestInline(t *testing.T) {
	link := Node{Type: "text", Text: "docs", Marks: []Mark{{Type: "link", Attrs: map[string]any{"href": "https://example.com/a_b"}}}}
	cases := []struct {
		name  string
		nodes []Node
		want  string
	}{
		{"plain", []Node{text("hello world")}, "hello world"},
		{"bold with trailing space", []Node{text("bold ", "bold"), text("plain")}, "**bold** plain"},
		{"bold with leading space", []Node{text("plain"), text(" bold", "bold")}, "plain **bold**"},
		{"adjacent same marks merge", []Node{text("one ", "bold"), text("two", "bold")}, "**one two**"},
		{"nested marks", []Node{text("a ", "bold"), text("b", "bold", "italic"), text(" c", "bold")}, "**a *b* c**"},
		{"strong and em aliases", []Node{text("x", "strong", "em")}, "***x***"},
		{"underline has no markdown", []Node{text("x", "underline")}, "x"},
		{"whitespace only between marks", []Node{text("a", "bold"), text(" "), text("b", "italic")}, "**a** *b*"},
		{"break inside bold", []Node{text("a", "bold"), {Type: "hardBreak"}, text("b", "bold")}, "**a  \nb**"},
		{"break before closing mark", []Node{text("a", "bold"), {Type: "hardBreak"}}, "**a**"},
		{"link", []Node{text("see "), link}, "see [docs](https://example.com/a_b)"},
		{"code is not escaped", []Node{text("a*b_c", "code")}, "`a*b_c`"},
		{"code with backtick", []Node{text("a`b", "code")}, "``a`b``"},
		{"escapes emphasis and links", []Node{text("2 * 3 [x] <b> \\ `t`")}, "2 \\* 3 \\[x\\] \\<b> \\\\ \\`t\\`"},
		{"intraword underscore kept", []Node{text("snake_case _lead trail_")}, "snake_case \\_lead trail\\_"},
		{"doubled tilde and equals", []Node{text("~5 a == b ~~x~~")}, "~5 a \\=\\= b \\~\\~x\\~\\~"},
		{"heading marker", []Node{text("# title")}, "\\# title"},
		{"hashtag kept", []Node{text("#project update")}, "#project update"},
		{"bullet marker", []Node{text("- item")}, "\\- item"},
		{"plus marker", []Node{text("+ item")}, "\\+ item"},
		{"quote marker", []Node{text(">quote")}, "\\>quote"},
		{"numbered marker", []Node{text("12. item")}, "12\\. item"},
		{"rule", []Node{text("---")}, "\\---"},
		{"setext underline", []Node{text("Title"), {Type: "hardBreak"}, text("=")}, "Title  \n\\="},
		{"indented second line", []Node{text("a"), {Type: "hardBreak"}, text("    b")}, "a  \nb"},
		{"empty", nil, ""},
		{"whitespace only", []Node{text("   ")}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := Node{Type: "doc", Content: []Node{para(c.nodes...)}}
			if got := Render(&doc); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestBlocks(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"snake_case schema",
			`{"type":"doc","content":[{"type":"bullet_list","content":[{"type":"list_item","content":[{"type":"paragraph","content":[{"type":"text","text":"a"}]}]}]},{"type":"code_block","content":[{"type":"text","text":"x"}]},{"type":"horizontal_rule"}]}`,
			"- a\n\n```\nx\n```\n\n---"},
		{"heading level is clamped",
			`{"type":"doc","content":[{"type":"heading","attrs":{"level":9},"content":[{"type":"text","text":"Deep"}]},{"type":"heading","content":[{"type":"text","text":"Top"}]}]}`,
			"###### Deep\n\n# Top"},
		{"empty heading is dropped",
			`{"type":"doc","content":[{"type":"heading","attrs":{"level":2}},{"type":"paragraph","content":[{"type":"text","text":"a"}]}]}`,
			"a"},
		{"code block containing a fence",
			`{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"` + "```" + `\ninner\n` + "```" + `"}]}]}`,
			"````\n```\ninner\n```\n````"},
		{"list item holding text without a paragraph",
			`{"type":"doc","content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"text","text":"bare"}]}]}]}`,
			"- bare"},
		{"code block inside a list item",
			`{"type":"doc","content":[{"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"run"}]},{"type":"codeBlock","content":[{"type":"text","text":"make"}]}]}]}]}`,
			"1. run\n\n   ```\n   make\n   ```"},
		{"unknown leaf node falls back to its text",
			`{"type":"doc","content":[{"type":"mystery","text":"kept"}]}`,
			"kept"},
		{"unknown node without text is dropped",
			`{"type":"doc","content":[{"type":"mystery","attrs":{"id":"1"}}]}`,
			""},
		{"empty document",
			`{"type":"doc"}`,
			""},
		{"skeleton paragraph without content",
			`{"type":"doc","content":[{"type":"paragraph","attrs":{"id":"x"}}]}`,
			""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ToMarkdown([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestToMarkdownRejectsNonDocuments(t *testing.T) {
	for _, raw := range []string{`"a string"`, `[1,2]`, `{"type":"doc","content":"text"}`, `{`} {
		if _, err := ToMarkdown([]byte(raw)); err == nil {
			t.Errorf("ToMarkdown(%s) returned no error", raw)
		}
	}
}
