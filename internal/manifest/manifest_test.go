package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripAndOrder(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, FileName), []byte("ignored"), 0o644)
	m, err := FromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb  a.txt\n" +
		"3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d  b.txt\n"
	if got := string(m.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	p, err := Parse(bytes.NewReader(m.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if Diff(m, p) != nil || p.Check(dir) != nil {
		t.Fatal("round trip mismatch")
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	if p.Check(dir) == nil {
		t.Fatal("modified file passed Check")
	}
}

func TestParseRejectsHostileInput(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, in := range []string{
		"",
		sum + "  ../etc/passwd\n",
		sum + "  /abs\n",
		sum + "  sub/dir\n",
		sum + "  .hidden\n",
		sum + "  -flag\n",
		sum + " one-space\n",
		sum + "  a\n" + sum + "  a\n",
		"zz" + sum[2:] + "  a\n",
		sum + "  a\n\n",
		sum + "  name with space\n",
		sum + "  tab\tname\n",
	} {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestDiff(t *testing.T) {
	a, _ := Parse(strings.NewReader(strings.Repeat("a", 64) + "  x\n" + strings.Repeat("b", 64) + "  y\n"))
	b, _ := Parse(strings.NewReader(strings.Repeat("a", 64) + "  x\n" + strings.Repeat("c", 64) + "  z\n"))
	err := Diff(a, b)
	if err == nil || !strings.Contains(err.Error(), "y: missing") || !strings.Contains(err.Error(), "z: unexpected") {
		t.Fatalf("Diff = %v", err)
	}
}
