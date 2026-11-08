package cdp

import (
	"context"
	"unicode/utf8"
)

// IO.read and IO.close for the streams of ReturnAsStream traces
// (tracing_domain.go). The design table has no IO domain; it is added
// because Puppeteer's page.tracing (also chrome-devtools-mcp's
// performance_start_trace) and Playwright's browser.startTracing read
// every trace through it. A stream belongs to the attachment that ended
// the trace and is dropped at its detach; the memory is bounded by
// maxStreams * maxTraceBytes per attachment.

func init() {
	register(
		Method{Name: "IO.read", Levels: OnPage, Handle: ioRead},
		Method{Name: "IO.close", Levels: OnPage, Handle: ioClose},
	)
}

// ioChunk is Chrome's default IO.read size (io_handler.cc kDefaultChunkSize).
const ioChunk = 10 << 20

// Chrome's texts (io_handler.cc).
var (
	errBadStream   = paramError("Invalid stream handle")
	errBadReadSize = paramError("Invalid max read size")
)

// ioRead reads the next chunk of a stream (from offset when given). As in
// Chrome, a chunk ends at a UTF-8 character boundary, base64Encoded is
// false (the trace is JSON text), and eof is true on the read that finds
// no data left.
func ioRead(_ context.Context, call *Call) (any, Later, error) {
	var p struct {
		Handle string `json:"handle"`
		Offset *int   `json:"offset"`
		Size   *int   `json:"size"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	size := ioChunk
	if p.Size != nil {
		if size = *p.Size; size <= 0 {
			return nil, nil, errBadReadSize
		}
	}
	x := extrasOf(call.session)
	x.mu.Lock()
	defer x.mu.Unlock()
	st := x.streams[p.Handle]
	if st == nil {
		return nil, nil, errBadStream
	}
	if p.Offset != nil && *p.Offset >= 0 {
		st.pos = min(*p.Offset, len(st.data))
	}
	if st.pos >= len(st.data) {
		return map[string]any{"data": "", "eof": true, "base64Encoded": false}, nil, nil
	}
	end := min(st.pos+size, len(st.data))
	for end > st.pos && end < len(st.data) && !utf8.RuneStart(st.data[end]) {
		end--
	}
	if end == st.pos { // size is smaller than the character at pos
		_, n := utf8.DecodeRune(st.data[st.pos:])
		end = st.pos + n
	}
	data := string(st.data[st.pos:end])
	st.pos = end
	return map[string]any{"data": data, "eof": false, "base64Encoded": false}, nil, nil
}

func ioClose(_ context.Context, call *Call) (any, Later, error) {
	var p struct {
		Handle string `json:"handle"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	x := extrasOf(call.session)
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.streams[p.Handle] == nil {
		return nil, nil, errBadStream
	}
	delete(x.streams, p.Handle)
	return map[string]any{}, nil, nil
}
