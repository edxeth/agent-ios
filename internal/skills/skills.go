// Package skills serves the agent skills that are embedded in the binary
// (design docs/design/agentic-parity.md, sections 4.2 "skills get|list|path"
// and 7.9). The source files are skills/<name>/SKILL.md in the repository
// (package github.com/nnemirovsky/iwdp-cli/skills).
package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	files "github.com/nnemirovsky/iwdp-cli/skills"
)

// FileName is the file of a skill inside its directory.
const FileName = "SKILL.md"

// versionHexDigits is the length of a skills version (version), as for the
// runtime bundle version (design 3.1).
const versionHexDigits = 12

// Skill is one embedded skill.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content,omitempty"` // the whole SKILL.md; empty in List
}

// source is the file system that holds <name>/SKILL.md; tests replace it.
var source fs.FS = files.FS

// List returns every embedded skill sorted by name, without its content.
func List() ([]Skill, error) {
	all, err := load()
	if err != nil {
		return nil, err
	}
	for i := range all {
		all[i].Content = ""
	}
	return all, nil
}

// Get returns one skill with its content. An unknown name is an error that
// lists the known names.
func Get(name string) (Skill, error) {
	all, err := load()
	if err != nil {
		return Skill{}, err
	}
	names := make([]string, 0, len(all))
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
		names = append(names, s.Name)
	}
	return Skill{}, fmt.Errorf("unknown skill %q; the skills are %s (iwdp-cli skills list)", name, strings.Join(names, ", "))
}

// All returns every skill with its content, sorted by name.
func All() ([]Skill, error) { return load() }

// version is the first 12 hex digits of the SHA-256 of the names and
// contents of the skills all. It changes with every change of a skill, so
// Install never mixes the files of two versions.
func version(all []Skill) string {
	h := sha256.New()
	for _, s := range all {
		// The lengths keep "ab"+"c" apart from "a"+"bc".
		_, _ = fmt.Fprintf(h, "%d:%s\n%d:%s\n", len(s.Name), s.Name, len(s.Content), s.Content) // a hash never fails a write
	}
	return hex.EncodeToString(h.Sum(nil))[:versionHexDigits]
}

// Install writes every skill to root/<version>/<name>/SKILL.md (directories
// mode 0700, files mode 0600) and returns root/<version>. A file that
// already has the right content is not written again; a file with other
// content is replaced through a temporary file and a rename, so a reader
// never sees a partial skill.
func Install(root string) (string, error) {
	all, err := load()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, version(all))
	for _, s := range all {
		if err := writeFile(filepath.Join(dir, s.Name), []byte(s.Content)); err != nil {
			return "", fmt.Errorf("installing skill %s: %w", s.Name, err)
		}
	}
	return dir, nil
}

// tempFile is the part of *os.File that writeFile uses.
type tempFile interface {
	io.Writer
	Close() error
	Name() string
}

// createTemp makes the temporary file of writeFile; tests replace it to
// make the create, write and close steps fail. After an error its file
// value is not used.
var createTemp = func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }

func writeFile(dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, FileName)
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp, err := createTemp(dir, ".skill-*")
	if err != nil {
		return err
	}
	// After a successful rename the temporary name is gone, so this
	// removal only cleans up after a failure; its error adds nothing.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file with mode 0600 already; the skill is not
	// secret, but the cache directory belongs to the user alone.
	return os.Rename(tmp.Name(), dst)
}

// load reads and checks every <name>/SKILL.md of source.
func load() ([]Skill, error) {
	paths, err := fs.Glob(source, "*/"+FileName)
	if err != nil {
		return nil, fmt.Errorf("listing the embedded skills: %w", err)
	}
	if len(paths) == 0 {
		return nil, errors.New("no skills are embedded in this binary")
	}
	sort.Strings(paths)
	out := make([]Skill, 0, len(paths))
	for _, p := range paths {
		data, err := fs.ReadFile(source, p)
		if err != nil {
			return nil, fmt.Errorf("reading the embedded skill %s: %w", p, err)
		}
		s, err := parse(path.Dir(p), string(data))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// parse reads the front matter of a SKILL.md: a first line "---", lines
// "key: value", and a closing "---". name must equal the directory name,
// and description must not be empty.
func parse(dir, content string) (Skill, error) {
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return Skill{}, fmt.Errorf("skill %s: %s does not start with a front matter line ---", dir, FileName)
	}
	front, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Skill{}, fmt.Errorf("skill %s: the front matter of %s has no closing line ---", dir, FileName)
	}
	s := Skill{Content: content}
	for _, line := range strings.Split(front, "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(k) {
		case "name":
			s.Name = strings.TrimSpace(v)
		case "description":
			s.Description = strings.TrimSpace(v)
		}
	}
	if s.Name != dir {
		return Skill{}, fmt.Errorf("skill %s: front matter name %q differs from its directory", dir, s.Name)
	}
	if s.Description == "" {
		return Skill{}, fmt.Errorf("skill %s: front matter has no description", dir)
	}
	return s, nil
}
