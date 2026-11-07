//go:build darwin || linux

package snapshot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const (
	restoreLockName = "agent-snap-restore.lock"
	lockLineLimit   = 32
)

type lockRole string

const (
	lockCore  lockRole = "core"
	lockWrite lockRole = "write"
	lockCheck lockRole = "check"
)

var (
	ErrRestoreBusy   = errors.New("another restore is running in this worktree")
	errNoChangeTimes = errors.New("restore needs a file system that records change times")

	lockLinePattern = regexp.MustCompile(`^(core|write|check) [1-9][0-9]*\n$`)
	setBackTime     = time.Unix(1_000_000_000, 0)

	flockFile    = func(f *os.File, how int) error { return syscall.Flock(int(f.Fd()), how) }
	lockFileStat = func(f *os.File) (time.Time, error) {
		info, err := f.Stat()
		if err != nil {
			return time.Time{}, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return time.Time{}, errors.New("restore lock file has no stat data")
		}
		return changeTime(st), nil
	}
)

type restoreLock struct {
	file     *os.File
	lockTime time.Time
}

func (s *Store) restoreLockPath() string {
	return filepath.Join(s.gitDir, restoreLockName)
}

func (s *Store) lockRestore() (*restoreLock, error) {
	path := s.restoreLockPath()
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := flockFile(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrRestoreBusy, lockHolder(f))
		}
		return nil, fmt.Errorf("take restore lock: %w", err)
	}
	lock := &restoreLock{file: f}
	// No child holds the descriptor yet, so its close ends the lock even when
	// LOCK_UN fails.
	if err := lockedFileIsOnPath(f, path); err != nil {
		// A refused file can have another name, so nothing writes to it.
		_ = lock.unlock()
		return nil, err
	}
	if err := lock.start(); err != nil {
		_ = lock.release()
		return nil, err
	}
	return lock, nil
}

func (l *restoreLock) start() error {
	if err := writeLockLine(l.file, lockCore, os.Getpid()); err != nil {
		return err
	}
	// No process can set a change time back, so a ping's set-back of the
	// modification time between the write and this read cannot reach T.
	changed, err := lockFileStat(l.file)
	if err != nil {
		return fmt.Errorf("read restore lock: %w", err)
	}
	l.lockTime = time.Unix(changed.Unix(), 0)
	return checkChangeTimes(l.file, l.lockTime)
}

// A file deleted or replaced between the open and the lock would let a
// second restore lock the new file while this one holds the old.
func lockedFileIsOnPath(f *os.File, path string) error {
	locked, err := f.Stat()
	if err != nil {
		return fmt.Errorf("read restore lock: %w", err)
	}
	onPath, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("read restore lock: %w", err)
	}
	if !regularWithOneLink(locked) || !os.SameFile(locked, onPath) {
		return fmt.Errorf("restore lock %s was replaced while it was locked", path)
	}
	return nil
}

func (l *restoreLock) release() error {
	return errors.Join(l.file.Truncate(0), l.unlock())
}

// A process that inherited the descriptor keeps a lock held past the close,
// and LOCK_UN ends it for every holder.
func (l *restoreLock) unlock() error {
	return errors.Join(flockFile(l.file, syscall.LOCK_UN), l.file.Close())
}

// CheckRestoreSupport runs the lock step's checks of the lock file and its
// file system without taking the lock or writing a line, so it can run while
// a restore holds the lock. It sets the lock file's modification time back.
func (s *Store) CheckRestoreSupport() error {
	f, err := openLockFile(s.restoreLockPath())
	if err != nil {
		return err
	}
	defer f.Close()
	changed, err := lockFileStat(f)
	if err != nil {
		return fmt.Errorf("read restore lock: %w", err)
	}
	return checkChangeTimes(f, time.Unix(changed.Unix(), 0))
}

// A symbolic link or a hard link at the path would make the lock truncate
// another file and take T from another file system.
func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open restore lock: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read restore lock: %w", err)
	}
	if !regularWithOneLink(info) {
		f.Close()
		return nil, fmt.Errorf("restore lock %s is not a regular file with one link", path)
	}
	return f, nil
}

func regularWithOneLink(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && uint64(st.Nlink) == 1
}

// On FAT the change time follows the set-back, and Git's stat check can then
// trust a set-back edit.
func checkChangeTimes(f *os.File, lockTime time.Time) error {
	if err := setBackModTime(f); err != nil {
		return fmt.Errorf("set restore lock time: %w", err)
	}
	changed, err := lockFileStat(f)
	if err != nil {
		return fmt.Errorf("read restore lock: %w", err)
	}
	if !changed.After(setBackTime) || changed.Before(lockTime) {
		return errNoChangeTimes
	}
	return nil
}

func setBackModTime(f *os.File) error {
	tv := syscall.NsecToTimeval(setBackTime.UnixNano())
	return syscall.Futimes(int(f.Fd()), []syscall.Timeval{tv, tv})
}

func writeLockLine(f *os.File, role lockRole, id int) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("write restore lock line: %w", err)
	}
	if _, err := f.WriteAt(fmt.Appendf(nil, "%s %d\n", role, id), 0); err != nil {
		return fmt.Errorf("write restore lock line: %w", err)
	}
	return nil
}

// Bytes that do not form a whole line read as unknown, so nothing another
// process wrote reaches a terminal.
func lockHolder(f *os.File) string {
	buf := make([]byte, lockLineLimit)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "unknown"
	}
	line := buf[:n]
	if !lockLinePattern.Match(line) {
		return "unknown"
	}
	return string(line[:n-1])
}
