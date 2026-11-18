package granola

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/prosemirror"
)

// parseDocument reads one Granola document, as returned by the document list
// endpoint and as stored in the cache. fallbackID is used when the document
// carries no id of its own (the cache keys documents by id).
func parseDocument(raw json.RawMessage, fallbackID string) (*Note, error) {
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return nil, &ShapeError{"a document", "expected an object, found " + jsonType(raw)}
	}
	id := f.str("id")
	if id == "" {
		id = fallbackID
	}
	if id == "" {
		return nil, &ShapeError{"a document", "no id; keys: " + f.keys()}
	}
	n := &Note{
		ID:         id,
		Title:      strings.Join(strings.Fields(f.str("title")), " "),
		CreatedAt:  parseTime(f["created_at"]),
		UpdatedAt:  parseTime(f["updated_at"]),
		UpdatedRaw: strings.Trim(strings.TrimSpace(string(f["updated_at"])), `"`),
		Deleted:    f.isSet("deleted_at"),
		OwnerID:    f.str("user_id"),
		Attendees:  parseAttendees(f),
	}
	n.NotesMarkdown = strings.TrimSpace(f.str("notes_markdown"))
	if n.NotesMarkdown == "" && f.isSet("notes") {
		if md, err := contentMarkdown(f["notes"]); err == nil {
			n.NotesMarkdown = md
		}
	}
	if n.NotesMarkdown == "" {
		n.NotesMarkdown = strings.TrimSpace(f.str("notes_plain"))
	}
	return n, nil
}

type person struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Details     struct {
		Person struct {
			Name struct {
				FullName string `json:"fullName"`
			} `json:"name"`
		} `json:"person"`
	} `json:"details"`
}

func (p person) label() string {
	for _, s := range []string{p.Name, p.DisplayName, p.Details.Person.Name.FullName, p.Email} {
		if s = strings.Join(strings.Fields(s), " "); s != "" {
			return s
		}
	}
	return ""
}

func parseAttendees(f fields) []string {
	var people []person
	var grouped struct {
		Creator   *person  `json:"creator"`
		Attendees []person `json:"attendees"`
	}
	if json.Unmarshal(f["people"], &grouped) == nil {
		if grouped.Creator != nil {
			people = append(people, *grouped.Creator)
		}
		people = append(people, grouped.Attendees...)
	}
	if len(people) == 0 {
		var event struct {
			Attendees []person `json:"attendees"`
		}
		if json.Unmarshal(f["google_calendar_event"], &event) == nil {
			people = event.Attendees
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range people {
		label := p.label()
		key := strings.ToLower(p.Email)
		if key == "" {
			key = strings.ToLower(label)
		}
		if label == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, label)
	}
	return out
}

// contentMarkdown converts rich text that is normally a ProseMirror document
// but may also arrive as a JSON-encoded document or as ready-made text.
func contentMarkdown(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return "", nil
	case raw[0] == '{':
		return prosemirror.ToMarkdown(raw)
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "{") {
			if md, err := prosemirror.ToMarkdown(json.RawMessage(s)); err == nil {
				return md, nil
			}
		}
		return s, nil
	default:
		return "", &ShapeError{"rich text content", "expected an object or string, found " + jsonType(raw)}
	}
}

// parsePanels reads AI-generated panels given as an array, as {"panels":
// [...]}, as a single panel, or as an object keyed by panel id (cache v3).
func parsePanels(raw json.RawMessage) ([]Panel, error) {
	items, err := listOf(raw, "panels", "content")
	if err != nil {
		return nil, &ShapeError{"document panels", err.Error()}
	}
	type dated struct {
		Panel
		created time.Time
	}
	var panels []dated
	for _, item := range items {
		var f fields
		if err := json.Unmarshal(item, &f); err != nil || f == nil {
			return nil, &ShapeError{"a document panel", "expected an object, found " + jsonType(item)}
		}
		if f.isSet("deleted_at") {
			continue
		}
		md, err := contentMarkdown(f["content"])
		if err != nil {
			return nil, fmt.Errorf("a document panel: %w", err)
		}
		if strings.TrimSpace(md) == "" {
			continue
		}
		title := strings.Join(strings.Fields(f.str("title")), " ")
		if title == "" {
			title = "Summary"
		}
		panels = append(panels, dated{Panel{Title: title, Markdown: md}, parseTime(f["created_at"])})
	}
	sort.SliceStable(panels, func(i, j int) bool { return panels[i].created.Before(panels[j].created) })
	out := make([]Panel, len(panels))
	for i, p := range panels {
		out[i] = p.Panel
	}
	return out, nil
}

// parseTranscript reads transcript segments given as an array or as
// {"transcript": [...]}.
func parseTranscript(raw json.RawMessage) ([]Segment, error) {
	items, err := listOf(raw, "transcript", "text")
	if err != nil {
		return nil, &ShapeError{"a transcript", err.Error()}
	}
	var out []Segment
	for _, item := range items {
		var f fields
		if err := json.Unmarshal(item, &f); err != nil || f == nil {
			return nil, &ShapeError{"a transcript segment", "expected an object, found " + jsonType(item)}
		}
		text := strings.Join(strings.Fields(f.str("text")), " ")
		if text == "" {
			continue
		}
		out = append(out, Segment{Start: parseTime(f["start_timestamp"]), Source: f.str("source"), Text: text})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// listOf returns the elements of a JSON list that may be a bare array, an
// object wrapping the array under wrapKey, a single element (recognised by
// having itemKey), or an object whose values are the elements.
func listOf(raw json.RawMessage, wrapKey, itemKey string) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	switch raw[0] {
	case '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("malformed array")
		}
		return items, nil
	case '{':
		var obj fields
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("malformed object")
		}
		if inner, ok := obj[wrapKey]; ok {
			if err := json.Unmarshal(inner, &items); err != nil {
				return nil, fmt.Errorf("%q is %s, expected an array", wrapKey, jsonType(inner))
			}
			return items, nil
		}
		if _, ok := obj[itemKey]; ok {
			return []json.RawMessage{raw}, nil
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, obj[k])
		}
		return items, nil
	default:
		return nil, fmt.Errorf("expected an array or object, found %s", jsonType(raw))
	}
}
