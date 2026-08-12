// Package pathjail confines file operations to a directory tree.
//
// It exists because the obvious guard does not work. Checking a path with
// Lstat and then handing the same string to MkdirAll, CreateTemp, Rename or
// Remove is two independent resolutions with a gap between them, and every
// operation after the check re-resolves the ANCESTORS as well. A store that
// verified only its own directory and file was therefore writable outside its
// tree whenever any component above them was a link: the check passed, and the
// syscall that followed went somewhere else.
//
// On Windows the same guard failed for a second, independent reason. A junction
// is a reparse point but NOT a symlink, so os.Lstat reports it as ModeIrregular
// with ModeSymlink clear, and a guard testing only ModeSymlink returns "not a
// link" when pointed straight at one. Junctions also need no privilege to
// create, unlike Windows symlinks, so this is reachable by exactly the
// unprivileged user a workspace guard is meant to contain.
//
// The fix for both is to stop naming things by path. Open a handle on the root
// once and address everything below it relative to that handle, so the kernel
// resolves each component against an object that cannot be swapped underneath.
// os.Root is that handle: it refuses any component that would escape, including
// a Windows reparse point, and it is the same idea the sandbox uses for ACL
// materialization.
package pathjail

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscapes reports a target that is not inside the root it was confined to.
// Returned before anything is opened, so a caller cannot act on the path first.
var ErrEscapes = errors.New("path escapes its root")

// ErrReparse reports a link or reparse point where a real file or directory was
// required.
var ErrReparse = errors.New("refusing to write through a link")

// Open confines dir to root and returns a handle on root plus dir expressed
// relative to it. The caller closes the handle.
//
// root is resolved normally, because it is the boundary the caller chose and
// trusts (a workspace the operator opened, say, which may legitimately sit under
// a link). Everything BELOW it is resolved against the returned handle and
// cannot escape.
func Open(root, dir string) (*os.Root, string, error) {
	root = strings.TrimSpace(root)
	dir = strings.TrimSpace(dir)
	if root == "" {
		return nil, "", fmt.Errorf("%w: no root to confine %q to", ErrEscapes, dir)
	}
	if dir == "" {
		dir = root
	}
	// Rel before Open, so an escaping target is refused without opening
	// anything. filepath.Rel is lexical, which is the right test here: it says
	// what the caller ASKED for, and the handle enforces what actually happens.
	relative, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %q against %q: %v", ErrEscapes, dir, root, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("%w: %q is outside %q", ErrEscapes, dir, root)
	}
	// The root is created if it is missing, by ordinary pathname, because it is
	// the boundary the caller declared rather than anything being confined. The
	// stores this replaced called MkdirAll on the whole tree, so this creates
	// strictly less by pathname than before, and everything BELOW root is opened
	// relative to the handle.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, "", fmt.Errorf("create %s: %w", root, err)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", root, err)
	}
	return handle, relative, nil
}

// RefuseReparse fails when relative names a link or reparse point.
//
// Absent is fine: a name that does not exist yet is what a create is for.
//
// This is belt to os.Root's braces. The handle already refuses to traverse a
// reparse point, so this exists to give the FINAL component a clear error
// instead of an "escapes from parent" one, and to keep the refusal explicit at
// the point a reader looks for it.
//
// ModeIrregular is tested alongside ModeSymlink deliberately: that is what a
// Windows junction reports, and a guard checking only ModeSymlink lets one
// straight through.
func RefuseReparse(handle *os.Root, relative string) error {
	info, err := handle.Lstat(relative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", relative, err)
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%w: %s", ErrReparse, relative)
	}
	return nil
}

// CreateTemp makes an O_EXCL temp file named prefix.<random>suffix inside dir,
// relative to handle, and returns it with its relative path.
//
// O_EXCL rather than a fixed name: a link planted at a predictable temp path
// between the check and the create is the same race this package exists to
// close, and O_EXCL makes the kernel refuse it rather than us.
func CreateTemp(handle *os.Root, dir, prefix, suffix string) (*os.File, string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		noise := make([]byte, 8)
		if _, err := rand.Read(noise); err != nil {
			return nil, "", fmt.Errorf("generate a temporary name: %w", err)
		}
		relative := filepath.Join(dir, prefix+"."+hex.EncodeToString(noise)+suffix)
		file, err := handle.OpenFile(relative, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, relative, nil
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return nil, "", err
	}
	return nil, "", errors.New("could not create a temporary file with an unused name")
}
