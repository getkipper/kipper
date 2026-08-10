// Package export implements the sending side of a transfer: it builds the
// manifest, negotiates resume state with the import server, uploads missing
// chunks in parallel, and verifies the finalize report against the manifest.
package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/getkipper/kipper/datamover/internal/chunk"
	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

// Client drives one transfer against an import server.
type Client struct {
	// HTTP is the client used for all requests.
	HTTP *http.Client
	// BaseURL is the target base URL, without the /kipper-transfer prefix.
	BaseURL string
	// TransferID names the transfer in every URL.
	TransferID string
	// Token is the bearer token; it is never logged.
	Token string
	// Source provides ranged reads of the manifest units.
	Source Source
	// Manifest describes the data to move.
	Manifest *manifest.Manifest
	// Concurrency is the number of parallel chunk uploads (default 4).
	Concurrency int
	// MaxAttempts is the upload attempt budget per chunk (default 3).
	MaxAttempts int
	// Backoff is the base delay between chunk retries (default 1s, doubling).
	Backoff time.Duration
	// Logf, when set, receives progress lines.
	Logf func(format string, args ...any)

	raw  []byte
	plan *chunk.Plan
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// NewHTTPClient returns the transport posture every transfer request uses:
// TLS 1.2 minimum and no redirects, because following one would re-send the
// bearer token to wherever the redirect points.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
		CheckRedirect: refuseRedirect,
	}
}

// refuseRedirect fails any redirect: the token goes only to the configured
// target.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing redirect to %s: transfer requests must reach the configured target directly", req.URL.Redacted())
}

func (c *Client) defaults() {
	if c.HTTP == nil {
		c.HTTP = NewHTTPClient()
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.Backoff <= 0 {
		c.Backoff = time.Second
	}
}

// Run executes the full transfer: manifest, resume state, chunk uploads,
// finalize, and report verification. It returns an error on any mismatch.
func (c *Client) Run(ctx context.Context) error {
	c.defaults()
	if err := c.Manifest.Validate(); err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}
	var err error
	c.raw, err = manifest.Encode(c.Manifest)
	if err != nil {
		return err
	}
	c.plan = chunk.NewPlan(c.Manifest)
	if err := c.postManifest(ctx); err != nil {
		return fmt.Errorf("sending manifest: %w", err)
	}
	state, err := c.getState(ctx)
	if err != nil {
		return fmt.Errorf("fetching resume state: %w", err)
	}
	if err := c.uploadChunks(ctx, state.CompletedChunks); err != nil {
		return err
	}
	report, err := c.finalize(ctx)
	if err != nil {
		return fmt.Errorf("finalizing: %w", err)
	}
	return c.verifyReport(report)
}

func (c *Client) url(op string) string {
	return wire.URL(c.BaseURL, c.TransferID, op)
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return c.HTTP.Do(req)
}

// checkStatus drains and closes the body on non-2xx and returns a StatusError.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)) //nolint:errcheck // best-effort error context
	_ = resp.Body.Close()
	return &wire.StatusError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(body))}
}

func (c *Client) postManifest(ctx context.Context) error {
	compressed, err := manifest.Compress(c.raw)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("manifest"), bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return checkStatus(resp)
}

func (c *Client) getState(ctx context.Context) (*wire.StateResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("state"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var state wire.StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decoding state: %w", err)
	}
	if state.ManifestDigest != manifest.Digest(c.raw) {
		return nil, fmt.Errorf("server manifest digest %s does not match ours", state.ManifestDigest)
	}
	return &state, nil
}

func (c *Client) uploadChunks(ctx context.Context, done wire.Bitmap) error {
	total := c.plan.NumChunks()
	var missing []int
	for n := 0; n < total; n++ {
		if !done.Get(n) {
			missing = append(missing, n)
		}
	}
	c.logf("uploading %d of %d chunks (%d already on target)", len(missing), total, total-len(missing))
	if len(missing) == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan int)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	workers := min(c.Concurrency, len(missing))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				if err := c.uploadChunk(ctx, n); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
	for _, n := range missing {
		select {
		case work <- n:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (c *Client) uploadChunk(ctx context.Context, n int) error {
	spans := c.plan.Spans(n)
	sum, err := c.hashSpans(ctx, spans)
	if err != nil {
		return fmt.Errorf("hashing chunk %d: %w", n, err)
	}
	var lastErr error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		if attempt > 1 {
			delay := c.Backoff << uint(attempt-2)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		lastErr = c.putChunk(ctx, n, spans, sum)
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			return fmt.Errorf("uploading chunk %d: %w", n, lastErr)
		}
	}
	return fmt.Errorf("uploading chunk %d: %w (after %d attempts)", n, lastErr, c.MaxAttempts)
}

// retryable reports whether a chunk upload failure is worth another attempt:
// transport errors, 5xx, and 422 (payload corrupted in flight).
func retryable(err error) bool {
	var se *wire.StatusError
	if errors.As(err, &se) {
		return se.Status >= 500 || se.Status == http.StatusUnprocessableEntity
	}
	return !errors.Is(err, context.Canceled)
}

func (c *Client) hashSpans(ctx context.Context, spans []chunk.Span) (string, error) {
	h := sha256.New()
	for _, sp := range spans {
		r, err := c.Source.OpenRange(ctx, sp.Path, sp.FileOffset, sp.Length)
		if err != nil {
			return "", err
		}
		n, err := io.Copy(h, r)
		_ = r.Close()
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", sp.Path, err)
		}
		if n != sp.Length {
			return "", fmt.Errorf("short read on %s: got %d of %d bytes", sp.Path, n, sp.Length)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) putChunk(ctx context.Context, n int, spans []chunk.Span, sum string) error {
	pr, pw := io.Pipe()
	go func() {
		enc, err := zstd.NewWriter(pw)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		for _, sp := range spans {
			r, err := c.Source.OpenRange(ctx, sp.Path, sp.FileOffset, sp.Length)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			copied, err := io.Copy(enc, r)
			_ = r.Close()
			if err != nil {
				pw.CloseWithError(fmt.Errorf("reading %s: %w", sp.Path, err))
				return
			}
			if copied != sp.Length {
				pw.CloseWithError(fmt.Errorf("short read on %s: got %d of %d bytes", sp.Path, copied, sp.Length))
				return
			}
		}
		pw.CloseWithError(enc.Close())
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(fmt.Sprintf("chunk/%d", n)), pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	req.Header.Set(wire.HeaderChunkSHA256, sum)
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := c.do(req)
	if err != nil {
		_ = pr.Close()
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return checkStatus(resp)
}

func (c *Client) finalize(ctx context.Context) (*wire.Report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("finalize"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var report wire.Report
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return nil, fmt.Errorf("decoding finalize report: %w", err)
	}
	return &report, nil
}

// verifyReport checks every manifest entry appears in the report with a
// matching hash (files) or a confirmed commit (dirs, symlinks) and no apply
// error.
func (c *Client) verifyReport(report *wire.Report) error {
	results := make(map[string]wire.FileResult, len(report.Files))
	for _, r := range report.Files {
		results[r.Path] = r
	}
	var problems []string
	for _, e := range c.Manifest.Entries {
		r, ok := results[e.Path]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: missing from report", e.Path))
		case !r.Match || r.SHA256 != e.SHA256:
			problems = append(problems, fmt.Sprintf("%s: hash mismatch (target %s)", e.Path, r.SHA256))
		case r.Error != "":
			problems = append(problems, fmt.Sprintf("%s: %s", e.Path, r.Error))
		}
	}
	if len(report.Files) != len(c.Manifest.Entries) {
		problems = append(problems, fmt.Sprintf("report covers %d entries, manifest has %d", len(report.Files), len(c.Manifest.Entries)))
	}
	if len(problems) > 0 {
		return fmt.Errorf("finalize verification failed: %s", joinLimited(problems, 10))
	}
	c.logf("verified %d files, %d target entries deleted", len(report.Files), len(report.Deleted))
	return nil
}

func joinLimited(items []string, limit int) string {
	if len(items) > limit {
		items = append(items[:limit:limit], fmt.Sprintf("and %d more", len(items)-limit))
	}
	out := items[0]
	for _, s := range items[1:] {
		out += "; " + s
	}
	return out
}
