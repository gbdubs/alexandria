package archive

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// A library's drive may be ejected or yanked while the service runs. Stopping
// (on SIGTERM/SIGINT, or on POST /api/release before an eject) refuses new
// requests, cancels background work and source syncs at a safe point (ingest
// commits one workspace at a time), waits for what is in flight, and closes
// the catalog, so nothing on the drive is left open.

// stopTimeout bounds a stop. DiskArbitration waits about 10 s for the app to
// approve an eject before unmounting anyway, so a release gives up well before
// that; the app then refuses the eject and the user can simply try again.
const stopTimeout = 7 * time.Second

var errStopTimeout = errors.New("Pharos is finishing a write; try again in a moment")

type lifecycle struct {
	ctx      context.Context // cancelled when the service starts stopping
	cancel   context.CancelFunc
	mu       sync.Mutex
	stopping bool
	released bool // a release asked for this stop
	requests sync.WaitGroup
	workers  sync.WaitGroup
	once     sync.Once
	closed   chan struct{} // closed once the catalog is
	closeErr error

	// listening runs once Serve has bound its port.
	listening func()
}

// onListen runs what waits for the service's port (see lifecycle.listening).
func (l *lifecycle) onListen() {
	if l.listening != nil {
		l.listening()
	}
}

func (l *lifecycle) init() {
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.closed = make(chan struct{})
}

// syncAdapter makes source adapters for syncs; tests replace it.
var syncAdapter = MakeAdapter

func (s *Server) cookieName() string { return fmt.Sprintf("pharos_token_%d", s.Config().Port) }

// admit counts a request in, unless the service is stopping.
func (s *Server) admit() bool {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	if s.life.stopping {
		return false
	}
	s.life.requests.Add(1)
	return true
}

// spawn runs background work, which must return soon after ctx is cancelled.
// Once the service is stopping it runs nothing and reports false.
func (s *Server) spawn(work func(ctx context.Context)) bool {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	if s.life.stopping {
		return false
	}
	s.life.workers.Add(1)
	go func() {
		defer s.life.workers.Done()
		defer s.exitOnFault()
		debug.SetPanicOnFault(true)
		work(s.life.ctx)
	}()
	return true
}

// SQLite maps the catalog's -shm index into memory. Once the drive holding it
// is yanked, touching that mapping raises SIGBUS, which crashes the process
// with a goroutine dump unless the goroutine has debug.SetPanicOnFault set.
// Every goroutine that uses the catalog sets it and defers exitOnFault (or
// calls exitIfFault from its own recover), so a yank ends the service with a
// clear message instead. SQLite's state is unusable after such a fault, so
// the process exits at once.

func (s *Server) exitOnFault() {
	if value := recover(); value != nil {
		s.exitIfFault(value)
		panic(value)
	}
}

func (s *Server) exitIfFault(value any) {
	if _, fault := value.(interface{ Addr() uintptr }); !fault {
		return
	}
	dir := filepath.Dir(s.Catalog.Path)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		fmt.Fprintf(os.Stderr, "Pharos: memory fault while %s is still present: %v\n", dir, value)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, driveGoneMessage(dir))
	os.Exit(1)
}

func driveGoneMessage(dir string) string {
	return fmt.Sprintf("Pharos: the library's drive disappeared (%s is gone); stopped without touching it. Reconnect the drive to continue.", dir)
}

func (s *Server) beginStop() {
	s.life.mu.Lock()
	s.life.stopping = true
	s.life.mu.Unlock()
	s.life.cancel()
}

// stop stops the service and closes the catalog, waiting at most timeout. A
// stop that times out carries on; a later call waits for it again.
func (s *Server) stop(timeout time.Duration) error {
	s.life.once.Do(func() {
		s.beginStop()
		go func() {
			defer s.exitOnFault()
			debug.SetPanicOnFault(true)
			s.life.requests.Wait()
			s.life.workers.Wait()
			s.life.closeErr = s.Catalog.Close()
			s.life.mu.Lock()
			released := s.life.released
			s.life.mu.Unlock()
			if released {
				// See releasedMarkerName.
				_ = os.WriteFile(releasedMarker(s.Catalog.Path), []byte(now()+"\n"), 0o644)
			}
			close(s.life.closed)
		}()
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.life.closed:
		return s.life.closeErr
	case <-timer.C:
		return errStopTimeout
	}
}

// release answers only once the catalog is closed, so the app can let the
// library's drive be ejected. The process then exits (see serveUntilStopped).
func (s *Server) release(w http.ResponseWriter) {
	s.life.mu.Lock()
	s.life.released = true
	s.life.mu.Unlock()
	if err := s.stop(stopTimeout); err != nil {
		writeJSON(w, map[string]any{"released": false, "error": err.Error()}, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"released": true}, http.StatusOK)
}

// serveUntilStopped serves until a signal, a release, or the loss of the
// catalog's drive, and returns nil only after a clean stop.
func (s *Server) serveUntilStopped(server *http.Server, listener net.Listener) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	dir := filepath.Dir(s.Catalog.Path)
	var err error
	select {
	case err = <-served:
		_ = s.stop(stopTimeout)
	case <-s.life.closed:
	case <-signals:
		// A second signal, or a stop that never finishes, still ends the process.
		go func() {
			select {
			case <-signals:
			case <-time.After(stopTimeout + 5*time.Second):
			}
			fmt.Fprintln(os.Stderr, "Pharos: exiting without closing the catalog")
			os.Exit(1)
		}()
		err = s.stop(stopTimeout)
	case <-driveGone(s.life.ctx, dir, time.Second):
		// Nothing can reach a drive that is gone, and closing the catalog would
		// touch its mapped index (see exitOnFault), so exit without it.
		s.beginStop()
		fmt.Fprintln(os.Stderr, driveGoneMessage(dir))
		os.Exit(1)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	return err
}

// driveGone is closed once dir is missing or no longer on the volume it was
// on at the start: its drive was unplugged or force-unmounted.
func driveGone(ctx context.Context, dir string, every time.Duration) <-chan struct{} {
	gone := make(chan struct{})
	device := func() (int32, bool) {
		info, err := os.Stat(dir)
		if err != nil {
			return 0, false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return stat.Dev, ok
	}
	start, ok := device()
	if !ok {
		return gone
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if current, ok := device(); !ok || current != start {
				close(gone)
				return
			}
		}
	}()
	return gone
}
