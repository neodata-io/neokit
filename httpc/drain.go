package httpc

import "io"

// drainLimit bounds how much of an unread response body is consumed to keep the
// connection reusable. Past some size the read costs more than the handshake it
// saves, and a hostile upstream would stream forever; 64 KiB covers the bodies
// that show up here and abandons anything larger, where a fresh connection is
// cheaper anyway.
const drainLimit = 64 << 10

// drainClose reads and discards up to drainLimit of r before closing it, so the
// connection goes back to the idle pool instead of being torn down.
//
// net/http only reuses a connection whose body reached EOF: on a keep-alive
// socket it has to consume the current response to know where the next one
// begins. Closing an unread body therefore costs a TCP handshake on the next
// call, plus a full TLS handshake over HTTPS.
//
// The limit is drainLimit+1, and the extra byte is load-bearing: net/http marks
// a connection reusable when a Read returns io.EOF, not when the last byte is
// consumed. An io.LimitReader sized exactly to the body stops on its own counter
// and never issues that final Read, so a body of precisely drainLimit bytes
// would be fully read and still tear the connection down.
//
// io.Discard implements io.ReaderFrom with a pooled buffer, so this allocates
// nothing.
func drainClose(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, drainLimit+1))
	_ = r.Close()
}
