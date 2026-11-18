// Package obsidian renders Granola notes as Markdown files that Obsidian
// reads as notes with properties.
package obsidian

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
)

// IDProperty is the frontmatter property that ties a file to its Granola note.
const IDProperty = "granola_id"

const (
	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02T15:04:05"
	maxTitleRunes  = 100
	// File names are limited to 255 bytes; this leaves room for the date,
	// a disambiguating suffix and the extension.
	maxTitleBytes = 180
)

type Options struct {
	IncludeTranscript bool
	// Location is the time zone dates and times are written in.
	Location *time.Location
}

func (o Options) location() *time.Location {
	if o.Location == nil {
		return time.Local
	}
	return o.Location
}

// Render returns the full contents of the note's Markdown file.
func Render(n *granola.Note, o Options) string {
	loc := o.location()
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlString(displayTitle(n)))
	if !n.CreatedAt.IsZero() {
		created := n.CreatedAt.In(loc)
		fmt.Fprintf(&b, "date: %s\n", created.Format(dateLayout))
		fmt.Fprintf(&b, "created: %s\n", created.Format(dateTimeLayout))
	}
	if !n.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "updated: %s\n", n.UpdatedAt.In(loc).Format(dateTimeLayout))
	}
	fmt.Fprintf(&b, "%s: %s\n", IDProperty, yamlID(n.ID))
	fmt.Fprintf(&b, "granola_url: https://notes.granola.ai/d/%s\n", url.PathEscape(n.ID))
	if len(n.Attendees) > 0 {
		b.WriteString("attendees:\n")
		for _, a := range n.Attendees {
			fmt.Fprintf(&b, "  - %s\n", yamlString(a))
		}
	}
	b.WriteString("tags:\n  - granola\n")
	b.WriteString("---\n")

	for _, p := range n.Panels {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", p.Title, strings.TrimSpace(p.Markdown))
	}
	if notes := strings.TrimSpace(n.NotesMarkdown); notes != "" {
		fmt.Fprintf(&b, "\n## My notes\n\n%s\n", notes)
	}
	if o.IncludeTranscript && len(n.Transcript) > 0 {
		b.WriteString("\n")
		b.WriteString(transcriptCallout(n.Transcript, loc))
	}
	return b.String()
}

func displayTitle(n *granola.Note) string {
	if n.Title == "" {
		return "Untitled"
	}
	return n.Title
}

// transcriptCallout renders the transcript as a callout that is collapsed by
// default, one paragraph per speaker turn.
func transcriptCallout(segments []granola.Segment, loc *time.Location) string {
	var b strings.Builder
	b.WriteString("> [!quote]- Transcript\n")
	for i := 0; i < len(segments); {
		j := i
		var words []string
		for j < len(segments) && segments[j].Source == segments[i].Source {
			words = append(words, segments[j].Text)
			j++
		}
		if i > 0 {
			b.WriteString(">\n")
		}
		fmt.Fprintf(&b, "> **%s**", speaker(segments[i].Source))
		if !segments[i].Start.IsZero() {
			b.WriteString(" " + segments[i].Start.In(loc).Format("15:04"))
		}
		b.WriteString(": " + escapeSpeech(strings.Join(words, " ")) + "\n")
		i = j
	}
	return b.String()
}

func speaker(source string) string {
	switch strings.ToLower(source) {
	case "microphone":
		return "Me"
	case "system", "speaker":
		return "Them"
	case "":
		return "Speaker"
	}
	return escapeSpeech(source)
}

var speechSpecials = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", `\<`)

func escapeSpeech(s string) string {
	return speechSpecials.Replace(strings.Join(strings.Fields(s), " "))
}

// yamlString writes s as a double-quoted YAML scalar, which is safe for any
// content.
func yamlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 || r == utf8.RuneError:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// unquoteYAML undoes the quoting of a simple YAML scalar: the double-quoted
// form yamlString writes, or the single-quoted form other tools may use.
func unquoteYAML(value string) string {
	if len(value) < 2 || value[0] != value[len(value)-1] {
		return value
	}
	switch value[0] {
	case '"':
		if s, err := strconv.Unquote(value); err == nil {
			return s
		}
		return value[1 : len(value)-1]
	case '\'':
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

var plainID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func yamlID(id string) string {
	if plainID.MatchString(id) {
		return id
	}
	return yamlString(id)
}

// SanitizeTitle makes a note title safe to use in a file name on macOS and
// in Obsidian, where * " \ / < > : | ? are not allowed and # ^ [ ] break
// links to the note.
func SanitizeTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r == '/' || r == '\\' || r == '|':
			b.WriteRune('-')
		case strings.ContainsRune(`*"<>:?#^[]`, r):
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r) || r == utf8.RuneError:
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	runes := []rune(clean)
	if len(runes) > maxTitleRunes {
		runes = runes[:maxTitleRunes]
	}
	for len(string(runes)) > maxTitleBytes {
		runes = runes[:len(runes)-1]
	}
	clean = string(runes)
	clean = strings.Trim(clean, " .-")
	if clean == "" {
		return "Untitled"
	}
	return clean
}

// BaseName returns the file name for a note, without the .md extension:
// the local date of the note followed by its title.
func BaseName(n *granola.Note, loc *time.Location) string {
	title := SanitizeTitle(n.Title)
	when := n.CreatedAt
	if when.IsZero() {
		when = n.UpdatedAt
	}
	if when.IsZero() {
		return title
	}
	if loc == nil {
		loc = time.Local
	}
	return when.In(loc).Format(dateLayout) + " " + title
}

// ShortID returns a few characters of the note id that are safe in a file
// name, for telling apart notes that would otherwise share a name.
func ShortID(id string, length int) string {
	var b strings.Builder
	for _, r := range id {
		if b.Len() >= length {
			break
		}
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParseID returns the Granola note id recorded in the frontmatter at the
// start of a Markdown file, if there is one.
func ParseID(head []byte) (string, bool) {
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	scanner := bufio.NewScanner(bytes.NewReader(head))
	scanner.Buffer(make([]byte, 0, 64*1024), len(head)+1)
	if !scanner.Scan() || strings.TrimRight(scanner.Text(), " \t\r") != "---" {
		return "", false
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		if line == "---" || line == "..." {
			return "", false
		}
		value, ok := strings.CutPrefix(line, IDProperty+":")
		if !ok {
			continue
		}
		value = unquoteYAML(strings.TrimSpace(value))
		return value, value != ""
	}
	return "", false
}
