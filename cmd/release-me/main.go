// Command release-me publishes verifiable, reproducible releases to GitHub
// and Forgejo-family hosts (Forgejo, Codeberg, Gitea) and verifies them as
// a recipient. See docs/CONTRACT.md for what each command guarantees.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// version is set by the release build (-X main.version=...).
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "release-me:", err)
		os.Exit(1)
	}
}

type command struct {
	name, usage string
	run         func(ctx context.Context, args []string) error
}

var commands = []command{
	{"pack", "write a reproducible tar.gz or zip archive", cmdPack},
	{"manifest", "create, check or diff SHA256SUMS manifests", cmdManifest},
	{"changelog", "lint or query a Keep a Changelog file, or cut a release section", cmdChangelog},
	{"tag", "verify (or create) a signed release tag", cmdTag},
	{"provenance", "write an in-toto SLSA provenance statement for a manifest", cmdProvenance},
	{"sign", "sign a file with an SSH key (sshsig) or a statement keylessly (sigstore)", cmdSign},
	{"publish", "publish a directory of assets as a release, verified before it becomes visible", cmdPublish},
	{"verify", "verify a published release as a recipient", cmdVerify},
	{"trusted-root", "fetch the Sigstore public-good trusted root for offline verification", cmdTrustedRoot},
	{"version", "print the version", cmdVersion},
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		return flag.ErrHelp
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	usage()
	return fmt.Errorf("unknown command %q", args[0])
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: release-me <command> [flags]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-13s %s\n", c.name, c.usage)
	}
}

func cmdVersion(_ context.Context, _ []string) error {
	fmt.Println(version)
	return nil
}

// flagSet builds a flag set whose usage prints the given synopsis.
func flagSet(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet("release-me "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: release-me %s\n\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// envOr returns the first non-empty environment variable among names.
func envOr(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "release-me: "+format+"\n", args...)
}

func splitRepo(s string) (owner, repo string, err error) {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("repository must be owner/name, got %q", s)
	}
	return owner, repo, nil
}
