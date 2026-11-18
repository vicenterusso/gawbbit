package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AtomicWrite durably replaces path with data at the given mode.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".deploy-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs a directory.
func SyncDir(path string) error {
	fd, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

// Locked runs fn while holding an exclusive flock on path. The file is opened
// for append and never truncated or renamed: container and host share the
// inode. Waiting is bounded; active work is never killed.
func Locked(path string, timeout time.Duration, fn func() error) error {
	stream, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return errHost
	}
	defer stream.Close()
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(stream.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return errHost
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fail("Drain timed out; active work was NOT killed; gate retained")
		}
		time.Sleep(min(100*time.Millisecond, remaining))
	}
	defer syscall.Flock(int(stream.Fd()), syscall.LOCK_UN) //nolint:errcheck // released on close anyway
	return fn()
}

// Resolve mirrors pathlib.Path.resolve(strict=False): absolute, symlinks
// resolved for existing components, "." and ".." normalized.
func Resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + "/" + p
	}
	resolved := "/"
	rest := strings.Split(p, "/")
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		target, err := os.Readlink(next)
		if err != nil {
			resolved = next // not a symlink, or does not exist
			continue
		}
		if hops++; hops > 255 {
			return "", errors.New("symlink loop")
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return resolved, nil
}
