package connectors

import "testing"

func TestResultOutputPrefersBareThenSinglePart(t *testing.T) {
	cases := []struct {
		name    string
		outputs map[string]string
		want    string
	}{
		{"bare key", map[string]string{"sha256": "aa"}, "aa"},
		{"part zero", map[string]string{"part.0.sha256": "bb", "part.0.etag": "e"}, "bb"},
		{"single other part", map[string]string{"part.3.sha256": "cc"}, "cc"},
		{"bare wins over part", map[string]string{"sha256": "aa", "part.0.sha256": "bb"}, "aa"},
		{"ambiguous multi-part", map[string]string{"part.0.sha256": "bb", "part.1.sha256": "cc"}, ""},
		{"missing", map[string]string{"part.0.etag": "e"}, ""},
		{"nil map", nil, ""},
	}
	for _, tc := range cases {
		if got := resultOutput(tc.outputs, "sha256"); got != tc.want {
			t.Errorf("%s: resultOutput = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := resultOutput(map[string]string{"part.0.etag": "\"x\""}, "etag"); got != "\"x\"" {
		t.Errorf("etag: got %q", got)
	}
}
