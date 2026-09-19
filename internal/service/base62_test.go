package service

import (
	"math"
	"testing"
)

func TestEncodeBase62KnownValues(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "A"},
		{35, "Z"},
		{36, "a"},
		{61, "z"},
		{62, "10"},
		{63, "11"},
		{62 * 62, "100"},
		{62*62 + 3, "103"},
	}
	for _, c := range cases {
		if got := EncodeBase62(c.in); got != c.want {
			t.Errorf("EncodeBase62(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEncodeBase62MaxUint64FitsIn11Chars(t *testing.T) {
	got := EncodeBase62(math.MaxUint64)
	if len(got) > 11 {
		t.Fatalf("MaxUint64 encoded to %d chars (%q), expected <= 11", len(got), got)
	}
}

func TestEncodeBase62IsInjective(t *testing.T) {
	seen := map[string]uint64{}
	for n := uint64(0); n < 100_000; n++ {
		code := EncodeBase62(n)
		if prev, ok := seen[code]; ok {
			t.Fatalf("collision: EncodeBase62(%d) == EncodeBase62(%d) == %q", prev, n, code)
		}
		seen[code] = n
	}
	if len(seen) != 100_000 {
		t.Fatalf("expected 100_000 distinct codes, got %d", len(seen))
	}
}

func TestEncodeBase62UsesOnlyAlphabetChars(t *testing.T) {
	for n := uint64(0); n < 1_000_000; n++ {
		for _, ch := range EncodeBase62(n) {
			if !isBase62Char(byte(ch)) {
				t.Fatalf("code for %d contains non-base62 char %q", n, ch)
			}
		}
	}
}

func isBase62Char(b byte) bool {
	switch {
	case b >= '0' && b <= '9':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	}
	return false
}