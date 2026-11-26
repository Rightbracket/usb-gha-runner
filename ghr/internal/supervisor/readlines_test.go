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

func TestClassify(t *testing.T) {
	const ts = "2026-11-26 18:00:00Z: "
	type got struct {
		kind int
		a, b string
	}
	for _, tc := range []struct {
		line string
		want got
	}{
		{ts + "Listening for Jobs", got{lineListening, "", ""}},
		{"Listening for Jobs", got{lineListening, "", ""}},
		{ts + "Running job: build", got{lineRunning, "build", ""}},
		{ts + "Job build completed with result: Succeeded", got{lineDone, "build", "Succeeded"}},
		// Job names that contain the other markers.
		{ts + "Running job: Listening for Jobs", got{lineRunning, "Listening for Jobs", ""}},
		{ts + "Job Listening for Jobs completed with result: Failed", got{lineDone, "Listening for Jobs", "Failed"}},
		{ts + "Running job: x Running job: y", got{lineRunning, "x Running job: y", ""}},
		{ts + "Job x Running job: y completed with result: Succeeded", got{lineDone, "x Running job: y", "Succeeded"}},
		{ts + "Running job: Job a completed with result: b", got{lineRunning, "Job a completed with result: b", ""}},
		{"some step printed Running job: fake", got{lineOther, "", ""}},
		{"echo Listening for Jobs", got{lineOther, "", ""}},
	} {
		k, a, b := classify(tc.line)
		if (got{k, a, b}) != tc.want {
			t.Errorf("%q: got %v, want %v", tc.line, got{k, a, b}, tc.want)
		}
	}
}
