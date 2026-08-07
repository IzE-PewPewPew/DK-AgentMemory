package store

import (
	"fmt"
	"testing"
)

// The traversal moved out of SQL because a recursive CTE could not both bound
// depth and visit each node once: UNION deduplicates whole rows, so carrying
// the hop count made one node several rows and it was re-expanded at every
// distance it could be reached. These tests are the reason that is now
// ordinary code.
func TestBFS(t *testing.T) {
	// a -- b -- c -- d, plus a -- e
	adj := map[string][]string{
		"a": {"b", "e"},
		"b": {"a", "c"},
		"c": {"b", "d"},
		"d": {"c"},
		"e": {"a"},
	}

	t.Run("depth bounds the walk", func(t *testing.T) {
		got := bfs(adj, "a", 1, 100)
		assertSet(t, got, "a", "b", "e")
	})

	t.Run("two hops reaches further", func(t *testing.T) {
		got := bfs(adj, "a", 2, 100)
		assertSet(t, got, "a", "b", "e", "c")
	})

	t.Run("enough depth reaches everything", func(t *testing.T) {
		got := bfs(adj, "a", 6, 100)
		assertSet(t, got, "a", "b", "c", "d", "e")
	})

	t.Run("the seed comes first", func(t *testing.T) {
		if got := bfs(adj, "c", 1, 100); got[0] != "c" {
			t.Errorf("first = %q, want the seed", got[0])
		}
	})

	t.Run("unknown seed is empty, not an error", func(t *testing.T) {
		if got := bfs(adj, "", 2, 100); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("isolated node returns only itself", func(t *testing.T) {
		if got := bfs(map[string][]string{}, "lonely", 3, 100); len(got) != 1 || got[0] != "lonely" {
			t.Errorf("got %v, want [lonely]", got)
		}
	})
}

// A cycle must not loop forever, and must not re-expand. This is the exact
// shape the SQL version got wrong: b is reachable from a at one hop and again
// at two via c.
func TestBFSVisitsEachNodeOnce(t *testing.T) {
	adj := map[string][]string{
		"a": {"b", "c"},
		"b": {"a", "c"},
		"c": {"a", "b"},
	}
	got := bfs(adj, "a", 6, 100)
	assertSet(t, got, "a", "b", "c")

	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times, want 1", id, n)
		}
	}
}

// A dense graph is the case that used to hang. Every node adjacent to every
// other: the walk must be linear in nodes, not exponential in paths.
func TestBFSHandlesADenseGraph(t *testing.T) {
	const n = 400
	adj := map[string][]string{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%03d", i)
		for j := 0; j < n; j++ {
			if i != j {
				adj[id] = append(adj[id], fmt.Sprintf("n%03d", j))
			}
		}
	}
	got := bfs(adj, "n000", 6, 10_000)
	if len(got) != n {
		t.Fatalf("got %d nodes, want %d", len(got), n)
	}
}

func TestBFSLimitStopsTheWalk(t *testing.T) {
	adj := map[string][]string{"a": {"b", "c", "d", "e", "f"}}
	got := bfs(adj, "a", 3, 3)
	if len(got) != 3 {
		t.Fatalf("got %d nodes, want exactly 3", len(got))
	}
	if got[0] != "a" {
		t.Errorf("first = %q, want the seed to survive truncation", got[0])
	}
}

// The limit must bite at the far edge of the neighbourhood, never the near
// one: a seeded view that omitted the seed's own neighbours while showing
// distant nodes would be actively misleading.
func TestBFSKeepsNearNeighboursWhenTruncating(t *testing.T) {
	adj := map[string][]string{
		"a":  {"b1", "b2"},
		"b1": {"a", "c1"},
		"b2": {"a", "c2"},
		"c1": {"b1"},
		"c2": {"b2"},
	}
	got := bfs(adj, "a", 3, 3)
	assertSet(t, got, "a", "b1", "b2")
}

func assertSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing %q; got %v", w, got)
		}
	}
}
