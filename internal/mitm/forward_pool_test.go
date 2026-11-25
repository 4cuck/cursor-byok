package mitm

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// burstBackend is a backend that answers a burst of requests only once all of
// them have arrived, so every request of the burst needs its own connection.
type burstBackend struct {
	server *httptest.Server
	opened atomic.Int32

	mu      sync.Mutex
	waiting int
	size    int
	release chan struct{}
}

func newBurstBackend(t *testing.T) *burstBackend {
	t.Helper()
	backend := &burstBackend{}
	backend.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		backend.mu.Lock()
		release := backend.release
		backend.waiting++
		if backend.waiting == backend.size {
			close(release)
		}
		backend.mu.Unlock()
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	backend.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			backend.opened.Add(1)
		}
	}
	backend.server.Start()
	t.Cleanup(backend.server.Close)
	return backend
}

func (backend *burstBackend) expect(size int) {
	backend.mu.Lock()
	backend.waiting = 0
	backend.size = size
	backend.release = make(chan struct{})
	backend.mu.Unlock()
}

// The Cursor client sends the frames of a stream side by side, up to 32 at a
// time, and each one is forwarded to the backend on a connection of its own.
// Those connections have to be kept for the next burst. With the default of
// two idle connections per host, every burst closed all but two of them and
// dialed them again.
func TestForwardToServerKeepsBackendConnectionsBetweenBursts(t *testing.T) {
	const burst = 16
	backend := newBurstBackend(t)
	proxy, err := NewProxyServer("127.0.0.1:0", backend.server.URL, "", nil, nil)
	if err != nil {
		t.Fatalf("NewProxyServer() error = %v", err)
	}

	sendBurst := func() {
		backend.expect(burst)
		var requests sync.WaitGroup
		for i := 0; i < burst; i++ {
			requests.Add(1)
			go func() {
				defer requests.Done()
				incoming := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh/aiserver.v1.BidiService/BidiAppend", strings.NewReader("frame"))
				resp, err := proxy.forwardToServer(incoming)
				if err != nil {
					t.Errorf("forwardToServer() error = %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
		}
		requests.Wait()
	}

	sendBurst()
	afterFirst := backend.opened.Load()
	if afterFirst != burst {
		t.Fatalf("first burst opened %d connections, want %d (one per concurrent request)", afterFirst, burst)
	}
	sendBurst()

	// A connection that goes idle a moment late may be dialed again; anything
	// near a second full set means the pool was thrown away.
	if reopened := backend.opened.Load() - afterFirst; reopened > 2 {
		t.Fatalf("second burst dialed %d new connections to the backend, want the %d from the first burst reused", reopened, burst)
	}
}
