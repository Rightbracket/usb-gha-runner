package supervisor

import (
	"strings"
	"testing"
)

func TestReadLinesSkipsOverlongLine(t *testing.T) {
	in := "a\n" + strings.Repeat("x", 3000) + "\nRunning job: build\nlast"
	var got []string
	readLines(strings.NewReader(in), 1024, func(l string) { got = append(got, l) })
	if len(got) != 4 || got[0] != "a" || !strings.HasSuffix(got[1], "[line too long, cut]") ||
		got[2] != "Running job: build" || got[3] != "last" {
		t.Fatalf("%q", got)
	}
	if len(got[1]) > 1024+len(" [line too long, cut]") {
		t.Fatalf("cut line is %d bytes", len(got[1]))
	}
}
