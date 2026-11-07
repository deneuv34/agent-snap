//go:build darwin || linux

package snapshot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func lockPathOf(store *Store) string {
	return filepath.Join(store.gitDir, restoreLockName)
}

func readLockFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func takeLock(t *testing.T, store *Store) *restoreLock {
	t.Helper()
	lock, err := store.lockRestore()
	if err != nil {
		t.Fatalf("take the restore lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.release() })
	return lock
}

func replaceLockFileStat(t *testing.T, stat func(*os.File) (time.Time, error)) {
	t.Helper()
	realStat := lockFileStat
	lockFileStat = stat
	t.Cleanup(func() { lockFileStat = realStat })
}

func replaceFlockFile(t *testing.T, flock func(*os.File, int) error) {
	t.Helper()
	realFlock := flockFile
	flockFile = flock
	t.Cleanup(func() { flockFile = realFlock })
}

func setBackThroughSecondDescriptor(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := setBackModTime(f); err != nil {
		t.Fatal(err)
	}
}

// Linux stamps files from a coarse clock that can trail time.Now by a tick.
func changeTimeFloor() time.Time {
	return time.Now().Add(-time.Second).Truncate(time.Second)
}

func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func changeTimeFollowsSetBack() func(*os.File) (time.Time, error) {
	calls := 0
	realStat := lockFileStat
	return func(f *os.File) (time.Time, error) {
		calls++
		if calls == 1 {
			return realStat(f)
		}
		info, err := f.Stat()
		if err != nil {
			return time.Time{}, err
		}
		return info.ModTime(), nil
	}
}

func TestRestoreTakesTFromTheChangeTime(t *testing.T) {
	t.Run("a set-back between the write and the read", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		path := lockPathOf(store)
		started := changeTimeFloor()
		realStat := lockFileStat
		calls := 0
		replaceLockFileStat(t, func(f *os.File) (time.Time, error) {
			calls++
			if calls == 1 {
				setBackThroughSecondDescriptor(t, path)
			}
			return realStat(f)
		})

		lock := takeLock(t, store)
		if lock.lockTime.Before(started) {
			t.Fatalf("T %v is before the test started at %v", lock.lockTime, started)
		}
		if lock.lockTime.Nanosecond() != 0 {
			t.Fatalf("T keeps whole seconds: %v", lock.lockTime)
		}
	})

	t.Run("a set-back on a file system whose change time follows it", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		path := lockPathOf(store)
		calls := 0
		replaceLockFileStat(t, func(f *os.File) (time.Time, error) {
			calls++
			if calls == 1 {
				setBackThroughSecondDescriptor(t, path)
			}
			info, err := f.Stat()
			if err != nil {
				return time.Time{}, err
			}
			return info.ModTime(), nil
		})

		_, err := store.lockRestore()
		if err == nil || err.Error() != "restore needs a file system that records change times" {
			t.Fatalf("want the change-time refusal, got %v", err)
		}
	})

	t.Run("control with no set-back", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		started := changeTimeFloor()
		if lock := takeLock(t, store); lock.lockTime.Before(started) {
			t.Fatalf("T %v is before the test started at %v", lock.lockTime, started)
		}
	})
}

func TestRestoreLockEndsWithAProcessThatInheritedIt(t *testing.T) {
	leaveSleeper := func(t *testing.T, inherit *os.File) int {
		t.Helper()
		cmd := exec.Command("sh", "-c", "sleep 60 </dev/null >/dev/null 2>&1 & echo $!")
		if inherit != nil {
			cmd.ExtraFiles = []*os.File{inherit}
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("start the child: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("read the grandchild's pid from %q: %v", out, err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		return pid
	}
	alive := func(t *testing.T, pid int) {
		t.Helper()
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("the grandchild %d must still live: %v", pid, err)
		}
	}

	t.Run("a grandchild holds the descriptor", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		lock, err := store.lockRestore()
		if err != nil {
			t.Fatal(err)
		}
		pid := leaveSleeper(t, lock.file)

		if _, err := store.lockRestore(); !errors.Is(err, ErrRestoreBusy) {
			t.Fatalf("a second lock before LOCK_UN must be busy, got %v", err)
		}
		if err := lock.release(); err != nil {
			t.Fatalf("release: %v", err)
		}
		alive(t, pid)
		takeLock(t, store)
		alive(t, pid)
	})

	t.Run("control: a grandchild started without the descriptor holds nothing", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		lock, err := store.lockRestore()
		if err != nil {
			t.Fatal(err)
		}
		pid := leaveSleeper(t, nil)
		if err := lock.file.Close(); err != nil {
			t.Fatal(err)
		}
		alive(t, pid)
		takeLock(t, store)
	})
}

func TestRestoreLockRefusesWhileHeld(t *testing.T) {
	store := newStore(t, testRepo(t))
	path := lockPathOf(store)
	takeLock(t, store)

	holder := fmt.Sprintf("core %d", os.Getpid())
	_, err := store.lockRestore()
	if !errors.Is(err, ErrRestoreBusy) {
		t.Fatalf("want ErrRestoreBusy, got %v", err)
	}
	if want := "another restore is running in this worktree: " + holder; err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
	if got := readLockFile(t, path); got != holder+"\n" {
		t.Fatalf("the holder's line changed to %q", got)
	}

	for name, content := range map[string]string{
		"a file of 40 bytes":        "core " + strings.Repeat("1", 34) + "\n",
		"a line with an escape":     "core 1\x1b[2J\n",
		"an empty file":             "",
		"a line past its last byte": "core 1\nwrite 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.lockRestore()
			if !errors.Is(err, ErrRestoreBusy) || !strings.HasSuffix(err.Error(), ": unknown") {
				t.Fatalf("want a busy refusal ending in unknown, got %v", err)
			}
			if got := readLockFile(t, path); got != content {
				t.Fatalf("the holder's file changed from %q to %q", content, got)
			}
		})
	}
}

func TestRestoreLockRefusesAReplacedFile(t *testing.T) {
	const deadCore = "core 4242\n"
	refuse := func(t *testing.T, change func(store *Store, path string)) (*Store, string) {
		t.Helper()
		store := newStore(t, testRepo(t))
		path := lockPathOf(store)
		if err := os.WriteFile(path, []byte(deadCore), 0o600); err != nil {
			t.Fatal(err)
		}
		locked, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer locked.Close()
		realFlock := flockFile
		replaceFlockFile(t, func(f *os.File, how int) error {
			if how != syscall.LOCK_UN {
				change(store, path)
			}
			return realFlock(f, how)
		})

		_, err = store.lockRestore()
		if err == nil || errors.Is(err, ErrRestoreBusy) {
			t.Fatalf("want an internal refusal, got %v", err)
		}
		got, err := io.ReadAll(locked)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != deadCore {
			t.Fatalf("the locked file changed to %q", got)
		}
		flockFile = realFlock
		return store, path
	}

	t.Run("a new file renamed onto the path", func(t *testing.T) {
		const replacement = "replacement\n"
		store, path := refuse(t, func(store *Store, path string) {
			next := filepath.Join(store.gitDir, "replacement")
			if err := os.WriteFile(next, []byte(replacement), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(next, path); err != nil {
				t.Fatal(err)
			}
		})
		if got := readLockFile(t, path); got != replacement {
			t.Fatalf("the new file changed to %q", got)
		}
		takeLock(t, store)
	})

	t.Run("a second name linked to the file", func(t *testing.T) {
		var second string
		store, _ := refuse(t, func(store *Store, path string) {
			second = filepath.Join(store.gitDir, "second-name")
			if err := os.Link(path, second); err != nil {
				t.Fatal(err)
			}
		})
		if err := os.Remove(second); err != nil {
			t.Fatal(err)
		}
		takeLock(t, store)
	})
}

func TestRestoreLockLineFormat(t *testing.T) {
	store := newStore(t, testRepo(t))
	path := lockPathOf(store)
	lock, err := store.lockRestore()
	if err != nil {
		t.Fatal(err)
	}
	if got := readLockFile(t, path); got != fmt.Sprintf("core %d\n", os.Getpid()) || !lockLinePattern.MatchString(got) {
		t.Fatalf("the core line is %q", got)
	}
	for _, step := range []struct {
		role lockRole
		id   int
		want string
	}{
		{lockCore, 1234567, "core 1234567\n"},
		{lockWrite, 42, "write 42\n"},
		{lockCheck, 7, "check 7\n"},
	} {
		if err := writeLockLine(lock.file, step.role, step.id); err != nil {
			t.Fatal(err)
		}
		if got := readLockFile(t, path); got != step.want || !lockLinePattern.MatchString(got) {
			t.Fatalf("after %s %d the file holds %q, want %q", step.role, step.id, got, step.want)
		}
	}
	if err := lock.release(); err != nil {
		t.Fatal(err)
	}
	if got := readLockFile(t, path); got != "" {
		t.Fatalf("a free lock leaves %q", got)
	}

	replaceLockFileStat(t, changeTimeFollowsSetBack())
	if _, err := store.lockRestore(); err == nil {
		t.Fatal("want a refusal")
	}
	if got := readLockFile(t, path); got != "" {
		t.Fatalf("a refusal leaves %q", got)
	}
}

func TestRestoreLockFailureIsNotBusy(t *testing.T) {
	store := newStore(t, testRepo(t))
	replaceFlockFile(t, func(*os.File, int) error { return syscall.ENOLCK })

	_, err := store.lockRestore()
	if !errors.Is(err, syscall.ENOLCK) || errors.Is(err, ErrRestoreBusy) {
		t.Fatalf("want ENOLCK and not busy, got %v", err)
	}
}

func TestRestoreLockRefusesAFileThatIsNotPlain(t *testing.T) {
	plant := map[string]func(t *testing.T, path string) (target string){
		"a symbolic link": func(t *testing.T, path string) string {
			target := filepath.Join(t.TempDir(), "outside")
			writeFile(t, filepath.Dir(target), filepath.Base(target), "outside\n")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			return target
		},
		"a hard link": func(t *testing.T, path string) string {
			target := filepath.Join(filepath.Dir(path), "other")
			writeFile(t, filepath.Dir(target), filepath.Base(target), "other\n")
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
			return target
		},
		"a FIFO": func(t *testing.T, path string) string {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("no FIFOs: %v", err)
			}
			return ""
		},
	}
	checks := map[string]func(*Store) error{
		"lock":    func(s *Store) error { _, err := s.lockRestore(); return err },
		"support": func(s *Store) error { return s.CheckRestoreSupport() },
	}
	for name, plantAt := range plant {
		for check, run := range checks {
			t.Run(name+" "+check, func(t *testing.T) {
				store := newStore(t, testRepo(t))
				target := plantAt(t, lockPathOf(store))
				var before os.FileInfo
				var content string
				if target != "" {
					before = statOf(t, target)
					content = readLockFile(t, target)
				}

				done := make(chan error, 1)
				go func() { done <- run(store) }()
				var err error
				select {
				case err = <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("the check blocked on the lock path")
				}
				if err == nil || errors.Is(err, ErrRestoreBusy) {
					t.Fatalf("want an internal refusal, got %v", err)
				}
				if target == "" {
					return
				}
				after := statOf(t, target)
				if got := readLockFile(t, target); got != content || !after.ModTime().Equal(before.ModTime()) {
					t.Fatalf("the target changed: %q at %v, was %q at %v", got, after.ModTime(), content, before.ModTime())
				}
			})
		}
	}
}

func TestRestoreLockRefusesAFileSystemWithoutChangeTimes(t *testing.T) {
	t.Run("change time follows the set-back", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		replaceLockFileStat(t, changeTimeFollowsSetBack())
		_, err := store.lockRestore()
		if err == nil || err.Error() != "restore needs a file system that records change times" {
			t.Fatalf("want the change-time refusal, got %v", err)
		}
	})
	t.Run("control on the test's file system", func(t *testing.T) {
		takeLock(t, newStore(t, testRepo(t)))
	})
}

func TestRestoreSupportRefusesAFileSystemWithoutChangeTimes(t *testing.T) {
	t.Run("change time follows the set-back", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		replaceLockFileStat(t, changeTimeFollowsSetBack())
		err := store.CheckRestoreSupport()
		if err == nil || err.Error() != "restore needs a file system that records change times" {
			t.Fatalf("want the change-time refusal, got %v", err)
		}
	})
	t.Run("control on the test's file system", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		if err := store.CheckRestoreSupport(); err != nil {
			t.Fatal(err)
		}
		if got := readLockFile(t, lockPathOf(store)); got != "" {
			t.Fatalf("the check wrote %q", got)
		}
	})
	t.Run("a held lock", func(t *testing.T) {
		store := newStore(t, testRepo(t))
		takeLock(t, store)
		if err := store.CheckRestoreSupport(); err != nil {
			t.Fatalf("the check takes no lock, got %v", err)
		}
		if got, want := readLockFile(t, lockPathOf(store)), fmt.Sprintf("core %d\n", os.Getpid()); got != want {
			t.Fatalf("the check changed the line to %q", got)
		}
	})
}

func TestRestoreLockIsPerWorktree(t *testing.T) {
	dir := testRepo(t)
	main := newStore(t, dir)
	linked := newStore(t, addWorktree(t, dir, "linked"))
	if lockPathOf(main) == lockPathOf(linked) {
		t.Fatalf("both worktrees name %s", lockPathOf(main))
	}
	takeLock(t, main)
	takeLock(t, linked)
}
