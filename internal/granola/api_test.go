package granola

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "secret-test-token"

// fakeAPI stands in for the Granola API. Handlers are keyed by path.
type fakeAPI struct {
	t        *testing.T
	server   *httptest.Server
	handlers map[string]func(w http.ResponseWriter, body map[string]any)
	calls    map[string]int
	sleeps   []time.Duration
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, handlers: map[string]func(http.ResponseWriter, map[string]any){}, calls: map[string]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls[r.URL.Path]++
		if r.Method != http.MethodPost {
			t.Errorf("%s: method %s, want POST", r.URL.Path, r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("%s: Authorization = %q", r.URL.Path, got)
		}
		if got := r.Header.Get("X-Client-Version"); got != "7.1.0" {
			t.Errorf("%s: X-Client-Version = %q", r.URL.Path, got)
		}
		if got := r.Header.Get("User-Agent"); got != "granola-export/test" {
			t.Errorf("%s: User-Agent = %q", r.URL.Path, got)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("%s: request body is not JSON: %s", r.URL.Path, raw)
		}
		handler, ok := f.handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		handler(w, body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) client() *Client {
	c := NewClient(f.server.URL+"/", testToken, "granola-export/test", "7.1.0")
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		f.sleeps = append(f.sleeps, d)
		return ctx.Err()
	}
	return c
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func docs(from, to int) []any {
	var out []any
	for i := from; i < to; i++ {
		out = append(out, map[string]any{"id": fmt.Sprintf("doc-%03d", i), "title": fmt.Sprintf("Note %d", i), "updated_at": "2026-11-17T16:45:00Z"})
	}
	return out
}

func TestListPaginates(t *testing.T) {
	api := newFakeAPI(t)
	var offsets []float64
	api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
		offset := body["offset"].(float64)
		offsets = append(offsets, offset)
		if body["limit"].(float64) != 100 {
			t.Errorf("limit = %v", body["limit"])
		}
		end := min(int(offset)+100, 230)
		respond(w, 200, map[string]any{"docs": docs(int(offset), end), "deleted": []any{}})
	}
	notes, err := NewAPISource(api.client()).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 230 || notes[0].ID != "doc-000" || notes[229].ID != "doc-229" {
		t.Errorf("got %d notes", len(notes))
	}
	if fmt.Sprint(offsets) != "[0 100 200 230]" {
		t.Errorf("offsets requested = %v", offsets)
	}
}

func TestListCopesWithOddPaging(t *testing.T) {
	t.Run("server caps the page size", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			offset := int(body["offset"].(float64))
			respond(w, 200, map[string]any{"docs": docs(offset, min(offset+30, 70))})
		}
		notes, err := NewAPISource(api.client()).List(context.Background())
		if err != nil || len(notes) != 70 {
			t.Errorf("got %d notes, %v; want all 70", len(notes), err)
		}
	})
	t.Run("server ignores the offset", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			respond(w, 200, map[string]any{"docs": docs(0, 100)})
		}
		notes, err := NewAPISource(api.client()).List(context.Background())
		if err != nil || len(notes) != 100 {
			t.Errorf("got %d notes, %v; want 100", len(notes), err)
		}
		if api.calls[pathDocuments] != 2 {
			t.Errorf("made %d requests, want 2", api.calls[pathDocuments])
		}
	})
	t.Run("bare array", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			if body["offset"].(float64) > 0 {
				respond(w, 200, []any{})
				return
			}
			respond(w, 200, docs(0, 3))
		}
		notes, err := NewAPISource(api.client()).List(context.Background())
		if err != nil || len(notes) != 3 {
			t.Errorf("got %d notes, %v; want 3", len(notes), err)
		}
	})
	t.Run("no notes at all", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			respond(w, 200, map[string]any{"docs": []any{}})
		}
		notes, err := NewAPISource(api.client()).List(context.Background())
		if err != nil || len(notes) != 0 {
			t.Errorf("got %d notes, %v", len(notes), err)
		}
	})
}

func TestListShapeErrors(t *testing.T) {
	cases := map[string]any{
		"no docs key":       map[string]any{"documents": []any{}, "cursor": nil},
		"docs not an array": map[string]any{"docs": "nope"},
		"a string":          "nope",
		"nothing readable":  map[string]any{"docs": []any{"x", "y"}},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) { respond(w, 200, response) }
			_, err := NewAPISource(api.client()).List(context.Background())
			var shape *ShapeError
			if !errors.As(err, &shape) {
				t.Errorf("error = %v, want a ShapeError", err)
			}
		})
	}
}

func TestRetries(t *testing.T) {
	t.Run("rate limit honours Retry-After", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			if api.calls[pathDocuments] == 1 {
				w.Header().Set("Retry-After", "7")
				respond(w, 429, map[string]any{"message": "slow down"})
				return
			}
			respond(w, 200, map[string]any{"docs": []any{}})
		}
		if _, err := NewAPISource(api.client()).List(context.Background()); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(api.sleeps) != "[7s]" {
			t.Errorf("waited %v, want [7s]", api.sleeps)
		}
	})
	t.Run("server errors back off then give up", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			respond(w, 503, map[string]any{"message": "unavailable"})
		}
		_, err := NewAPISource(api.client()).List(context.Background())
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != 503 || he.Message != "unavailable" {
			t.Errorf("error = %v, want HTTP 503", err)
		}
		if fmt.Sprint(api.sleeps) != "[1s 2s 4s 8s]" || api.calls[pathDocuments] != 5 {
			t.Errorf("waited %v over %d requests", api.sleeps, api.calls[pathDocuments])
		}
	})
	t.Run("rejected login is not retried", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			respond(w, 401, map[string]any{"message": "Unauthorized"})
		}
		_, err := NewAPISource(api.client()).List(context.Background())
		if !errors.Is(err, ErrAuth) || api.calls[pathDocuments] != 1 {
			t.Errorf("error = %v after %d requests, want ErrAuth after 1", err, api.calls[pathDocuments])
		}
	})
	t.Run("client errors are not retried", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			respond(w, 400, map[string]any{"error": "bad request"})
		}
		_, err := NewAPISource(api.client()).List(context.Background())
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != 400 || he.Message != "bad request" || api.calls[pathDocuments] != 1 {
			t.Errorf("error = %v after %d requests", err, api.calls[pathDocuments])
		}
	})
	t.Run("network failure is retried and never leaks the token", func(t *testing.T) {
		api := newFakeAPI(t)
		c := api.client()
		api.server.Close()
		_, err := NewAPISource(c).List(context.Background())
		if err == nil || len(api.sleeps) != 4 {
			t.Fatalf("error = %v after %d waits, want a failure after 4 retries", err, len(api.sleeps))
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("error contains the token: %v", err)
		}
	})
	t.Run("cancellation stops retrying", func(t *testing.T) {
		api := newFakeAPI(t)
		ctx, cancel := context.WithCancel(context.Background())
		var requests atomic.Int32
		api.handlers[pathDocuments] = func(w http.ResponseWriter, body map[string]any) {
			requests.Add(1)
			cancel()
			respond(w, 503, nil)
		}
		_, err := NewAPISource(api.client()).List(ctx)
		if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
			t.Errorf("error = %v after %d requests, want cancellation after 1", err, requests.Load())
		}
	})
}

func TestHydrate(t *testing.T) {
	api := newFakeAPI(t)
	api.handlers[pathPanels] = func(w http.ResponseWriter, body map[string]any) {
		if body["document_id"] != "doc-1" {
			t.Errorf("panels requested for %v", body["document_id"])
		}
		respond(w, 200, []any{map[string]any{"title": "Summary", "content": pmDoc("the summary")}})
	}
	api.handlers[pathTranscript] = func(w http.ResponseWriter, body map[string]any) {
		respond(w, 200, []any{map[string]any{"text": "hello", "source": "microphone", "start_timestamp": "2026-11-17T15:30:01Z"}})
	}
	src := NewAPISource(api.client())

	n := &Note{ID: "doc-1"}
	if err := src.Hydrate(context.Background(), n, true); err != nil {
		t.Fatal(err)
	}
	if !n.Hydrated || len(n.Panels) != 1 || n.Panels[0].Markdown != "the summary" || len(n.Transcript) != 1 {
		t.Errorf("unexpected note: %+v", n)
	}

	n = &Note{ID: "doc-1"}
	if err := src.Hydrate(context.Background(), n, false); err != nil {
		t.Fatal(err)
	}
	if len(n.Transcript) != 0 || api.calls[pathTranscript] != 1 {
		t.Errorf("transcript was fetched although not wanted (%d requests)", api.calls[pathTranscript])
	}
	if len(src.Notices()) != 0 {
		t.Errorf("unexpected notices: %q", src.Notices())
	}
}

func TestHydrateNotFoundIsRecordedNotFatal(t *testing.T) {
	api := newFakeAPI(t)
	src := NewAPISource(api.client())
	n := &Note{ID: "doc-1"}
	if err := src.Hydrate(context.Background(), n, true); err != nil {
		t.Fatal(err)
	}
	if !n.Hydrated || !n.PanelsUnavailable || !n.TranscriptUnavailable {
		t.Errorf("unexpected note: %+v", n)
	}
	if len(src.Notices()) != 2 {
		t.Errorf("notices = %q, want one each for panels and transcript", src.Notices())
	}
}

func TestHydrateFailures(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathPanels] = func(w http.ResponseWriter, body map[string]any) { respond(w, 500, nil) }
		n := &Note{ID: "doc-1"}
		if err := NewAPISource(api.client()).Hydrate(context.Background(), n, true); err == nil || n.Hydrated {
			t.Errorf("error = %v, hydrated = %v", err, n.Hydrated)
		}
	})
	t.Run("unexpected panel shape", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathPanels] = func(w http.ResponseWriter, body map[string]any) { respond(w, 200, "surprise") }
		n := &Note{ID: "doc-1"}
		err := NewAPISource(api.client()).Hydrate(context.Background(), n, true)
		var shape *ShapeError
		if !errors.As(err, &shape) || n.Hydrated {
			t.Errorf("error = %v, hydrated = %v", err, n.Hydrated)
		}
	})
	t.Run("rejected login", func(t *testing.T) {
		api := newFakeAPI(t)
		api.handlers[pathPanels] = func(w http.ResponseWriter, body map[string]any) { respond(w, 401, nil) }
		err := NewAPISource(api.client()).Hydrate(context.Background(), &Note{ID: "doc-1"}, true)
		if !errors.Is(err, ErrAuth) {
			t.Errorf("error = %v, want ErrAuth", err)
		}
	})
}
