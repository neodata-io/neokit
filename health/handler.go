package health

import (
	"encoding/json"
	"net/http"
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
// otherwise. The body names each check and its error, because "not ready" on its
// own tells an operator nothing about what to fix.
//
// It carries no-store: a cached readiness answer is worse than none, since it
// keeps reporting the state of a probe that has since changed.
func (r *Registry) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		result := r.Check(req.Context())

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !result.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}
