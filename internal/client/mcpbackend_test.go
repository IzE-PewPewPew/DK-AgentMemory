package client

import "testing"

// A folder name is not a project identity, and treating it as one made every
// MCP tool return nothing.
//
// Host applications launch the MCP process from wherever they happen to be --
// Claude Desktop uses the user's home directory. Scoping unqualified lookups to
// the resulting folder name meant every search ran against a project that had
// never existed, and returned zero results while reporting success, which is
// indistinguishable from an empty corpus. Verified against the live server: the
// same binary answering from the repository returned three lessons, and from
// the home directory returned none.
func TestOnlyStableProjectIdentitiesScopeMCPLookups(t *testing.T) {
	// Each of these names the same project from any directory on any machine.
	for _, s := range []Source{SourceRemote, SourceConfig, SourceFile, SourceEnv} {
		if !trustedProjectSource(s) {
			t.Errorf("%q should scope lookups: it names one project across machines", s)
		}
	}

	// These describe where a process happens to be running, which says nothing
	// about what the user is working on. An unknown source is untrusted too, so
	// a new one added later fails open to "everything" rather than silently
	// scoping every tool to something meaningless.
	for _, s := range []Source{SourceFolder, SourceCWD, Source("added later")} {
		if trustedProjectSource(s) {
			t.Errorf("%q must not scope lookups: too narrow is indistinguishable "+
				"from an empty corpus, while too broad is visible and correctable", s)
		}
	}
}
