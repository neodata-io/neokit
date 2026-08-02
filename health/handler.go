package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// LiveHandler answers 200 unconditionally. See the package doc for why it must
// never consult a dependency.
func LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// ReadyHandler runs the registry and answers 200 when every check passed, 503
// otherwise. The body carries the verdict and nothing else:
//
//	{"ready":false}
//
// Terse on purpose. This endpoint goes on the application listener, where its
// audience is an orchestrator that reads only the status code — while a body
// naming each dependency and its error is a map of your infrastructure served to
// anyone who can reach the port. The detail is not lost: it goes to the log on
// every transition (see [Registry.Log]), and [Registry.DetailHandler] serves it
// in full to a route you put behind your own authentication.
//
// It carries no-store: a cached readiness answer is worse than none, since it
// keeps reporting the state of a probe that has since changed.
func (r *Registry) ReadyHandler() http.Handler {
	return r.handler(false)
}

// DetailHandler answers exactly as [Registry.ReadyHandler] does, but with the
// full per-check body: each dependency, whether it passed, its error and how
// long it took.
//
// Mount it behind authentication. Nothing here checks who is asking — that is
// deliberate, because this package cannot know what an application's auth looks
// like, and a guess would be worse than the explicit decision to wrap it.
func (r *Registry) DetailHandler() http.Handler {
	return r.handler(true)
}

func (r *Registry) handler(detailed bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		result := r.Check(req.Context())
		r.logTransition(req.Context(), result)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !result.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		if detailed {
			_ = json.NewEncoder(w).Encode(result)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Ready bool `json:"ready"`
		}{result.Ready})
	})
}

// readiness states for the transition log. Zero is "not yet observed", so the
// first sweep always logs — a process whose readiness never changes should still
// say once what it settled on.
const (
	stateUnknown int32 = iota
	stateReady
	stateUnready
)

// logTransition writes a line when readiness changes, and only then.
//
// This is what keeps the terse body from making a failure undiagnosable. A probe
// runs on a timer, so logging every sweep would write the same line every ten
// seconds for as long as a dependency is down — the shape of noise that gets a
// log ignored. Logging the edges instead gives an operator the two lines they
// actually want: when it broke, with which check and which error, and when it
// came back.
func (r *Registry) logTransition(ctx context.Context, res Result) {
	state := stateUnready
	if res.Ready {
		state = stateReady
	}
	if r.state.Swap(state) == state {
		return
	}

	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	if res.Ready {
		log.InfoContext(ctx, "ready")
		return
	}

	// Named individually rather than counted: "2 checks failing" sends someone to
	// find out which, which is the whole job this line exists to do.
	failed := make([]string, 0, len(res.Checks))
	for _, c := range res.Checks {
		if !c.OK {
			failed = append(failed, c.Name+": "+c.Err)
		}
	}
	log.WarnContext(ctx, "not ready", "failing", strings.Join(failed, "; "))
}
