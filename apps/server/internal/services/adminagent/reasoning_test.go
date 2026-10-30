package adminagent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripReasoning(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		streaming bool
		want      string
	}{
		{"plain text is unchanged", "Production is healthy.", false, "Production is healthy."},
		{"a block is removed", "<think>check status</think>Done.", false, "Done."},
		{"several blocks are removed", "<think>a</think>One <think>b</think>two", false, "One two"},
		{"multi-line reasoning is removed", "<think>line one\nline two</think>\n\nAnswer", false, "\n\nAnswer"},
		{"the thinking tag is removed too", "<thinking>plan</thinking>Answer", false, "Answer"},
		{"an unclosed block hides the rest", "Answer first<think>still thinking", false, "Answer first"},
		{"a closing tag alone drops the reasoning before it", "planning out loud</think>Answer", false, "Answer"},
		{"comparison operators and tags of other kinds are kept", "if a < b and c > d then <ok>", false, "if a < b and c > d then <ok>"},
		{"a half-typed tag is hidden while streaming", "Answer <thi", true, "Answer "},
		{"a half-typed tag is kept in a finished answer", "Answer <thi", false, "Answer <thi"},
		{"a lone angle bracket is kept while streaming", "x <", true, "x <"},
		{"reasoning that is still open is hidden while streaming", "<think>pondering", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, stripReasoning(tc.input, tc.streaming))
		})
	}
}
