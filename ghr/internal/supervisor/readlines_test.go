package supervisor

import (
	"reflect"
	"strings"
	"testing"
)

func TestReadLines(t *testing.T) {
	const cut = " [line too long, cut]"
	x := func(n int) string { return strings.Repeat("x", n) }
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"short", "a\nb", []string{"a", "b"}},
		{"just under", x(15) + "\n", []string{x(15)}},
		{"exactly max", x(16) + "\nok\n", []string{x(16), "ok"}},
		{"exactly max crlf", x(15) + "\r\nok\n", []string{x(15), "ok"}},
		{"exactly max at EOF", x(16), []string{x(16)}},
		{"long then marker", "a\n" + x(3000) + "\nmarker\nlast", []string{"a", x(16) + cut, "marker", "last"}},
		{"long at EOF", "a\n" + x(40), []string{"a", x(16) + cut}},
		{"long, newline right after a full buffer", x(32) + "\nok", []string{x(16) + cut, "ok"}},
	} {
		var got []string
		readLines(strings.NewReader(tc.in), 16, func(l string) { got = append(got, l) })
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
