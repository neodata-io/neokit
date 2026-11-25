package delivery

import "net/http"

// HTTPClient exposes the delivery client: tests swap its transport to reach a
// loopback httptest server, which the real one refuses by design.
func (s *Sender) HTTPClient() *http.Client { return s.client }
