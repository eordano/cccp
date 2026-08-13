package secretary

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const (
	sessionsFile   = "sessions.json"
	sessionsLock   = "sessions.lock"
	daemonLockFile = "secretary.daemon.lock"
	pidFile        = "secretary.pid"
)

type Paths struct {
	Dir string
}

func NewPaths(root string) Paths {
	return Paths{Dir: filepath.Join(root, "cccp")}
}

func (p Paths) Sessions() string     { return filepath.Join(p.Dir, sessionsFile) }
func (p Paths) SessionsLock() string { return filepath.Join(p.Dir, sessionsLock) }
func (p Paths) DaemonLock() string   { return filepath.Join(p.Dir, daemonLockFile) }
func (p Paths) PID() string          { return filepath.Join(p.Dir, pidFile) }

func (p Paths) ensure() error {
	return os.MkdirAll(p.Dir, 0o700)
}

func (p Paths) lockedSessions(fn func() error) error {
	if err := p.ensure(); err != nil {
		return err
	}
	f, err := os.OpenFile(p.SessionsLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock sessions: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (p Paths) TryAcquireDaemon() (*os.File, error) {
	if err := p.ensure(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.DaemonLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fmt.Errorf("flock daemon: %w", err)
	}
	return f, nil
}

func (p Paths) WritePID() error {
	return os.WriteFile(p.PID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func (p Paths) RemovePID() { _ = os.Remove(p.PID()) }

func (p Paths) ReadPID() int {
	b, err := os.ReadFile(p.PID())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(bytesTrim(b)))
	if err != nil {
		return 0
	}
	return n
}

func (p Paths) DaemonRunning() bool {
	st, _ := p.DaemonLiveness()
	return st == DaemonRunning
}

type DaemonLivenessState int

const (
	DaemonAbsent DaemonLivenessState = iota

	DaemonRunning

	DaemonUnknown
)

func (p Paths) DaemonLiveness() (DaemonLivenessState, int) {
	pid := p.ReadPID()
	if pid <= 0 {
		return DaemonAbsent, 0
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return DaemonAbsent, pid
	}

	err = proc.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return DaemonRunning, pid
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return DaemonAbsent, pid
	default:

		return DaemonUnknown, pid
	}
}

func (p Paths) AddSession(id string) (int, error) {
	var n int
	err := p.lockedSessions(func() error {
		ids, err := readSessions(p.Sessions())
		if err != nil {
			return err
		}
		for _, existing := range ids {
			if existing == id {
				n = len(ids)
				return nil
			}
		}
		ids = append(ids, id)
		n = len(ids)
		return writeSessions(p.Sessions(), ids)
	})
	return n, err
}

func (p Paths) RemoveSession(id string) (int, error) {
	var n int
	err := p.lockedSessions(func() error {
		ids, err := readSessions(p.Sessions())
		if err != nil {
			return err
		}
		out := ids[:0]
		for _, existing := range ids {
			if existing != id {
				out = append(out, existing)
			}
		}
		n = len(out)
		return writeSessions(p.Sessions(), out)
	})
	return n, err
}

func (p Paths) SessionCount() (int, error) {
	var n int
	err := p.lockedSessions(func() error {
		ids, err := readSessions(p.Sessions())
		if err != nil {
			return err
		}
		n = len(ids)
		return nil
	})
	return n, err
}

func (p Paths) SignalDaemon(sig os.Signal) bool {
	pid := p.ReadPID()
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(sig); err != nil {
		return false
	}
	return true
}

func readSessions(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytesTrim(b)) == 0 {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, fmt.Errorf("parse sessions: %w", err)
	}
	return ids, nil
}

func writeSessions(path string, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	b, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}
