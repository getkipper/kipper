package migration

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// dataTransferTimeout bounds one streamed database dump or volume upload end
// to end. Data-plane calls move up to the transfer caps over whatever uplink
// the source has, so they get a far longer budget than the 5-minute
// control-plane calls.
const dataTransferTimeout = time.Hour

// countingReader counts the bytes read through it, for progress reporting on
// streamed transfers.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// streamToTarget POSTs a raw byte stream to the target. Metadata travels in
// the query string and the body is the stream itself: no base64, no JSON
// envelope, constant memory on both sides regardless of payload size.
func (h *Handler) streamToTarget(ctx context.Context, token *Token, path string, params url.Values, body io.Reader) error {
	u := token.Endpoint + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Migration-Secret", token.Secret)

	httpClient := &http.Client{
		Timeout: dataTransferTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
		// Same rule as the control-plane client: the secret and the data
		// stream go only to the address the token named.
		CheckRedirect: refuseRedirect,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", token.Endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("target returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// streamExecToTarget runs a command in a source pod and pipes its stdout
// directly into a POST against the target, returning the bytes sent. The
// exec and the upload run concurrently; whichever side fails first surfaces
// as the returned error.
func (h *Handler) streamExecToTarget(ctx context.Context, token *Token, path string, params url.Values, namespace, pod, container string, cmd []string) (int64, error) {
	pr, pw := io.Pipe()
	counter := &countingReader{r: pr}

	execDone := make(chan error, 1)
	go func() {
		err := h.execInPodTo(ctx, namespace, pod, container, cmd, nil, pw)
		// Closing with the exec error propagates it into the in-flight
		// upload, so a failed dump aborts the request instead of truncating
		// it into a "successful" partial transfer.
		pw.CloseWithError(err)
		execDone <- err
	}()

	sendErr := h.streamToTarget(ctx, token, path, params, counter)
	// Unblock the exec writer if the upload ended before consuming the whole
	// stream; on a clean transfer the pipe is already closed and this is a
	// no-op.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	execErr := <-execDone

	if sendErr != nil {
		return counter.n, sendErr
	}
	if execErr != nil {
		return counter.n, fmt.Errorf("exporting: %w", execErr)
	}
	return counter.n, nil
}
