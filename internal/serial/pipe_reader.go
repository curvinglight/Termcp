package serial

import (
	"io"
	"sync"
)

// pipeReader is a blocking reader fed by the channel's read loop.
//
// It exists because the session layer consumes an io.Reader and treats a nil
// error as "there may be more". A serial Read on an idle device returns (0, nil),
// which would make the session's copy loop spin at full speed; buffering here
// lets the read loop absorb that and give the session only real bytes or EOF.
//
// Reads block until data is available, the buffer is closed, or a flush is
// requested. That is the contract the session expects, and the reason Close must
// wake a blocked Read rather than leaving it stuck.
type pipeReader struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newPipeReader() *pipeReader {
	r := &pipeReader{}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// Write appends bytes and wakes a blocked reader.
func (r *pipeReader) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	// Copy: the caller reuses its read buffer, so retaining the slice would let
	// the next read overwrite unread bytes.
	r.buf = append(r.buf, p...)
	r.mu.Unlock()
	r.cond.Broadcast()
	return len(p), nil
}

// Read blocks until bytes are available or the reader is closed.
//
// It never returns (0, nil): an empty read would be interpreted downstream as a
// busy loop rather than as "nothing yet".
func (r *pipeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for len(r.buf) == 0 && !r.closed {
		r.cond.Wait()
	}

	if len(r.buf) == 0 {
		// Closed with nothing left: the stream is over.
		return 0, io.EOF
	}

	n := copy(p, r.buf)
	// Drop the consumed prefix. Re-slicing rather than reslicing to zero keeps
	// the retained capacity from growing without bound across many writes.
	remaining := len(r.buf) - n
	if remaining == 0 {
		r.buf = nil
	} else {
		r.buf = append(r.buf[:0], r.buf[n:]...)
	}
	return n, nil
}

// Close ends the stream and wakes every blocked reader.
func (r *pipeReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cond.Broadcast()
	return nil
}
