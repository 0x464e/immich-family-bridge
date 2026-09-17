package filesystem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrCrossFilesystem = errors.New("hardlink crosses filesystem or mount boundary")
var ErrConflict = errors.New("recipient path already refers to different file")

type Linker struct{ SourceRoot, BridgeRoot string }

func within(root, p string) bool {
	r, e := filepath.Rel(root, p)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

func noSymlink(path string) error {
	path = filepath.Clean(path)
	for p := path; p != "/" && p != "."; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in path: %s", p)
		}
	}
	return nil
}

func (l Linker) Destination(family, member, originMember, originAsset, source string) (string, error) {
	for _, part := range []string{family, member, originMember, originAsset} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\") {
			return "", errors.New("unsafe path identifier")
		}
	}
	ext := filepath.Ext(source)
	if ext == "" || len(ext) > 16 || strings.ContainsAny(ext, "/\\") {
		return "", errors.New("unsupported source extension")
	}
	return filepath.Join(l.BridgeRoot, "families", family, "users", member, "assets", originMember, originAsset+ext), nil
}

func (l Linker) Ensure(source, dest string) error {
	if !within(l.BridgeRoot, dest) {
		return errors.New("link path outside configured root")
	}
	src, err := l.SourceInfo(source)
	if err != nil {
		return err
	}
	if err := noSymlink(filepath.Dir(dest)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0750); err != nil {
		return err
	}
	if err := noSymlink(filepath.Dir(dest)); err != nil {
		return err
	}
	if dst, err := os.Lstat(dest); err == nil {
		if dst.Mode().IsRegular() && os.SameFile(src, dst) {
			return nil
		}
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(source, dest); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("%w: %v", ErrCrossFilesystem, err)
		}
		if errors.Is(err, os.ErrExist) {
			dst, e := os.Lstat(dest)
			if e == nil && dst.Mode().IsRegular() && os.SameFile(src, dst) {
				return nil
			}
			return ErrConflict
		}
		return err
	}
	return nil
}

func (l Linker) SourceInfo(source string) (os.FileInfo, error) {
	if !within(l.SourceRoot, source) || within(l.BridgeRoot, source) {
		return nil, errors.New("source path outside configured source root")
	}
	if err := noSymlink(source); err != nil {
		return nil, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	return info, nil
}
