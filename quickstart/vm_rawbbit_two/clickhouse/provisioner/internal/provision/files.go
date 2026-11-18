package provision

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

// rootUID is the owner required for privileged files (tests may override).
var rootUID uint32

// geteuid is os.Geteuid (tests may override).
var geteuid = os.Geteuid

// pyParent mirrors pathlib.PurePosixPath(path).parent: it drops "." and
// empty components but, unlike filepath.Clean, keeps "..".
func pyParent(path string) string {
	parts, absolute := pyParts(path)
	if len(parts) == 0 {
		if absolute {
			return "/"
		}
		return "."
	}
	return joinParts(parts[:len(parts)-1], absolute)
}

func pyParts(path string) ([]string, bool) {
	absolute := strings.HasPrefix(path, "/")
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	return parts, absolute
}

func joinParts(parts []string, absolute bool) string {
	joined := strings.Join(parts, "/")
	switch {
	case absolute:
		return "/" + joined
	case joined == "":
		return "."
	}
	return joined
}

// pyName mirrors PurePosixPath.name.
func pyName(path string) string {
	parts, _ := pyParts(path)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// pyPath mirrors str(Path(path)).
func pyPath(path string) string {
	parts, absolute := pyParts(path)
	return joinParts(parts, absolute)
}

// pyAncestors mirrors (directory, *directory.parents).
func pyAncestors(directory string) []string {
	parts, absolute := pyParts(directory)
	out := []string{joinParts(parts, absolute)}
	for i := len(parts) - 1; i >= 0; i-- {
		out = append(out, joinParts(parts[:i], absolute))
	}
	return out
}

func ownerUID(info fs.FileInfo) uint32 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return ^uint32(0)
}

// validateRootOwnedParent rejects writable or replaceable parents.
func validateRootOwnedParent(directory string) error {
	for _, current := range pyAncestors(directory) {
		info, err := os.Lstat(current)
		if err != nil {
			return provisionErr("protected file parent directory is missing or inaccessible")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != rootUID || info.Mode().Perm()&0o022 != 0 {
			return provisionErr("protected file parent directories must be root-owned and not group/world writable")
		}
	}
	return nil
}

// SecureReadJSON mirrors _secure_read_json.
func SecureReadJSON(path string, requireRoot bool) (*pyjson.Dict, error) {
	path = pyPath(path)
	if requireRoot {
		if err := validateRootOwnedParent(pyParent(path)); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, provisionErr("protected JSON file is missing or invalid")
	}
	if !info.Mode().IsRegular() {
		return nil, provisionErr("configuration and journal paths must be regular files, not symlinks")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, provisionErr("configuration and journal files must have mode 0600 or stricter")
	}
	if requireRoot && ownerUID(info) != rootUID {
		return nil, provisionErr("privileged configuration and journal files must be root-owned")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, provisionErr("protected JSON file is missing or invalid")
	}
	parsed, err := pyjson.Decode(data)
	if err != nil {
		return nil, provisionErr("protected JSON file is missing or invalid")
	}
	obj, ok := parsed.(*pyjson.Dict)
	if !ok {
		return nil, provisionErr("protected JSON file must contain an object")
	}
	return obj, nil
}

// pathExists mirrors Path.exists() (missing or looping paths are absent).
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP),
		errors.Is(err, syscall.ENAMETOOLONG):
		return false, nil
	}
	return false, provisionErr("protected JSON file is missing or invalid")
}

// AtomicWriteJSON mirrors _atomic_write_json.
func AtomicWriteJSON(path string, value *pyjson.Dict, requireRoot bool) error {
	path = pyPath(path)
	parent := pyParent(path)
	if err := mkdirParents(parent); err != nil {
		return provisionErr("ownership journal could not be atomically written")
	}
	if info, err := os.Lstat(parent); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return provisionErr("ownership journal directory must not be a symlink")
	}
	if requireRoot {
		if err := validateRootOwnedParent(parent); err != nil {
			return err
		}
	}
	tmp, err := createTemp(parent, "."+pyName(path)+".")
	if err != nil {
		return provisionErr("ownership journal could not be atomically written")
	}
	tmpName := tmp.Name()
	defer func() {
		if _, err := os.Lstat(tmpName); err == nil {
			_ = os.Remove(tmpName)
		}
	}()
	writeErr := func() error {
		defer tmp.Close()
		if err := tmp.Chmod(0o600); err != nil {
			return err
		}
		if _, err := tmp.WriteString(pyjson.Dumps(value) + "\n"); err != nil {
			return err
		}
		return tmp.Sync()
	}()
	if writeErr != nil {
		return provisionErr("ownership journal could not be atomically written")
	}
	if requireRoot && geteuid() != 0 {
		return provisionErr("privileged ownership journal writes must run as root")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return provisionErr("ownership journal could not be atomically written")
	}
	dir, err := os.Open(parent)
	if err != nil {
		return provisionErr("ownership journal could not be atomically written")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return provisionErr("ownership journal could not be atomically written")
	}
	return nil
}

// mkdirParents mirrors Path.mkdir(parents=True, exist_ok=True, mode=0o700):
// missing ancestors get default permissions, the leaf gets 0700.
func mkdirParents(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return errors.New("not a directory")
		}
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := os.MkdirAll(parent, 0o777); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// createTemp mirrors tempfile.mkstemp(prefix=..., dir=...): an exclusive
// 0600 file named prefix + 8 random characters.
func createTemp(dir, prefix string) (*os.File, error) {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	for i := 0; i < 100; i++ {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, prefix+strings.ToLower(enc.EncodeToString(b)))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, errors.New("no usable temporary file name")
}
