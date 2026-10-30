package adminagent

import "strings"

// reasoningTags are the tags some models put their reasoning in when they write
// it into the answer instead of a separate field.
var reasoningTags = []struct{ open, closing string }{
	{"<think>", "</think>"},
	{"<thinking>", "</thinking>"},
}

// stripReasoning removes the reasoning a model wrote into its answer between
// think tags. It helps the model and confuses the reader. A block that is not
// closed yet hides everything after it, because the rest is still reasoning.
// While text streams, a half-typed opening tag at the end is hidden as well, so
// it does not flash on screen before the tag completes.
func stripReasoning(text string, streaming bool) string {
	if !strings.Contains(text, "<") {
		return text
	}
	for _, tag := range reasoningTags {
		text = removeTagged(text, tag.open, tag.closing)
	}
	if streaming {
		text = hidePartialTag(text)
	}
	return text
}

// removeTagged drops every block from an opening tag to its closing tag. An
// opening tag with no closing tag drops the rest of the text. A closing tag with
// no opening tag drops everything before it, for models whose template writes
// the opening tag into the prompt.
func removeTagged(text, open, closing string) string {
	if end := strings.Index(text, closing); end >= 0 {
		if start := strings.Index(text, open); start < 0 || end < start {
			text = text[end+len(closing):]
		}
	}
	var kept strings.Builder
	for {
		start := strings.Index(text, open)
		if start < 0 {
			kept.WriteString(text)
			return kept.String()
		}
		kept.WriteString(text[:start])
		rest := text[start+len(open):]
		end := strings.Index(rest, closing)
		if end < 0 {
			return kept.String()
		}
		text = rest[end+len(closing):]
	}
}

// hidePartialTag cuts an opening tag that has only been typed in part.
func hidePartialTag(text string) string {
	for _, tag := range reasoningTags {
		for size := len(tag.open) - 1; size >= 2; size-- {
			if strings.HasSuffix(text, tag.open[:size]) {
				return text[:len(text)-size]
			}
		}
	}
	return text
}
