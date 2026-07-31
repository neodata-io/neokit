package httpc

import (
	"io"
	"net/http"
	"testing"
)

// stubRoundTripper answers instantly so these benchmarks measure the transport
// stack's own overhead rather than a network.
type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(emptyReader{}),
		Request:    r,
		Header:     make(http.Header),
	}, nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func benchRequest() *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.test/v1/thing", nil)
	return req
}

func drive(b *testing.B, rt http.RoundTripper) {
	b.Helper()
	req := benchRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := rt.RoundTrip(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkTransport_BaseOnly is the floor: no neokit code in the path at all.
func BenchmarkTransport_BaseOnly(b *testing.B) {
	drive(b, stubRoundTripper{})
}

// BenchmarkTransport_RetryOnly measures the retry loop by itself, with no
// tracing wrapper — the cost neokit genuinely adds on the happy path.
func BenchmarkTransport_RetryOnly(b *testing.B) {
	drive(b, &RetryTransport{base: stubRoundTripper{}, cfg: DefaultRetryConfig()})
}

// BenchmarkTransport_Default is what a caller now gets from NewRetryTransport:
// the retry loop and nothing else. It must stay at parity with
// BenchmarkTransport_RetryOnly.
func BenchmarkTransport_Default(b *testing.B) {
	drive(b, NewRetryTransport(stubRoundTripper{}))
}

// BenchmarkTransport_Traced is the opt-in path, with no provider installed. The
// delta against BenchmarkTransport_Default is what tracing costs, and it is the
// number that justifies making it opt-in: measured at +1195ns, +2810B and +22
// allocations per request for spans nobody collects.
func BenchmarkTransport_Traced(b *testing.B) {
	drive(b, NewTracedRetryTransport(stubRoundTripper{}, DefaultRetryConfig()))
}
