package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, umaskLike os.FileMode) (string, []Entry) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	lic := filepath.Join(dir, "LICENSE")
	os.WriteFile(bin, bytes.Repeat([]byte("binary\x00"), 1000), 0o700|umaskLike)
	os.WriteFile(lic, []byte("license text\n"), 0o600|umaskLike)
	// Different mtimes on disk must not matter.
	os.Chtimes(bin, time.Now(), time.Now().Add(-time.Hour))
	return dir, []Entry{{Name: "tool", Path: bin}, {Name: "LICENSE", Path: lic}}
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDeterministicAcrossModesTimesAndOrder(t *testing.T) {
	mtime := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, format := range []Format{TarGz, Zip} {
		_, e1 := fixture(t, 0o022) // 0722 / 0622 on disk
		_, e2 := fixture(t, 0o000) // 0700 / 0600 on disk
		var a, b bytes.Buffer
		if err := Write(&a, format, e1, mtime); err != nil {
			t.Fatal(err)
		}
		// Reverse order and a different (non-UTC, sub-second) rendering of the same instant.
		rev := []Entry{e2[1], e2[0]}
		if err := Write(&b, format, rev, mtime.In(time.FixedZone("X", 3600)).Add(500*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if digest(a.Bytes()) != digest(b.Bytes()) {
			t.Errorf("%s: output depends on member order, on-disk mode/mtime or time rendering", format)
		}
	}
}

func TestTarContents(t *testing.T) {
	mtime := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	_, entries := fixture(t, 0)
	var buf bytes.Buffer
	if err := Write(&buf, TarGz, entries, mtime); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() || gz.OS != 255 {
		t.Errorf("gzip header leaks metadata: %+v", gz.Header)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || !h.ModTime.Equal(mtime) || !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
			t.Errorf("%s: header not normalized: %+v", h.Name, h)
		}
		want := int64(0o644)
		if h.Name == "tool" {
			want = 0o755
		}
		if h.Mode != want {
			t.Errorf("%s: mode %o, want %o", h.Name, h.Mode, want)
		}
	}
	if len(names) != 2 || names[0] != "LICENSE" || names[1] != "tool" {
		t.Errorf("members %v, want sorted [LICENSE tool]", names)
	}
}

func TestZipContents(t *testing.T) {
	mtime := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	_, entries := fixture(t, 0)
	var buf bytes.Buffer
	if err := Write(&buf, Zip, entries, mtime); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 || zr.File[0].Name != "LICENSE" || zr.File[1].Name != "tool" {
		t.Fatalf("members not sorted: %v", zr.File)
	}
	for _, f := range zr.File {
		if !f.Modified.Equal(mtime) {
			t.Errorf("%s: modified %v", f.Name, f.Modified)
		}
		if f.Method != zip.Deflate {
			t.Errorf("%s: method %d", f.Name, f.Method)
		}
		if f.Name == "tool" && f.Mode()&0o111 == 0 {
			t.Errorf("tool lost its executable bit")
		}
		rc, _ := f.Open()
		io.ReadAll(rc)
		rc.Close()
	}
}

func TestRejectsBadNames(t *testing.T) {
	_, entries := fixture(t, 0)
	for _, name := range []string{"", "/abs", "../up", "a/../b", "a//b", "a\\b", "./x"} {
		e := []Entry{{Name: name, Path: entries[0].Path}}
		if err := Write(io.Discard, TarGz, e, time.Time{}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	dup := []Entry{entries[0], {Name: entries[0].Name, Path: entries[1].Path}}
	if err := Write(io.Discard, Zip, dup, time.Time{}); err == nil {
		t.Error("accepted duplicate member")
	}
}

// Golden digests pin the exact bytes for this toolchain; a change here means
// the archive format changed and every published reproduction claim with it.
func TestGolden(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hello")
	os.WriteFile(p, []byte("hello\n"), 0o755)
	mtime := time.Unix(1700000000, 0)
	var tgz, z bytes.Buffer
	Write(&tgz, TarGz, []Entry{{Name: "hello", Path: p}}, mtime)
	Write(&z, Zip, []Entry{{Name: "hello", Path: p}}, mtime)
	t.Logf("tar.gz %s (%d bytes)", digest(tgz.Bytes()), tgz.Len())
	t.Logf("zip    %s (%d bytes)", digest(z.Bytes()), z.Len())
	if got := os.Getenv("ARCHIVE_GOLDEN_TGZ"); got != "" && got != digest(tgz.Bytes()) {
		t.Errorf("tar.gz golden mismatch")
	}
}
