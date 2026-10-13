package cmd

import (
	"bytes"
	"fmt"
	"io"

	"github.com/getkipper/kipper/kip/internal/installjob"
)

// firewallAckWriter filters firewall checks from job logs and acknowledges
// each nonce once successfully. It buffers possible markers at line starts
// while streaming ordinary output immediately.
type firewallAckWriter struct {
	out     io.Writer
	runID   string
	ack     func(nonce string) error
	pending []byte
	// Already streamed part of this line, so the remainder cannot be a marker.
	midLine bool
	acked   map[string]bool
}

var firewallMarkerBytes = []byte(installjob.FirewallCheckMarker)

func (w *firewallAckWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		nl := bytes.IndexByte(w.pending, '\n')
		if nl < 0 {
			break
		}
		line := w.pending[:nl]
		m := installjob.FirewallCheckLine.FindSubmatch(line)
		if !w.midLine && len(m) == 3 && string(m[1]) == w.runID {
			if err := w.answer(string(m[2])); err != nil {
				return 0, err
			}
		} else if _, err := w.out.Write(w.pending[:nl+1]); err != nil {
			return 0, err
		}
		w.pending = w.pending[nl+1:]
		w.midLine = false
	}
	if len(w.pending) > 0 && (w.midLine || !couldBeMarker(w.pending)) {
		if _, err := w.out.Write(w.pending); err != nil {
			return 0, err
		}
		w.pending = nil
		w.midLine = true
	}
	return len(p), nil
}

func (w *firewallAckWriter) answer(nonce string) error {
	if w.acked[nonce] {
		return nil
	}
	if err := w.ack(nonce); err != nil {
		_, werr := fmt.Fprintf(w.out, "  ⚠  could not confirm a new SSH connection: %v\n", err)
		return werr
	}
	if w.acked == nil {
		w.acked = map[string]bool{}
	}
	w.acked[nonce] = true
	return nil
}

// Flush writes the remaining partial line.
func (w *firewallAckWriter) Flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	_, err := w.out.Write(w.pending)
	w.pending = nil
	return err
}

func couldBeMarker(partial []byte) bool {
	if len(partial) <= len(firewallMarkerBytes) {
		return bytes.HasPrefix(firewallMarkerBytes, partial)
	}
	return bytes.HasPrefix(partial, firewallMarkerBytes)
}
