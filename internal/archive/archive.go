// Package archive writes byte-for-byte reproducible tar.gz and zip archives.
//
// Every source of nondeterminism that archivers normally leak is fixed here:
// members are sorted by name, ownership is 0/0 with empty names, modes are
// normalized to 0755 (executable) or 0644, modification times are the single
// time supplied by the caller (truncated to whole seconds), access and change
// times are absent, the gzip header carries no name, time or OS-specific
// byte, and zip members use the deflate method with the same fixed time in
// both the DOS field and the extended-timestamp extra field. The output is a
// function of (names, contents, modes, mtime, this package's Go toolchain).
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one archive member.
type Entry struct {
	// Name inside the archive: a relative slash-separated path with no "."
	// or ".." components.
	Name string
	// Path of the file on disk whose contents and executable bit are used.
	Path string
}

// Format selects the container.
type Format string

const (
	TarGz Format = "tar.gz"
	Zip   Format = "zip"
)

// FormatFor infers the format from a file name.
func FormatFor(name string) (Format, error) {
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return TarGz, nil
	case strings.HasSuffix(name, ".zip"):
		return Zip, nil
	}
	return "", fmt.Errorf("archive: cannot infer format from %q (want .tar.gz, .tgz or .zip)", name)
}

// Write writes the entries to w in the given format with all timestamps set to mtime.
func Write(w io.Writer, format Format, entries []Entry, mtime time.Time) error {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i, e := range sorted {
		if err := validName(e.Name); err != nil {
			return err
		}
		if i > 0 && sorted[i-1].Name == e.Name {
			return fmt.Errorf("archive: duplicate member %q", e.Name)
		}
	}
	mtime = mtime.UTC().Truncate(time.Second)
	switch format {
	case TarGz:
		return writeTarGz(w, sorted, mtime)
	case Zip:
		return writeZip(w, sorted, mtime)
	}
	return fmt.Errorf("archive: unknown format %q", format)
}

func validName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("archive: invalid member name %q", name)
	}
	if path.Clean(name) != name {
		return fmt.Errorf("archive: member name %q is not clean", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == "" {
			return fmt.Errorf("archive: member name %q has an invalid component", name)
		}
	}
	return nil
}

// mode normalizes a file's permission bits: 0755 if any execute bit is set,
// 0644 otherwise, so the builder's umask cannot reach the archive.
func mode(info fs.FileInfo) int64 {
	if info.Mode()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func writeTarGz(w io.Writer, entries []Entry, mtime time.Time) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	// gzip.NewWriterLevel leaves Name and Comment empty, ModTime zero and
	// OS = 255 (unknown); nothing host-specific is written.
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := addTar(tw, e, mtime); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addTar(tw *tar.Writer, e Entry, mtime time.Time) error {
	f, err := os.Open(e.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive: %s is not a regular file", e.Path)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     e.Name,
		Mode:     mode(info),
		Size:     info.Size(),
		ModTime:  mtime,
		// Uid, Gid = 0; Uname, Gname = ""; AccessTime, ChangeTime = zero.
		// Format is left unknown: Go picks USTAR when the name fits and
		// otherwise PAX with only the records that are needed; both are
		// deterministic for a given header.
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("archive: %s changed size while packing", e.Path)
	}
	return nil
}

func writeZip(w io.Writer, entries []Entry, mtime time.Time) error {
	zw := zip.NewWriter(w)
	for _, e := range entries {
		if err := addZip(zw, e, mtime); err != nil {
			return err
		}
	}
	return zw.Close()
}

func addZip(zw *zip.Writer, e Entry, mtime time.Time) error {
	f, err := os.Open(e.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive: %s is not a regular file", e.Path)
	}
	hdr := &zip.FileHeader{
		Name:     e.Name,
		Method:   zip.Deflate,
		Modified: mtime,
	}
	hdr.SetMode(fs.FileMode(mode(info)))
	fw, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	return nil
}

// WriteFile writes the archive to path, inferring the format from the name,
// through a temporary file so that a failure never leaves a partial archive.
func WriteFile(dest string, entries []Entry, mtime time.Time) error {
	format, err := FormatFor(dest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".pack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	werr := Write(tmp, format, entries, mtime)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
