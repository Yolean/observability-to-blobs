package main

import "testing"

func TestTextFloat(t *testing.T) {
	for v, want := range map[float64]string{77094912: "77094912", 0.004333333333333348: "0.00433333333333", 1234567.891234567: "1234567.89123", 0.25: "0.25", 1e-9: "1e-09", 3e20: "3e+20", 0: "0", -1.5: "-1.5"} {
		if got := text(v); got != want {
			t.Errorf("%v: %q, want %q", v, got, want)
		}
	}
}
