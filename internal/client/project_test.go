package client

import "testing"

// T2.2 acceptance: the same repository cloned to two different paths, on two
// different operating systems, must yield one project ID.
func TestNormaliseRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:devkuong/launcher.git":          "github.com/devkuong/launcher",
		"git@github.com:devkuong/launcher":              "github.com/devkuong/launcher",
		"https://github.com/devkuong/launcher.git":      "github.com/devkuong/launcher",
		"https://github.com/devkuong/launcher":          "github.com/devkuong/launcher",
		"ssh://git@github.com/devkuong/launcher.git":    "github.com/devkuong/launcher",
		"ssh://git@github.com:22/devkuong/launcher.git": "github.com/devkuong/launcher",
		"https://user:token@github.com/org/repo.git":    "github.com/org/repo",
		"git://github.com/org/repo.git":                 "github.com/org/repo",
		"https://GitHub.com/Org/Repo.git":               "github.com/Org/Repo",
		"https://gitlab.example.com/group/sub/repo.git": "gitlab.example.com/group/sub/repo",
		"git@bitbucket.org:team/repo.git":               "bitbucket.org/team/repo",
		"https://github.com/org/repo/":                  "github.com/org/repo",

		// Not resolvable to a project.
		"":                   "",
		"github.com":         "",
		"https://github.com": "",
	}

	for in, want := range cases {
		if got := NormaliseRemote(in); got != want {
			t.Errorf("NormaliseRemote(%q)\n  got  %q\n  want %q", in, got, want)
		}
	}
}

func TestSSHAndHTTPSAgree(t *testing.T) {
	// The property that makes team sharing work: one person cloning over SSH
	// and another over HTTPS must land on the same project.
	ssh := NormaliseRemote("git@github.com:IzE-PewPewPew/DK-AgentMemory.git")
	https := NormaliseRemote("https://github.com/IzE-PewPewPew/DK-AgentMemory.git")
	if ssh != https {
		t.Fatalf("SSH and HTTPS remotes disagree:\n  ssh:   %q\n  https: %q", ssh, https)
	}
	if ssh != "github.com/IzE-PewPewPew/DK-AgentMemory" {
		t.Fatalf("unexpected identity %q", ssh)
	}
}

func TestCaseHandling(t *testing.T) {
	// Host is case-insensitive in DNS and gets lowercased; the path is not, and
	// lowercasing it would make the identity disagree with the URL people read.
	if got := NormaliseRemote("git@GITHUB.COM:MyOrg/MyRepo.git"); got != "github.com/MyOrg/MyRepo" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveProjectFallsBackToDirectoryName(t *testing.T) {
	dir := t.TempDir()
	p := ResolveProject(dir)
	if p.ID == "" {
		t.Fatal("expected a project ID even outside a repository")
	}
	if p.Source != SourceCWD && p.Source != SourceFolder {
		t.Fatalf("unexpected source %q", p.Source)
	}
	if p.Warning == "" {
		// The warning is the feature here: a folder-derived identity will not
		// match a teammate, and silently proceeding is how people conclude that
		// sharing is broken.
		t.Error("a folder-derived identity must carry a warning")
	}
}
