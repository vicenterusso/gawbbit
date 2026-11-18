package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathlibSemantics(t *testing.T) {
	// Expected values follow pathlib.PurePosixPath.
	for in, want := range map[string][2]string{
		"/srv/rawbbit-two/clickhouse/x.json": {"/srv/rawbbit-two/clickhouse", "x.json"},
		"journal.json":                       {".", "journal.json"},
		"a//b/./c.json":                      {"a/b", "c.json"},
		"a/../b.json":                        {"a/..", "b.json"},
		"/x.json":                            {"/", "x.json"},
		"":                                   {".", ""},
		"dir/":                               {".", "dir"},
	} {
		if got := pyParent(in); got != want[0] {
			t.Errorf("parent(%q) = %q, want %q", in, got, want[0])
		}
		if got := pyName(in); got != want[1] {
			t.Errorf("name(%q) = %q, want %q", in, got, want[1])
		}
	}
	if got := strings.Join(pyAncestors("/srv/a/b"), ","); got != "/srv/a/b,/srv/a,/srv,/" {
		t.Errorf("ancestors %s", got)
	}
	if got := strings.Join(pyAncestors("a/b"), ","); got != "a/b,a,." {
		t.Errorf("relative ancestors %s", got)
	}
	if got := strings.Join(pyAncestors("."), ","); got != "." {
		t.Errorf("dot ancestors %s", got)
	}
}

func TestRootOwnedParentValidation(t *testing.T) {
	dir := t.TempDir()
	// The temporary directory's ancestors are not owned by the current uid.
	saved := rootUID
	rootUID = uint32(os.Getuid())
	t.Cleanup(func() { rootUID = saved })
	err := validateRootOwnedParent(dir)
	expectError(t, err, "root-owned and not group/world writable|missing or inaccessible")

	expectError(t, validateRootOwnedParent(filepath.Join(dir, "missing")), "missing or inaccessible")

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = SecureReadJSON(path, true)
	expectError(t, err, "parent directories must be root-owned")

	link := filepath.Join(dir, "link.json")
	_ = os.Symlink(path, link)
	_, err = SecureReadJSON(link, false)
	expectError(t, err, "regular files, not symlinks")
}

func TestAtomicWriteRefusesNonRootPrivilegedWrites(t *testing.T) {
	saved, savedEUID := rootUID, geteuid
	rootUID = uint32(os.Getuid())
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { rootUID, geteuid = saved, savedEUID })
	err := AtomicWriteJSON(filepath.Join(t.TempDir(), "j.json"), NewJournal(), true)
	expectError(t, err, "root-owned|must run as root")

	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	_ = os.Mkdir(target, 0o700)
	_ = os.Symlink(target, filepath.Join(dir, "linked"))
	err = AtomicWriteJSON(filepath.Join(dir, "linked", "j.json"), NewJournal(), false)
	expectError(t, err, "directory must not be a symlink")

	nested := filepath.Join(t.TempDir(), "a", "b", "j.json")
	if err := AtomicWriteJSON(nested, NewJournal(), false); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Dir(nested))
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("leaf directory mode %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(nested))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestHasStableUUID(t *testing.T) {
	for in, want := range map[any]bool{
		"61f0c404-5cb3-11e7-907b-a6006ad3dba0": true, "00000000-0000-0000-0000-000000000000": false,
		"{61f0c404-5cb3-11e7-907b-a6006ad3dba0}": true, "urn:uuid:61f0c404-5cb3-11e7-907b-a6006ad3dba0": true,
		"61f0c4045cb311e7907ba6006ad3dba0": true, "not-a-uuid": false, nil: false, "61f0c404": false,
	} {
		if got := hasStableUUID(in); got != want {
			t.Errorf("hasStableUUID(%v) = %v", in, got)
		}
	}
}
