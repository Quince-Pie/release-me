package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Quince-Pie/release-me/internal/archive"
	"github.com/Quince-Pie/release-me/internal/changelog"
	"github.com/Quince-Pie/release-me/internal/manifest"
	"github.com/Quince-Pie/release-me/internal/semver"
)

func parseTime(s string) (time.Time, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Parse(time.RFC3339, s)
}

func cmdPack(_ context.Context, args []string) error {
	fs := flagSet("pack", "pack --out FILE.tar.gz|FILE.zip --mtime EPOCH|RFC3339 [NAME=]PATH...")
	out := fs.String("out", "", "archive to write; the extension selects tar.gz or zip")
	mtime := fs.String("mtime", envOr("SOURCE_DATE_EPOCH"), "modification time for every member (default $SOURCE_DATE_EPOCH)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() == 0 || *mtime == "" {
		fs.Usage()
		return errors.New("pack: --out, --mtime and at least one file are required")
	}
	t, err := parseTime(*mtime)
	if err != nil {
		return fmt.Errorf("pack: --mtime: %v", err)
	}
	var entries []archive.Entry
	for _, a := range fs.Args() {
		name, path, ok := strings.Cut(a, "=")
		if !ok {
			path, name = a, filepath.Base(a)
		}
		entries = append(entries, archive.Entry{Name: name, Path: path})
	}
	if err := archive.WriteFile(*out, entries, t); err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	return nil
}

func cmdManifest(_ context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("manifest: expected create DIR | check DIR | diff A B")
	}
	switch args[0] {
	case "create":
		fs := flagSet("manifest create", "manifest create [--out NAME] DIR")
		out := fs.String("out", manifest.FileName, "manifest file name, written inside DIR")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("manifest create: one directory expected")
		}
		m, err := manifest.FromDir(fs.Arg(0))
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(fs.Arg(0), *out), m.Bytes(), 0o644)
	case "check":
		fs := flagSet("manifest check", "manifest check [--name NAME] DIR")
		name := fs.String("name", manifest.FileName, "manifest file name inside DIR")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("manifest check: one directory expected")
		}
		m, err := loadManifest(filepath.Join(fs.Arg(0), *name))
		if err != nil {
			return err
		}
		if err := m.Check(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("%d files match %s\n", len(m.Entries), *name)
		return nil
	case "diff":
		if len(args) != 3 {
			return errors.New("manifest diff: two manifest files expected")
		}
		a, err := loadManifest(args[1])
		if err != nil {
			return err
		}
		b, err := loadManifest(args[2])
		if err != nil {
			return err
		}
		if err := manifest.Diff(a, b); err != nil {
			return fmt.Errorf("manifests differ:\n%v", err)
		}
		fmt.Printf("identical: %d entries\n", len(a.Entries))
		return nil
	}
	return fmt.Errorf("manifest: unknown subcommand %q", args[0])
}

func loadManifest(path string) (*manifest.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return manifest.Parse(f)
}

func cmdChangelog(_ context.Context, args []string) error {
	fs := flagSet("changelog", "changelog [--file CHANGELOG.md] lint | latest | section VERSION | release VERSION [--date YYYY-MM-DD] [--compare-url URL]")
	file := fs.String("file", "CHANGELOG.md", "changelog file")
	date := fs.String("date", time.Now().UTC().Format("2006-01-02"), "release date for 'release'")
	compare := fs.String("compare-url", "", "compare URL template for 'release', e.g. https://github.com/o/r/compare/{from}...{to}")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("changelog: subcommand required")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	c, err := changelog.Parse(string(data))
	if err != nil {
		return err
	}
	switch fs.Arg(0) {
	case "lint":
		if err := c.Lint(); err != nil {
			return fmt.Errorf("%s:\n%v", *file, err)
		}
		fmt.Printf("%s: ok\n", *file)
		return nil
	case "latest":
		r, ok := c.Latest()
		if !ok {
			return errors.New("no released version")
		}
		fmt.Println(r.Version)
		return nil
	case "section":
		if fs.NArg() != 2 {
			return errors.New("changelog section: VERSION expected")
		}
		v, err := semver.Parse(strings.TrimPrefix(fs.Arg(1), "v"))
		if err != nil {
			return err
		}
		body, ok := c.Section(v)
		if !ok {
			return fmt.Errorf("no section for %s", v)
		}
		fmt.Println(body)
		return nil
	case "release":
		if fs.NArg() != 2 {
			return errors.New("changelog release: VERSION expected")
		}
		v, err := semver.Parse(strings.TrimPrefix(fs.Arg(1), "v"))
		if err != nil {
			return err
		}
		d, err := time.Parse("2006-01-02", *date)
		if err != nil {
			return err
		}
		if *compare == "" {
			return errors.New("changelog release: --compare-url is required")
		}
		if err := c.Lint(); err != nil {
			return fmt.Errorf("%s fails lint before release:\n%v", *file, err)
		}
		out, err := c.Release(v, d, func(from, to string) string {
			if from == "" {
				return strings.NewReplacer("compare/{from}...{to}", "releases/tag/"+to, "{from}", "", "{to}", to).Replace(*compare)
			}
			return strings.NewReplacer("{from}", from, "{to}", to).Replace(*compare)
		})
		if err != nil {
			return err
		}
		if _, err := changelog.Parse(out); err != nil {
			return fmt.Errorf("internal error: released changelog does not parse: %v", err)
		}
		return os.WriteFile(*file, []byte(out), 0o644)
	}
	return fmt.Errorf("changelog: unknown subcommand %q", fs.Arg(0))
}
