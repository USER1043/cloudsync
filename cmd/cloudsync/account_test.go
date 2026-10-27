package main

import "testing"

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                       "0 B",
		1023:                    "1023 B",
		1024:                    "1.0 KB",
		1536:                    "1.5 KB",
		15 * 1024 * 1024 * 1024: "15.0 GB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
