package client

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Project identity is the most consequential design decision in the system.
//
// It decides whether one person's memories ever reach a teammate. Identify a
// project by folder path and they never do: ~/dev/api and D:\work\api are
// different strings, so two people on the same repository build two disjoint
// stores and each concludes the sharing feature does not work.
//
// The git remote is the one identifier that is already identical on every
// machine, in every checkout, on every OS. Normalising it is therefore the
// whole feature.

// Source records how a project ID was determined, so `dkm doctor` can say why
// two machines disagree.
type Source string

const (
	SourceFile   Source = "explicit .dkm/project file"
	SourceConfig Source = "explicit in client config"
	SourceRemote Source = "git remote"
	SourceFolder Source = "git root folder name"
	SourceCWD    Source = "current directory name"
	SourceEnv    Source = "DKM_PROJECT"
)

// Project is a resolved project identity.
type Project struct {
	ID     string
	Source Source
	// Warning is set when the identity will not match a teammate's checkout.
	Warning string
	// Root is the repository root, when there is one.
	Root string
}

// ResolveProject determines the project identity for a directory.
//
// Order, most explicit first:
//  1. .dkm/project in the repo root — committable, so the whole team agrees
//  2. the normalised git remote `origin`
//  3. the git root folder name, with a warning
//  4. the current directory name, with a louder warning
func ResolveProject(dir string) Project {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		} else {
			dir = "."
		}
	}

	root := gitRoot(dir)

	// 1. An explicit file wins over everything. Monorepos need it, and so does
	// any repo whose remote has moved.
	if root != "" {
		if id := readProjectFile(filepath.Join(root, ".dkm", "project")); id != "" {
			return Project{ID: id, Source: SourceFile, Root: root}
		}
	}
	if id := readProjectFile(filepath.Join(dir, ".dkm", "project")); id != "" {
		return Project{ID: id, Source: SourceFile, Root: root}
	}

	// 2. The git remote: the only identifier that is the same everywhere.
	if root != "" {
		if remote := gitRemote(root); remote != "" {
			if id := NormaliseRemote(remote); id != "" {
				return Project{ID: id, Source: SourceRemote, Root: root}
			}
		}
	}

	// 3. A git repo with no usable remote.
	if root != "" {
		return Project{
			ID:     filepath.Base(root),
			Source: SourceFolder,
			Root:   root,
			Warning: "this project is identified by folder name because the repository has no `origin` remote. " +
				"It will not match a teammate whose folder is named differently. " +
				"Fix it by adding a remote, or by committing a .dkm/project file.",
		}
	}

	// 4. Not a repository at all.
	return Project{
		ID:     filepath.Base(dir),
		Source: SourceCWD,
		Warning: "this directory is not a git repository, so the project is identified by directory name alone. " +
			"Memories saved here will not reach anyone else, and will not follow you to another machine. " +
			"Create a .dkm/project file to pin an identity.",
	}
}

func readProjectFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// NormaliseRemote converts any git remote URL to a stable project identity.
//
//	git@github.com:devkuong/launcher.git      -> github.com/devkuong/launcher
//	https://github.com/devkuong/launcher.git  -> github.com/devkuong/launcher
//	ssh://git@github.com:22/devkuong/launcher -> github.com/devkuong/launcher
//
// The host is lowercased because DNS is case-insensitive and people type it
// both ways. The path is not: GitHub treats org and repo names
// case-insensitively for lookup but preserves case, and lowercasing them would
// make the identity disagree with every URL anyone reads.
func NormaliseRemote(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" {
		return ""
	}

	// scp-like syntax: user@host:path
	if !strings.Contains(s, "://") {
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		if colon := strings.Index(s, ":"); colon >= 0 {
			s = s[:colon] + "/" + s[colon+1:]
		}
	} else {
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		if at := strings.Index(s, "@"); at >= 0 && at < strings.Index(s+"/", "/") {
			s = s[at+1:]
		}
	}

	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	host, path, ok := strings.Cut(s, "/")
	if !ok || path == "" {
		return ""
	}
	// Strip an explicit port: github.com:22/org/repo is the same project as
	// github.com/org/repo.
	if h, port, found := strings.Cut(host, ":"); found && isAllDigits(port) {
		host = h
	}
	host = strings.ToLower(host)

	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	return host + "/" + path
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func gitRoot(dir string) string {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		// Fall back to walking upward. `git` may be absent, and detection that
		// depends on a binary being installed is detection that silently stops
		// working on a minimal container.
		return findGitDirUpward(dir)
	}
	return filepath.Clean(out)
}

func findGitDirUpward(dir string) string {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if fi, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			_ = fi
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func gitRemote(root string) string {
	if out, err := runGit(root, "remote", "get-url", "origin"); err == nil && out != "" {
		return out
	}
	// No `origin`, but possibly exactly one other remote, which is
	// unambiguous enough to use.
	out, err := runGit(root, "remote")
	if err != nil {
		return ""
	}
	names := strings.Fields(out)
	if len(names) != 1 {
		return ""
	}
	url, err := runGit(root, "remote", "get-url", names[0])
	if err != nil {
		return ""
	}
	return url
}

// runGit executes git with a short deadline.
//
// Bounded because this runs on the session-start hook path. A git command that
// hangs -- a network remote helper, a credential prompt, a stale lock -- must
// not hang the user's editor.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Never prompt. Without this, a credential helper can block on a terminal
	// that is not attached to anything.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")

	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("git %s timed out", strings.Join(args, " "))
	}

	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
