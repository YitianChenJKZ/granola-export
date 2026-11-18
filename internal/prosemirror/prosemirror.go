// Package prosemirror converts ProseMirror/TipTap document JSON, the format
// Granola stores rich text in, to Markdown.
package prosemirror

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Node struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Marks   []Mark         `json:"marks"`
	Content []Node         `json:"content"`
}

type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// ToMarkdown converts a ProseMirror document given as JSON.
func ToMarkdown(raw json.RawMessage) (string, error) {
	var n Node
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", fmt.Errorf("not a ProseMirror document: %w", err)
	}
	return Render(&n), nil
}

// Render converts a parsed node and everything below it.
func Render(n *Node) string {
	return strings.Join(blocks([]Node{*n}), "\n\n")
}

// kind normalises node and mark names so that camelCase (TipTap) and
// snake_case (plain ProseMirror) schemas are handled alike.
func kind(t string) string {
	return strings.ToLower(strings.ReplaceAll(t, "_", ""))
}

func isInline(n *Node) bool {
	switch kind(n.Type) {
	case "text", "hardbreak", "mention", "emoji":
		return true
	}
	return false
}

func blocks(nodes []Node) []string {
	var out []string
	for i := 0; i < len(nodes); {
		if isInline(&nodes[i]) {
			j := i
			for j < len(nodes) && isInline(&nodes[j]) {
				j++
			}
			if s := paragraph(nodes[i:j]); s != "" {
				out = append(out, s)
			}
			i = j
			continue
		}
		if s := block(&nodes[i]); s != "" {
			out = append(out, s)
		}
		i++
	}
	return out
}

func block(n *Node) string {
	switch kind(n.Type) {
	case "paragraph":
		return paragraph(n.Content)
	case "heading":
		text := strings.Join(strings.Fields(inline(n.Content)), " ")
		if text == "" {
			return ""
		}
		level := min(max(attrInt(n.Attrs, "level", 1), 1), 6)
		return strings.Repeat("#", level) + " " + text
	case "bulletlist", "tasklist":
		return list(n, func(int) string { return "- " })
	case "orderedlist":
		start := attrInt(n.Attrs, "start", 1)
		if _, ok := n.Attrs["start"]; !ok {
			start = attrInt(n.Attrs, "order", 1)
		}
		return list(n, func(i int) string { return strconv.Itoa(start+i) + ". " })
	case "listitem", "taskitem":
		return listItem(n, "- ")
	case "blockquote":
		lines := strings.Split(strings.Join(blocks(n.Content), "\n\n"), "\n")
		for i, line := range lines {
			if line == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + line
			}
		}
		if len(lines) == 1 && lines[0] == ">" {
			return ""
		}
		return strings.Join(lines, "\n")
	case "codeblock":
		return codeBlock(n)
	case "horizontalrule":
		return "---"
	case "table":
		return table(n)
	case "image":
		return image(n)
	default:
		if len(n.Content) == 0 {
			return paragraph([]Node{*n})
		}
		return strings.Join(blocks(n.Content), "\n\n")
	}
}

func paragraph(nodes []Node) string {
	return escapeLineStarts(strings.TrimSpace(inline(nodes)))
}

var listStart = regexp.MustCompile(`^(- |\d+\. )`)

func list(n *Node, marker func(i int) string) string {
	var items []string
	for i := range n.Content {
		if item := listItem(&n.Content[i], marker(len(items))); item != "" {
			items = append(items, item)
		}
	}
	return strings.Join(items, "\n")
}

func listItem(item *Node, marker string) string {
	indent := strings.Repeat(" ", len(marker))
	var parts []string
	switch kind(item.Type) {
	case "taskitem":
		if attrBool(item.Attrs, "checked") {
			marker += "[x] "
		} else {
			marker += "[ ] "
		}
		parts = blocks(item.Content)
	case "listitem":
		parts = blocks(item.Content)
	default:
		parts = blocks([]Node{*item})
	}
	if len(parts) == 0 {
		return ""
	}
	var body strings.Builder
	for i, part := range parts {
		if i > 0 {
			// A nested list follows its parent's text directly; anything
			// else starts a new paragraph inside the item.
			if listStart.MatchString(part) {
				body.WriteString("\n")
			} else {
				body.WriteString("\n\n")
			}
		}
		body.WriteString(part)
	}
	lines := strings.Split(body.String(), "\n")
	for i, line := range lines {
		switch {
		case i == 0:
			lines[i] = marker + line
		case line != "":
			lines[i] = indent + line
		}
	}
	return strings.Join(lines, "\n")
}

var languageName = regexp.MustCompile(`^[A-Za-z0-9_+#.-]+`)

func codeBlock(n *Node) string {
	var text strings.Builder
	for i := range n.Content {
		if kind(n.Content[i].Type) == "hardbreak" {
			text.WriteString("\n")
		} else {
			text.WriteString(plainText(&n.Content[i]))
		}
	}
	code := strings.TrimRight(text.String(), "\n")
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`"
	}
	lang := languageName.FindString(attrString(n.Attrs, "language"))
	return fence + lang + "\n" + code + "\n" + fence
}

func image(n *Node) string {
	src := attrString(n.Attrs, "src")
	// Embedded image data would put megabytes of base64 into the note.
	if src == "" || strings.HasPrefix(src, "data:") {
		return ""
	}
	alt := strings.Join(strings.Fields(attrString(n.Attrs, "alt")), " ")
	return "![" + escapeText(alt) + "](" + linkDestination(src) + ")"
}

func table(n *Node) string {
	var rows [][]string
	width := 0
	for i := range n.Content {
		row := &n.Content[i]
		var cells []string
		for j := range row.Content {
			cell := strings.Join(blocks(row.Content[j].Content), "<br>")
			cell = strings.ReplaceAll(cell, "\n", "<br>")
			cells = append(cells, strings.ReplaceAll(cell, "|", `\|`))
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
			width = max(width, len(cells))
		}
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	writeRow(rows[0])
	separator := make([]string, width)
	for i := range separator {
		separator[i] = "---"
	}
	writeRow(separator)
	for _, row := range rows[1:] {
		writeRow(row)
	}
	return strings.TrimRight(b.String(), "\n")
}

type inlineMark struct {
	kind string
	href string
}

func (m inlineMark) open() string {
	switch m.kind {
	case "link":
		return "["
	case "bold":
		return "**"
	case "italic":
		return "*"
	case "strike":
		return "~~"
	case "highlight":
		return "=="
	}
	return ""
}

func (m inlineMark) close() string {
	if m.kind == "link" {
		return "](" + linkDestination(m.href) + ")"
	}
	return m.open()
}

// inlineMarks returns the marks of a text node in a fixed nesting order, and
// whether the node is inline code.
func inlineMarks(marks []Mark) (out []inlineMark, code bool) {
	var link, bold, italic, strike, highlight bool
	var href string
	for _, m := range marks {
		switch kind(m.Type) {
		case "link":
			if h := attrString(m.Attrs, "href"); h != "" {
				link, href = true, h
			}
		case "bold", "strong":
			bold = true
		case "italic", "em":
			italic = true
		case "strike", "strikethrough":
			strike = true
		case "highlight":
			highlight = true
		case "code":
			code = true
		}
	}
	if link {
		out = append(out, inlineMark{"link", href})
	}
	if bold {
		out = append(out, inlineMark{kind: "bold"})
	}
	if italic {
		out = append(out, inlineMark{kind: "italic"})
	}
	if strike {
		out = append(out, inlineMark{kind: "strike"})
	}
	if highlight {
		out = append(out, inlineMark{kind: "highlight"})
	}
	return out, code
}

// inline renders a run of inline nodes. Whitespace at the edges of marked
// text is moved outside the delimiters, because "** bold **" is not emphasis
// in Markdown.
func inline(nodes []Node) string {
	var b strings.Builder
	var open []inlineMark
	pending := ""

	emit := func(text string, want []inlineMark) {
		core := strings.TrimLeft(text, " \t\n")
		lead := text[:len(text)-len(core)]
		core = strings.TrimRight(core, " \t\n")
		if core == "" {
			pending += text
			return
		}
		trail := text[len(lead)+len(core):]

		shared := 0
		for shared < len(open) && shared < len(want) && open[shared] == want[shared] {
			shared++
		}
		for i := len(open) - 1; i >= shared; i-- {
			b.WriteString(open[i].close())
		}
		open = open[:shared]
		b.WriteString(pending)
		b.WriteString(lead)
		for _, m := range want[shared:] {
			b.WriteString(m.open())
			open = append(open, m)
		}
		b.WriteString(core)
		pending = trail
	}

	for i := range nodes {
		n := &nodes[i]
		switch kind(n.Type) {
		case "hardbreak":
			pending = strings.TrimRight(pending, " \t") + "  \n"
		case "text":
			marks, code := inlineMarks(n.Marks)
			if code {
				emit(codeSpan(n.Text), marks)
			} else {
				emit(escapeText(n.Text), marks)
			}
		case "image":
			emit(image(n), nil)
		default:
			emit(escapeText(plainText(n)), nil)
		}
	}
	for i := len(open) - 1; i >= 0; i-- {
		b.WriteString(open[i].close())
	}
	b.WriteString(pending)
	return b.String()
}

func plainText(n *Node) string {
	if n.Text != "" {
		return n.Text
	}
	for _, key := range []string{"label", "text"} {
		if s := attrString(n.Attrs, key); s != "" {
			return s
		}
	}
	var b strings.Builder
	for i := range n.Content {
		b.WriteString(plainText(&n.Content[i]))
	}
	return b.String()
}

func codeSpan(text string) string {
	text = strings.ReplaceAll(text, "\n", " ")
	ticks := "`"
	for strings.Contains(text, ticks) {
		ticks += "`"
	}
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	return ticks + text + ticks
}

func linkDestination(href string) string {
	return strings.NewReplacer(
		" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "\n", "", "\t", "%09",
	).Replace(strings.TrimSpace(href))
}

func isWordRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127
}

// escapeText backslash-escapes the characters that would otherwise be read as
// Markdown (or Obsidian wikilink) syntax in running text.
func escapeText(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		switch r {
		case '\\', '`', '*', '[', ']', '<':
			b.WriteRune('\\')
		case '_':
			intraword := i > 0 && i < len(runes)-1 && isWordRune(runes[i-1]) && isWordRune(runes[i+1])
			if !intraword {
				b.WriteRune('\\')
			}
		case '~', '=':
			// Only doubled, where they mean strikethrough and highlight.
			if i > 0 && runes[i-1] == r || i < len(runes)-1 && runes[i+1] == r {
				b.WriteRune('\\')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

var (
	lineStartMarker  = regexp.MustCompile(`^(#{1,6}|[-+>])([ \t]|$)`)
	lineStartNumber  = regexp.MustCompile(`^(\d+)([.)])([ \t]|$)`)
	lineIsRuleOrLine = regexp.MustCompile(`^(-{2,}|=+)[ \t]*$`)
)

// escapeLineStarts stops a paragraph line from being read as a heading, list
// item, quote, rule or indented code block.
func escapeLineStarts(paragraph string) string {
	if paragraph == "" {
		return ""
	}
	lines := strings.Split(paragraph, "\n")
	for i, line := range lines {
		line = strings.TrimLeft(line, " \t")
		switch {
		case lineStartMarker.MatchString(line), lineIsRuleOrLine.MatchString(line), strings.HasPrefix(line, ">"):
			line = `\` + line
		case lineStartNumber.MatchString(line):
			line = lineStartNumber.ReplaceAllString(line, `$1\$2$3`)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func attrString(attrs map[string]any, key string) string {
	s, _ := attrs[key].(string)
	return s
}

func attrBool(attrs map[string]any, key string) bool {
	switch v := attrs[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func attrInt(attrs map[string]any, key string, fallback int) int {
	switch v := attrs[key].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
