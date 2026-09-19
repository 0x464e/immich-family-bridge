package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestHardlinkIdentityAndUnlink(t *testing.T) {
	root := t.TempDir()
	srcRoot := filepath.Join(root, "source")
	dstRoot := filepath.Join(root, "bridge")
	if err := os.MkdirAll(srcRoot, 0750); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(srcRoot, "photo.jpg")
	if err := os.WriteFile(source, []byte("fake jpeg bytes"), 0640); err != nil {
		t.Fatal(err)
	}
	l := Linker{SourceRoot: srcRoot, BridgeRoot: dstRoot}
	dest, e := l.Destination("fam", "bob", "alice", "asset-1", source)
	if e != nil {
		t.Fatal(e)
	}
	if filepath.Ext(dest) != ".jpg" {
		t.Fatal(dest)
	}
	if sidecar, err := l.SidecarDestination(dest, source+".xmp"); err != nil || sidecar != dest+".xmp" {
		t.Fatalf("sidecar destination %q: %v", sidecar, err)
	}
	for i := 0; i < 2; i++ {
		if e := l.Ensure(source, dest); e != nil {
			t.Fatal(e)
		}
	}
	a, _ := os.Stat(source)
	b, _ := os.Stat(dest)
	if !os.SameFile(a, b) {
		t.Fatal("different inodes")
	}
	if stat, ok := a.Sys().(*syscall.Stat_t); ok && stat.Nlink != 2 {
		t.Fatalf("link count %d", stat.Nlink)
	}
	if e := os.Remove(dest); e != nil {
		t.Fatal(e)
	}
	if _, e := os.ReadFile(source); e != nil {
		t.Fatal("unlink removed source:", e)
	}
	if e := os.WriteFile(dest, []byte("conflict"), 0640); e != nil {
		t.Fatal(e)
	}
	if e := l.Ensure(source, dest); !errors.Is(e, ErrConflict) {
		t.Fatalf("wanted conflict, got %v", e)
	}
}

func TestCrossFilesystem(t *testing.T) {
	srcRoot := t.TempDir()
	other, err := os.MkdirTemp("/dev/shm", "familybridge-test-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(other)
	src := filepath.Join(srcRoot, "a.jpg")
	_ = os.WriteFile(src, []byte("x"), 0640)
	a, _ := os.Stat(srcRoot)
	b, _ := os.Stat(other)
	if a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev {
		t.Skip("same filesystem")
	}
	l := Linker{SourceRoot: srcRoot, BridgeRoot: other}
	e := l.Ensure(src, filepath.Join(other, "a.jpg"))
	if !errors.Is(e, ErrCrossFilesystem) {
		t.Fatalf("wanted EXDEV, got %v", e)
	}
}

func TestRejectSymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dst := filepath.Join(root, "bridge")
	_ = os.Mkdir(src, 0750)
	real := filepath.Join(src, "real.jpg")
	_ = os.WriteFile(real, []byte("x"), 0640)
	link := filepath.Join(src, "link.jpg")
	_ = os.Symlink(real, link)
	if err := (Linker{SourceRoot: src, BridgeRoot: dst}).Ensure(link, filepath.Join(dst, "link.jpg")); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestReadOnlyLinkerRejectsLinkBeforeCreatingDirectories(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.jpg")
	if err := os.WriteFile(source, []byte("image"), 0640); err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "bridge")
	l := Linker{SourceRoot: root, BridgeRoot: bridge, ReadOnly: true}
	if err := l.Ensure(source, filepath.Join(bridge, "photo.jpg")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only linker returned %v", err)
	}
	if _, err := os.Stat(bridge); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only linker created directory: %v", err)
	}
}
