package granola

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the API the Granola desktop app itself talks to. It is
// undocumented and may change without notice.
const DefaultBaseURL = "https://api.granola.ai"

const (
	pathDocuments  = "/v2/get-documents"
	pathPanels     = "/v1/get-document-panels"
	pathTranscript = "/v1/get-document-transcript"

	maxResponseBytes = 256 << 20
	maxRetryAfter    = time.Minute
)

// ErrAuth means Granola rejected the login token.
var ErrAuth = errors.New("Granola rejected the login token")

// HTTPError is a response the client could not use.
type HTTPError struct {
	Path    string
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s returned HTTP %d: %s", e.Path, e.Status, e.Message)
	}
	return fmt.Sprintf("%s returned HTTP %d", e.Path, e.Status)
}

func isNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

func isEncrypted(err error) bool { return errors.Is(err, ErrEncrypted) }

// Client makes read-only requests with the app's login token.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	// ClientVersion is sent as X-Client-Version when set.
	ClientVersion string
	HTTP          *http.Client
	// MaxRetries is how often a rate-limited or failed request is retried.
	MaxRetries int
	// Sleep waits between retries; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func NewClient(baseURL, token, userAgent, clientVersion string) *Client {
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		Token:         token,
		UserAgent:     userAgent,
		ClientVersion: clientVersion,
		HTTP:          &http.Client{Timeout: 90 * time.Second},
		MaxRetries:    4,
		Sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// post sends body as JSON and returns the response body. Rate limiting,
// server errors and network failures are retried with exponential backoff.
func (c *Client) post(ctx context.Context, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		data, status, retryAfter, err := c.send(ctx, path, payload)
		var failure error
		switch {
		case err != nil:
			failure = fmt.Errorf("%s: %w", path, err)
		case status >= 200 && status < 300:
			return data, nil
		case status == http.StatusUnauthorized:
			return nil, fmt.Errorf("%s: %w (HTTP 401)", path, ErrAuth)
		default:
			failure = &HTTPError{Path: path, Status: status, Message: errorMessage(data)}
			if status != http.StatusTooManyRequests && status < 500 {
				return nil, failure
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt >= c.MaxRetries {
			return nil, failure
		}
		wait := time.Second << attempt
		if retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfter)
		}
		if err := c.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (c *Client) send(ctx context.Context, path string, payload []byte) (data []byte, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.ClientVersion != "" {
		req.Header.Set("X-Client-Version", c.ClientVersion)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, 0, err
	}
	if len(data) > maxResponseBytes {
		return nil, 0, 0, fmt.Errorf("response larger than %d MB", maxResponseBytes>>20)
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && seconds > 0 {
		retryAfter = time.Duration(seconds) * time.Second
	}
	return data, resp.StatusCode, retryAfter, nil
}

// errorMessage extracts the short message from an error response body.
func errorMessage(data []byte) string {
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	msg := body.Message
	if msg == "" {
		msg = body.Error
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return msg
}

// APISource reads notes with the Granola app's own login.
type APISource struct {
	Client   *Client
	PageSize int

	unreadable   int
	noPanels     int
	noTranscript int
}

func NewAPISource(c *Client) *APISource {
	return &APISource{Client: c, PageSize: 100}
}

func (s *APISource) Name() string { return SourceApp }

func (s *APISource) List(ctx context.Context) ([]*Note, error) {
	const maxPages = 1000
	var notes []*Note
	seen := map[string]bool{}
	s.unreadable = 0
	for page, offset := 0, 0; page < maxPages; page++ {
		data, err := s.Client.post(ctx, pathDocuments, map[string]any{"limit": s.PageSize, "offset": offset})
		if err != nil {
			return nil, err
		}
		docs, err := documentList(data)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, raw := range docs {
			n, err := parseDocument(raw, "")
			if err != nil {
				s.unreadable++
				continue
			}
			if !seen[n.ID] {
				seen[n.ID] = true
				notes = append(notes, n)
				added++
			}
		}
		// Stop on the first page that brings nothing new, rather than on a
		// short page: the server may cap pages below the size asked for,
		// and one that ignores the offset must not cause a loop.
		if added == 0 {
			break
		}
		offset += len(docs)
	}
	if len(notes) == 0 && s.unreadable > 0 {
		return nil, &ShapeError{pathDocuments, fmt.Sprintf("none of the %d documents returned could be read", s.unreadable)}
	}
	return notes, nil
}

func documentList(data []byte) ([]json.RawMessage, error) {
	var docs []json.RawMessage
	var wrapped fields
	if json.Unmarshal(data, &wrapped) == nil && wrapped != nil {
		raw, ok := wrapped["docs"]
		if !ok {
			return nil, &ShapeError{pathDocuments, `no "docs" in the response; keys: ` + wrapped.keys()}
		}
		if err := json.Unmarshal(raw, &docs); err != nil {
			return nil, &ShapeError{pathDocuments, `"docs" is ` + jsonType(raw) + ", expected an array"}
		}
		return docs, nil
	}
	if err := json.Unmarshal(data, &docs); err != nil {
		return nil, &ShapeError{pathDocuments, "the response is " + jsonType(data) + ", expected an object or array"}
	}
	return docs, nil
}

func (s *APISource) Hydrate(ctx context.Context, n *Note, withTranscript bool) error {
	body := map[string]string{"document_id": n.ID}

	data, err := s.Client.post(ctx, pathPanels, body)
	switch {
	case isNotFound(err):
		n.PanelsUnavailable = true
		s.noPanels++
	case err != nil:
		return err
	default:
		if n.Panels, err = parsePanels(data); err != nil {
			return err
		}
	}

	if withTranscript {
		data, err := s.Client.post(ctx, pathTranscript, body)
		switch {
		case isNotFound(err):
			n.TranscriptUnavailable = true
			s.noTranscript++
		case err != nil:
			return err
		default:
			if n.Transcript, err = parseTranscript(data); err != nil {
				return err
			}
		}
	}
	n.Hydrated = true
	return nil
}

func (s *APISource) Notices() []string {
	var out []string
	if s.unreadable > 0 {
		out = append(out, fmt.Sprintf("%d documents returned by Granola could not be read and were skipped.", s.unreadable))
	}
	if s.noPanels > 0 {
		out = append(out, fmt.Sprintf("Granola answered \"not found\" for the AI notes of %d notes; they were treated as having none.", s.noPanels))
	}
	if s.noTranscript > 0 {
		out = append(out, fmt.Sprintf("Granola answered \"not found\" for the transcript of %d notes; they were treated as having none.", s.noTranscript))
	}
	return out
}
