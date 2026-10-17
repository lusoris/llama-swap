package mcptools

import (
	"strings"
	"testing"
)

func TestHelpers_CapText(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		max      int
		want     string
		wantCapd bool
	}{
		{"under the cap", "abc\ndef", 100, "abc\ndef", false},
		{"exactly the cap", "abc", 3, "abc", false},
		{"cuts at a newline", "abcd\nefgh\nijkl", 12, "abcd\nefgh", true},
		{"no newline to cut at", "abcdefghij", 4, "abcd", true},
		{"zero max disables", "abcdefghij", 0, "abcdefghij", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, capped := CapText(tt.in, tt.max)
			if got != tt.want || capped != tt.wantCapd {
				t.Errorf("CapText(%q, %d) = (%q, %v), want (%q, %v)", tt.in, tt.max, got, capped, tt.want, tt.wantCapd)
			}
		})
	}
}

func TestHelpers_CapResult(t *testing.T) {
	t.Run("positive over max", func(t *testing.T) {
		in := "line1\nline2\nline3"
		res := CapResult(in, 13)
		if !res.Truncated {
			t.Errorf("Truncated = false, want true")
		}
		if !strings.HasSuffix(res.Content, "\n[truncated]") {
			t.Errorf("Content does not end with \\n[truncated]: %q", res.Content)
		}
		want := "line1\nline2\n[truncated]"
		if res.Content != want {
			t.Errorf("Content = %q, want %q", res.Content, want)
		}
	})

	t.Run("negative under max", func(t *testing.T) {
		in := "line1\nline2"
		res := CapResult(in, 50)
		if res.Truncated {
			t.Errorf("Truncated = true, want false")
		}
		if res.Content != in {
			t.Errorf("Content = %q, want %q", res.Content, in)
		}
	})

	t.Run("boundary len equals max", func(t *testing.T) {
		in := "abc"
		res := CapResult(in, 3)
		if res.Truncated {
			t.Errorf("Truncated = true, want false")
		}
		if res.Content != in {
			t.Errorf("Content = %q, want %q", res.Content, in)
		}
	})

	t.Run("boundary max zero or negative", func(t *testing.T) {
		in := "abcdef"
		for _, max := range []int{0, -1} {
			res := CapResult(in, max)
			if res.Truncated {
				t.Errorf("CapResult(%q, %d).Truncated = true, want false", in, max)
			}
			if res.Content != in {
				t.Errorf("CapResult(%q, %d).Content = %q, want %q", in, max, res.Content, in)
			}
		}
	})
}
