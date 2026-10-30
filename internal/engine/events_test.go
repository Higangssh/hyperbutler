package engine

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// Review finding 4: a guest dockerd that streams one huge event must not make the host
// buffer it.
func TestReadEventLineBounded(t *testing.T) {
	r := io.MultiReader(strings.NewReader(`{"Type":"container"}`+"\n"), strings.NewReader(strings.Repeat("x", 10<<20)))
	br := bufio.NewReaderSize(r, maxEventLine)
	line, err := readEventLine(br)
	if err != nil || !strings.HasPrefix(string(line), `{"Type"`) {
		t.Fatalf("first line: %q %v", line, err)
	}
	if _, err := readEventLine(br); !errors.Is(err, errEventTooLong) {
		t.Fatalf("oversized line: %v, want errEventTooLong", err)
	}
}
