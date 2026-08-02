package app_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
)

// serveStream mounts an SSE route built on StreamContext and returns its URL
// plus the channel the stream body reports its exit reason on.
//
// The route uses SetBodyStreamWriter deliberately: fasthttp runs that closure
// after the handler has returned and Fiber has recycled the Ctx, which is the
// case a per-request context is most likely to get wrong.
func serveStream(t *testing.T, a *app.App) (url string, exit <-chan string) {
	t.Helper()

	reason := make(chan string, 1)

	a.HTTP.Get("/events", func(c fiber.Ctx) error {
		ctx, cancel := a.StreamContext(c)
		c.Set("Content-Type", "text/event-stream")
		c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
			defer cancel()

			fmt.Fprint(w, ": open\n\n")
			_ = w.Flush()

			select {
			case <-ctx.Done():
				reason <- "drained"
			case <-time.After(5 * time.Second):
				reason <- "still streaming"
			}
		})
		return nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.HTTP.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()

	return "http://" + ln.Addr().String() + "/events", reason
}

// openStream connects and reads the first frame, so the caller knows the stream
// body is running rather than merely dialled.
func openStream(t *testing.T, url string) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if _, err := resp.Body.Read(make([]byte, 32)); err != nil {
		t.Fatalf("reading the first frame: %v", err)
	}
}

// A stream must outlive the handler that started it. Fiber pools Ctx values, so
// a context that died when the handler returned would end every stream the
// instant it opened — the failure this guards is a broken app, not a hung one.
func TestStreamContextSurvivesTheHandler(t *testing.T) {
	a := newApp(t)
	defer a.Close()

	url, exit := serveStream(t, a)
	openStream(t, url)

	select {
	case got := <-exit:
		t.Fatalf("stream ended on its own with no drain: %s", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// The whole point: shutdown must reach a parked stream, so the HTTP drain has
// nothing left to wait out.
func TestStreamContextEndsOnDrain(t *testing.T) {
	a := newApp(t)

	url, exit := serveStream(t, a)
	openStream(t, url)

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case got := <-exit:
		if got != "drained" {
			t.Errorf("stream exited for the wrong reason: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Error("drain never reached the stream")
	}
}

// cancel is documented as the thing a stream body defers, so it has to be safe
// to run more than once — a stream that ends on a write error and then unwinds
// through a second cancel must not take the process down with it.
func TestStreamContextCancelIsIdempotent(t *testing.T) {
	a := newApp(t)
	defer a.Close()

	result := make(chan error, 1)
	a.HTTP.Get("/once", func(c fiber.Ctx) error {
		ctx, cancel := a.StreamContext(c)
		cancel()
		cancel()

		select {
		case <-ctx.Done():
			result <- nil
		default:
			result <- errors.New("cancel did not cancel the stream context")
		}
		return c.SendString("ok")
	})

	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, "/once", nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if err := <-result; err != nil {
		t.Error(err)
	}
}
