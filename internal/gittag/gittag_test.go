package gittag

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Quince-Pie/release-me/internal/sshsig"
)

func TestParseObject(t *testing.T) {
	raw := "object 0123456789abcdef0123456789abcdef01234567\ntype commit\ntag v1.0.0\ntagger A B <a@b.c> 1700000000 +0200\n\nRelease 1.0.0\n\nnotes\n-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"
	tag, err := ParseObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if tag.Name != "v1.0.0" || tag.ObjectType != "commit" || tag.Tagger != "A B <a@b.c>" || tag.TaggerTime.Unix() != 1700000000 {
		t.Errorf("headers: %+v", tag)
	}
	if tag.Message != "Release 1.0.0\n\nnotes\n" || !tag.Signed() {
		t.Errorf("message %q signed %v", tag.Message, tag.Signed())
	}
	if string(tag.payload) != raw[:strings.Index(raw, "-----BEGIN")] {
		t.Errorf("payload = %q", tag.payload)
	}
	unsigned, _ := ParseObject([]byte("object x\ntype commit\ntag v1\ntagger a <a> 1 +0000\n\nmsg\n"))
	if unsigned.Signed() || unsigned.Message != "msg\n" {
		t.Errorf("unsigned: %+v", unsigned)
	}
	if _, err := ParseObject([]byte("garbage")); err == nil {
		t.Error("garbage accepted")
	}
}

// End to end with real git and ssh-keygen: git signs the tag, we verify it.
func TestRealGitTag(t *testing.T) {
	for _, bin := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x.y", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x.y", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	key := filepath.Join(dir, "key")
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key+"2")
	run("git", "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	run("git", "add", "f")
	run("git", "commit", "-q", "-m", "one")
	run("git", "-c", "gpg.format=ssh", "-c", "user.signingkey="+key, "tag", "-s", "v1.0.0", "-m", "Release 1.0.0")
	run("git", "tag", "-a", "v1.0.1", "-m", "unsigned annotated")
	run("git", "tag", "v1.0.2")
	run("git", "-c", "gpg.format=ssh", "-c", "user.signingkey="+key+"2", "tag", "-s", "v1.0.3", "-m", "wrong key")
	run("git", "checkout", "-q", "-b", "side")
	os.WriteFile(filepath.Join(dir, "g"), []byte("y"), 0o644)
	run("git", "add", "g")
	run("git", "commit", "-q", "-m", "side")
	run("git", "-c", "gpg.format=ssh", "-c", "user.signingkey="+key, "tag", "-s", "v1.1.0", "-m", "not on main")
	run("git", "checkout", "-q", "main")

	pub, _ := os.ReadFile(key + ".pub")
	policy, err := sshsig.ParseAllowedSigners(strings.NewReader("rel@x.y namespaces=\"git\" " + string(pub)))
	if err != nil {
		t.Fatal(err)
	}
	repo := Repo{Dir: dir}

	tag, err := repo.Read(ctx, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	res, err := tag.VerifySignature(policy, sshsig.Options{Principal: "rel@x.y"})
	if err != nil {
		t.Fatalf("signed tag rejected: %v", err)
	}
	if res.Principal != "rel@x.y" || tag.ObjectType != "commit" || tag.Message != "Release 1.0.0\n" {
		t.Errorf("result %+v tag %+v", res, tag)
	}
	commit, _ := repo.Commit(ctx, "v1.0.0")
	if ok, _ := repo.IsAncestor(ctx, commit, "main"); !ok || commit != tag.Object {
		t.Errorf("reachability: %v %s %s", ok, commit, tag.Object)
	}

	if tag, err := repo.Read(ctx, "v1.0.1"); err != nil {
		t.Fatal(err)
	} else if _, err := tag.VerifySignature(policy, sshsig.Options{}); err == nil {
		t.Error("unsigned annotated tag accepted")
	}
	if _, err := repo.Read(ctx, "v1.0.2"); err == nil {
		t.Error("lightweight tag accepted")
	}
	if tag, _ := repo.Read(ctx, "v1.0.3"); tag != nil {
		if _, err := tag.VerifySignature(policy, sshsig.Options{}); err == nil {
			t.Error("tag signed by an unlisted key accepted")
		}
	}
	side, _ := repo.Commit(ctx, "v1.1.0")
	if ok, _ := repo.IsAncestor(ctx, side, "main"); ok {
		t.Error("side-branch tag reported reachable from main")
	}
	// Tampering: a modified payload must not verify against the original signature.
	tampered := *tag
	tampered.payload = append([]byte{}, tag.payload...)
	tampered.payload[len(tampered.payload)-2] ^= 1
	if _, err := tampered.VerifySignature(policy, sshsig.Options{}); err == nil {
		t.Error("tampered payload accepted")
	}
}
