package command //nolint:testpackage // white-box: the server is made inside watch, and its deadlines take half a minute to observe

import (
	"net/http"
	"testing"
)

// TestCallbackServerBoundsEveryRead: the server events are delivered to
// gives a deadline to a request's headers, to the whole request, and to
// a connection between two requests. Without the last two, a request that
// is no event has its body read with no deadline, and an idle connection
// is kept for ever: a host on the network holds a goroutine and a
// descriptor for each.
func TestCallbackServerBoundsEveryRead(t *testing.T) {
	t.Parallel()

	server := callbackServer(http.NotFoundHandler())

	for name, timeout := range map[string]int64{
		"ReadHeaderTimeout": int64(server.ReadHeaderTimeout),
		"ReadTimeout":       int64(server.ReadTimeout),
		"IdleTimeout":       int64(server.IdleTimeout),
	} {
		if timeout <= 0 {
			t.Errorf("%s is not set", name)
		}
	}
}
