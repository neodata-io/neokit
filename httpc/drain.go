package httpc

import "io"

// drainLimit bounds how much of an unread response body is consumed to keep the
// connection reusable.
//
// There is a real trade behind the number. Reading the remainder is what buys
// the reuse, but past some size the read costs more than the handshake it
// saves, and a hostile upstream would happily stream forever. 64 KiB covers the
// bodies that actually show up here — a command's ignored acknowledgement, a
// vendor's verbose 5xx explanation — and abandons anything larger, where a
// fresh connection is the cheaper answer anyway.
const drainLimit = 64 << 10

// drainClose reads and discards up to drainLimit of r before closing it, so the
// connection goes back to the idle pool instead of being torn down.
//
// net/http only reuses a connection whose body reached EOF: on a keep-alive
// socket it has to consume the current response to know where the next one
// begins. Closing an unread body therefore costs a TCP handshake on the next
// call — and a full TLS handshake on top of that over HTTPS.
//
// Measured against a test server before this existed: a command-style call that
// ignored its response body opened 20 connections for 20 calls, as did a read
// whose JSON value was followed by trailing bytes. Both drop to 1.
//
// io.Discard implements io.ReaderFrom with a pooled buffer, so this allocates
// nothing.
// The limit is drainLimit+1, not drainLimit, and the extra byte is load-bearing:
// net/http marks a connection reusable when a Read on the body returns io.EOF,
// not when the last byte happens to have been consumed. An io.LimitReader sized
// exactly to the body stops on its own counter and never issues that final Read,
// so a body of precisely drainLimit bytes would be fully read and still tear the
// connection down. One spare byte lets io.Copy make the Read that sees EOF.
func drainClose(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, drainLimit+1))
	_ = r.Close()
}
