package update

import "testing"

func TestDigestFromReference(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  string
	}{
		{"ghcr.io/llllogan/ccm@sha256:abc123", "sha256:abc123"},
		{"sha256:def456\n", "sha256:def456"},
		{"ghcr.io/llllogan/ccm:latest", ""},
	} {
		if got := digestFromReference(tt.input); got != tt.want {
			t.Errorf("digestFromReference(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
