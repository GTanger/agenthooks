//go:build !windows

package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// stateDir roots the sockets and lock files:
// $XDG_STATE_HOME/agenthooks, falling back to the user cache dir, then the
// system temp dir.
func stateDir() (string, error) {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "agenthooks"), nil
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "agenthooks"), nil
	}
	return filepath.Join(os.TempDir(), "agenthooks"), nil
}

func endpoint(dir, id string) string {
	name := "agenthooks-" + id + ".sock"
	path := filepath.Join(dir, name)
	if len(path) > maxSocketPath {
		return filepath.Join(os.TempDir(), name)
	}
	return path
}

func lockPath(dir, id, suffix string) string {
	return filepath.Join(dir, "agenthooks-"+id+suffix)
}

// Listen binds the endpoint. A socket file left behind by a crashed server
// (its advisory locks died with it, so the caller holds the server lock) is
// probed and swept; if something actually answers, ErrAlreadyRunning.
func Listen(endpoint string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", endpoint)
	if err == nil {
		return ln, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, err
	}
	if dialProbe(endpoint) {
		return nil, ErrAlreadyRunning
	}
	_ = os.Remove(endpoint)
	return net.Listen("unix", endpoint)
}

// Dial connects to a listening server, bounded by timeout.
func Dial(endpoint string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", endpoint, timeout)
}
