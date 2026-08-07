package consolidate

import "testing"

type item struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// Providers disagree about how to return a list, and the disagreement is not
// their fault: response_format json_object is what makes JSON reliable, and
// that mode forbids a bare array at the top level. Every shape below is a
// correct answer from some provider, and rejecting any of them means
// consolidation silently produces nothing while still spending tokens.
func TestDecodeListAcceptsEveryReasonableShape(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want int
	}{
		"bare array": {
			`[{"kind":"fact","title":"a"},{"kind":"decision","title":"b"}]`, 2},
		"wrapped in facts": {
			`{"facts":[{"kind":"fact","title":"a"}]}`, 1},
		"wrapped in lessons": {
			`{"lessons":[{"kind":"lesson","title":"a"},{"kind":"lesson","title":"b"}]}`, 2},
		"wrapped in an unconventional name": {
			`{"extracted_items":[{"kind":"fact","title":"a"}]}`, 1},
		"empty wrapped array": {
			`{"facts":[]}`, 0},
		"empty bare array": {
			`[]`, 0},
		"a single object rather than a list": {
			`{"kind":"fact","title":"only one"}`, 1},
		"fenced": {
			"```json\n[{\"kind\":\"fact\",\"title\":\"a\"}]\n```", 1},
		"prose around it": {
			"Here are the facts:\n[{\"kind\":\"fact\",\"title\":\"a\"}]\nHope that helps.", 1},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out []item
			if err := decodeList(tc.raw, &out); err != nil {
				t.Fatalf("decodeList: %v\ninput: %s", err, tc.raw)
			}
			if len(out) != tc.want {
				t.Fatalf("got %d items, want %d: %+v", len(out), tc.want, out)
			}
		})
	}
}

func TestDecodeListRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "not json at all", "{"} {
		var out []item
		if err := decodeList(raw, &out); err == nil {
			t.Errorf("decodeList(%q) should have failed", raw)
		}
	}
}

// An object with no array in it is a real failure and must not silently
// become an empty result — that would look identical to "nothing to extract".
func TestDecodeListRejectsObjectWithNoList(t *testing.T) {
	var out []item
	if err := decodeList(`{"status":"ok","count":0}`, &out); err == nil {
		t.Fatalf("expected an error, got %d items", len(out))
	}
}
