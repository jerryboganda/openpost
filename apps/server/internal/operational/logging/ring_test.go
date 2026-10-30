package logging

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecentLinesKeepsTheNewestRecordsInOrder(t *testing.T) {
	buffer := NewRecentLines(3)
	for index := 1; index <= 5; index++ {
		_, err := fmt.Fprintf(buffer, "{\"event\":\"line-%d\"}\n", index)
		require.NoError(t, err)
	}

	lines := buffer.Snapshot(10, nil)
	require.Equal(t, []string{`{"event":"line-3"}`, `{"event":"line-4"}`, `{"event":"line-5"}`}, lines)
	require.Equal(t, []string{`{"event":"line-5"}`}, buffer.Snapshot(1, nil), "limit keeps the newest record")
}

func TestRecentLinesFiltersBeforeApplyingTheLimit(t *testing.T) {
	buffer := NewRecentLines(10)
	_, err := buffer.Write([]byte("ok one\nerror alpha\nok two\nerror beta\nok three\n"))
	require.NoError(t, err)

	errorsOnly := func(line string) bool { return strings.HasPrefix(line, "error") }
	require.Equal(t, []string{"error alpha", "error beta"}, buffer.Snapshot(5, errorsOnly))
	require.Equal(t, []string{"error beta"}, buffer.Snapshot(1, errorsOnly))
	require.Empty(t, buffer.Snapshot(0, nil))
}

func TestRecentLinesBoundsOversizedRecords(t *testing.T) {
	buffer := NewRecentLines(2)
	_, err := buffer.Write([]byte(strings.Repeat("x", maxRecentLineBytes*2) + "\n"))
	require.NoError(t, err)
	lines := buffer.Snapshot(1, nil)
	require.Len(t, lines, 1)
	require.Len(t, lines[0], maxRecentLineBytes)
}
