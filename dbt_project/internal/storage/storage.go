// Package storage is the small durable JSON journal used for job, attempt,
// request and fence records. It never stores SQL, credentials, or server
// errors. Records are written atomically (unique temp file, fsync, rename,
// directory fsync) with mode 0600 and are byte-compatible with the original
// Python journal: sorted keys, compact separators, ASCII escapes, newline.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
)

// Encode renders a record exactly as it is written to disk.
func Encode(value any) []byte {
	return []byte(pyjson.Compact(value) + "\n")
}

// Save atomically replaces path with the JSON encoding of value.
func Save(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(dir, filepath.Base(path)+"."+hex.EncodeToString(suffix)+".tmp")
	defer os.Remove(tmp)
	stream, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := stream.Chmod(0o600); err != nil {
		stream.Close()
		return err
	}
	if _, err := stream.Write(Encode(value)); err != nil {
		stream.Close()
		return err
	}
	if err := stream.Sync(); err != nil {
		stream.Close()
		return err
	}
	if err := stream.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory so a rename or unlink is durable.
func SyncDir(dir string) error {
	fd, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

// Load reads one record (Python json.load semantics: last duplicate wins).
func Load(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pyjson.Decode(string(data), pyjson.DecodeOptions{})
}

// LoadObject reads a record that must be a JSON object.
func LoadObject(path string) (*pyjson.Object, error) {
	value, err := Load(path)
	if err != nil {
		return nil, err
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, os.ErrInvalid
	}
	return obj, nil
}

// GlobJSON lists entries of dir whose names end in ".json" (pathlib glob
// '*.json', hidden names included). A missing directory yields nothing.
func GlobJSON(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".json") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths
}
