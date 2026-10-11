// Package httpstream holds the HTTP plumbing the model providers share. It
// sits outside package provider, which the core imports and which must not
// know about HTTP.
package httpstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// StallTimeout is how long a model call may go without a byte: while it
// waits for the response headers, and between two reads of the streamed
// reply. It is not a limit on the whole reply. A model that reasons at
// length on local hardware streams for longer than any fixed limit, and a
// whole-request limit of ten minutes once cut a healthy stream and killed a
// sub-agent an hour into its work.
const StallTimeout = 10 * time.Minute

// NewHTTPClient is the client a provider uses when it is given none. It has
// no overall timeout: the transport bounds the wait for headers and
// WatchStall bounds the stream.
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = StallTimeout
	return &http.Client{Transport: t}
}

// WatchStall wraps a streamed response body. cancel must cancel the request
// the body belongs to. When no Read returns data for stall, WatchStall calls
// cancel, which fails the pending Read, and that Read and every later one
// report the stall instead of a bare cancellation. Close stops the watch and
// calls cancel.
func WatchStall(body io.ReadCloser, cancel context.CancelFunc, stall time.Duration) io.ReadCloser {
	w := &stallBody{body: body, cancel: cancel, stall: stall}
	w.timer = time.AfterFunc(stall, func() {
		w.stalled.Store(true)
		cancel()
	})
	return w
}

type stallBody struct {
	body    io.ReadCloser
	cancel  context.CancelFunc
	stall   time.Duration
	timer   *time.Timer
	stalled atomic.Bool
}

func (w *stallBody) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if w.stalled.Load() {
		return n, fmt.Errorf("model stream stalled: no data for %s", w.stall)
	}
	if n > 0 {
		w.timer.Reset(w.stall)
	}
	return n, err
}

func (w *stallBody) Close() error {
	w.timer.Stop()
	err := w.body.Close()
	w.cancel()
	return err
}
