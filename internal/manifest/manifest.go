// Package manifest reads and writes SHA256SUMS files in the GNU coreutils
// format ("<hex>  <name>\n") and compares them. The manifest is the binding
// between a release's assets and every signature or attestation: signatures
// cover the manifest, the manifest covers the bytes.
package manifest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one line of the manifest.
type Entry struct {
	Name   string // base name, no directory component
	SHA256 string // lowercase hex
}

// Manifest is an ordered list of entries. Entries are kept sorted by name in
// byte order so that the same set of files always renders identically.
type Manifest struct {
	Entries []Entry
}

// FileName is the conventional asset name.
const FileName = "SHA256SUMS"

// ValidName reports whether name is acceptable as an asset name: a single
// path component without separators, control characters or leading dots,
// so that it cannot escape a directory when a recipient writes it to disk and
// cannot be renamed by a host on upload.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." {
		return false
	}
	if name[0] == '.' || name[0] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '+', c == '~':
		default:
			return false
		}
	}
	return true
}

// Add inserts or replaces the entry for name.
func (m *Manifest) Add(name, sum string) error {
	if !ValidName(name) {
		return fmt.Errorf("manifest: invalid asset name %q", name)
	}
	if len(sum) != 64 {
		return fmt.Errorf("manifest: %s: digest must be 64 hex characters", name)
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return fmt.Errorf("manifest: %s: %v", name, err)
	}
	sum = strings.ToLower(sum)
	i := sort.Search(len(m.Entries), func(i int) bool { return m.Entries[i].Name >= name })
	if i < len(m.Entries) && m.Entries[i].Name == name {
		m.Entries[i].SHA256 = sum
		return nil
	}
	m.Entries = append(m.Entries, Entry{})
	copy(m.Entries[i+1:], m.Entries[i:])
	m.Entries[i] = Entry{Name: name, SHA256: sum}
	return nil
}

// Lookup returns the digest recorded for name.
func (m *Manifest) Lookup(name string) (string, bool) {
	i := sort.Search(len(m.Entries), func(i int) bool { return m.Entries[i].Name >= name })
	if i < len(m.Entries) && m.Entries[i].Name == name {
		return m.Entries[i].SHA256, true
	}
	return "", false
}

// Bytes renders the manifest.
func (m *Manifest) Bytes() []byte {
	var b bytes.Buffer
	for _, e := range m.Entries {
		b.WriteString(e.SHA256)
		b.WriteString("  ")
		b.WriteString(e.Name)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// Parse reads a manifest, rejecting anything that is not exactly the format
// this package writes (one entry per line, no duplicates, valid names). A
// recipient must not be led to write files outside the download directory by
// a hostile manifest, so names are validated on read as well as on write.
func Parse(r io.Reader) (*Manifest, error) {
	m := &Manifest{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if line == "" {
			return nil, fmt.Errorf("manifest: line %d: empty line", n)
		}
		i := strings.Index(line, "  ")
		if i != 64 || len(line) < 67 {
			return nil, fmt.Errorf("manifest: line %d: expected \"<sha256>  <name>\"", n)
		}
		sum, name := line[:64], line[66:]
		if _, ok := m.Lookup(name); ok {
			return nil, fmt.Errorf("manifest: line %d: duplicate entry %q", n, name)
		}
		if err := m.Add(name, sum); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if len(m.Entries) == 0 {
		return nil, errors.New("manifest: no entries")
	}
	return m, nil
}

// FromDir hashes every regular file in dir except the manifest itself.
func FromDir(dir string) (*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == FileName {
			continue
		}
		sum, err := HashFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if err := m.Add(e.Name(), sum); err != nil {
			return nil, err
		}
	}
	if len(m.Entries) == 0 {
		return nil, fmt.Errorf("manifest: no files in %s", dir)
	}
	return m, nil
}

// HashFile returns the lowercase hex SHA-256 of a file's contents.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return HashReader(f)
}

// HashReader hashes everything readable from r.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Check verifies that every entry's file in dir has the recorded digest and
// that no entry is missing. Extra files in dir are ignored.
func (m *Manifest) Check(dir string) error {
	var errs []error
	for _, e := range m.Entries {
		sum, err := HashFile(filepath.Join(dir, e.Name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if sum != e.SHA256 {
			errs = append(errs, fmt.Errorf("%s: sha256 %s, manifest says %s", e.Name, sum, e.SHA256))
		}
	}
	return errors.Join(errs...)
}

// Diff describes how b differs from a (entries missing, added or changed).
// It returns nil when the manifests are identical.
func Diff(a, b *Manifest) error {
	var errs []error
	for _, e := range a.Entries {
		if sum, ok := b.Lookup(e.Name); !ok {
			errs = append(errs, fmt.Errorf("%s: missing", e.Name))
		} else if sum != e.SHA256 {
			errs = append(errs, fmt.Errorf("%s: %s != %s", e.Name, e.SHA256, sum))
		}
	}
	for _, e := range b.Entries {
		if _, ok := a.Lookup(e.Name); !ok {
			errs = append(errs, fmt.Errorf("%s: unexpected", e.Name))
		}
	}
	return errors.Join(errs...)
}
