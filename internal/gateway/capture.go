package gateway

import "strings"

// leadingCapture retains up to limit bytes from the start of its input.
type leadingCapture struct {
	data      []byte
	limit     int
	truncated bool
}

// Write retains up to limit bytes from p and returns len(p), nil.
func (l *leadingCapture) Write(p []byte) (int, error) {
	if room := l.limit - len(l.data); room > 0 {
		l.data = append(l.data, p[:min(len(p), room)]...)
		l.truncated = l.truncated || len(p) > room
	} else if len(p) > 0 {
		l.truncated = true
	}
	return len(p), nil
}

// String returns the retained bytes as a string.
func (l *leadingCapture) String() string { return string(l.data) }

// trailingCapture retains the end of a process stream.
type trailingCapture struct {
	// data has length limit after allocation.
	data  []byte
	limit int
	// pos is the index for the next byte.
	pos int
	// full means the buffer holds limit retained bytes.
	full      bool
	truncated bool
}

// Write retains the most recent limit bytes and returns len(p), nil.
func (t *trailingCapture) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 || t.limit <= 0 {
		if n > 0 {
			t.truncated = true
		}
		return n, nil
	}
	if t.data == nil {
		t.data = make([]byte, t.limit)
	}

	if n >= t.limit {
		// The end of p contains the newest bytes.
		if t.full || t.pos > 0 || n > t.limit {
			t.truncated = true
		}
		copy(t.data, p[n-t.limit:])
		t.pos, t.full = 0, true
		return n, nil
	}

	if t.full || t.pos+n > t.limit {
		t.truncated = true
	}
	end := t.pos + n
	if end <= t.limit {
		copy(t.data[t.pos:end], p)
	} else {
		first := t.limit - t.pos
		copy(t.data[t.pos:], p[:first])
		copy(t.data, p[first:])
	}
	t.pos = end % t.limit
	t.full = t.full || end >= t.limit
	return n, nil
}

// String returns the retained bytes in write order.
func (t *trailingCapture) String() string {
	if !t.full {
		return string(t.data[:t.pos])
	}
	if t.pos == 0 {
		return string(t.data)
	}
	// pos marks the oldest byte; join the two ring-buffer segments in write order.
	var out strings.Builder
	out.Grow(t.limit)
	out.Write(t.data[t.pos:])
	out.Write(t.data[:t.pos])
	return out.String()
}
