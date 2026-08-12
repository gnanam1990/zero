package pathjail

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// linkDir points link at target, using whatever kind of link this platform lets
// an unprivileged process create.
//
// On Windows that is a JUNCTION, not a symlink, and the difference is the whole
// reason this package exists twice over. A symlink needs
// SeCreateSymbolicLinkPrivilege, which an ordinary account does not hold, so a
// test written with os.Symlink silently skips on Windows and proves nothing
// there. A junction needs no privilege at all, so it is both the reachable
// attack and the only one that can be tested.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
}

// The reported defect: a link in an ANCESTOR of the target, not at the target
// itself. Every guard that checked only the final directory and file passed
// this, because the components it checked were genuinely not links; the ones
// the syscall then traversed were.
func TestAnAncestorLinkCannotRedirectAWrite(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	workspace := filepath.Join(base, "workspace")
	for _, dir := range []string{outside, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkDir(t, outside, filepath.Join(workspace, ".zero"))

	handle, relative, err := Open(workspace, filepath.Join(workspace, ".zero", "store"))
	if err != nil {
		t.Fatalf("Open should confine, not refuse outright: %v", err)
	}
	defer handle.Close()

	if err := handle.MkdirAll(relative, 0o700); err == nil {
		t.Fatal("created a directory through a linked ancestor, so a write can still leave the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "store")); !os.IsNotExist(err) {
		t.Errorf("something was created outside the workspace, stat error = %v", err)
	}
}

// RefuseReparse has to catch a junction, which is the case os.ModeSymlink alone
// misses. Asserted through the exported helper rather than by reading the mode,
// so the test fails if the predicate is ever narrowed back.
func TestRefuseReparseCatchesAJunctionNotJustASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	inside := filepath.Join(base, "inside")
	for _, dir := range []string{target, inside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkDir(t, target, filepath.Join(inside, "linked"))

	handle, relative, err := Open(inside, inside)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	err = RefuseReparse(handle, filepath.Join(relative, "linked"))
	if !errors.Is(err, ErrReparse) {
		t.Fatalf("RefuseReparse(link) = %v, want ErrReparse; on Windows a junction reports ModeIrregular, so a ModeSymlink-only check returns nil here", err)
	}
}

// A name that does not exist yet is what a create is for, so it must not be
// refused. Without this the guard above would be satisfied by refusing
// everything.
func TestRefuseReparseAllowsAnAbsentName(t *testing.T) {
	base := t.TempDir()
	handle, relative, err := Open(base, base)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := RefuseReparse(handle, filepath.Join(relative, "not-there.md")); err != nil {
		t.Errorf("an absent name was refused: %v", err)
	}
	if err := RefuseReparse(handle, relative); err != nil {
		t.Errorf("an ordinary directory was refused: %v", err)
	}
}

// Open refuses a target outside its root before opening anything, so a caller
// cannot act on the path first.
func TestOpenRefusesATargetOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		filepath.Join(base, "sibling"),
		filepath.Join(inside, "..", "..", "escape"),
	} {
		if _, _, err := Open(inside, target); !errors.Is(err, ErrEscapes) {
			t.Errorf("Open(%q, %q) = %v, want ErrEscapes", inside, target, err)
		}
	}
	if _, _, err := Open("", filepath.Join(base, "anything")); !errors.Is(err, ErrEscapes) {
		t.Error("an empty root was accepted; a boundary is not optional")
	}
}

// CreateTemp must use O_EXCL and an unpredictable name, so a link planted at a
// guessable temp path is refused by the kernel rather than followed.
func TestCreateTempIsExclusiveAndUnpredictable(t *testing.T) {
	base := t.TempDir()
	handle, relative, err := Open(base, base)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		file, name, err := CreateTemp(handle, relative, "note", ".tmp")
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		if seen[name] {
			t.Fatalf("CreateTemp reused the name %q, so two concurrent writes would stomp each other", name)
		}
		seen[name] = true
		if filepath.Ext(name) != ".tmp" {
			t.Errorf("temp name %q lost its suffix, so a listing may pick it up as real content", name)
		}
	}
}
