package adminagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	maxTreeEntries     = 300
	maxFindResults     = 100
	defaultReadLines   = 400
	maxReadLines       = 800
	maxReadLineWidth   = 2000
	maxSearchResults   = 15
	maxWriteFileBytes  = githubMaxEditBytes
	rawContentMimeType = "application/vnd.github.raw+json"
)

func (s *Service) repoTools() []*toolSpec {
	g := s.github
	refProperty := stringSchema("Branch, tag, or commit. Defaults to the default branch.")
	branchProperty := stringSchema("Branch to change, for example agent/fix-login. It is created from the default branch when it does not exist yet. The default branch is never written directly.")
	messageProperty := stringSchema("Commit message. Optional.")
	return []*toolSpec{
		{
			name:        "repo_tree",
			description: "List one directory of the repository, or find files by name. Pass path to list a directory. Pass pattern to search every file path, for example *.svelte or agent/*.go.",
			parameters: objectSchema(nil, map[string]any{
				"path":    stringSchema("Directory to list. Empty means the repository root."),
				"pattern": stringSchema("Case-insensitive name or path pattern. * matches within one segment and ** across segments. A pattern without a slash matches file names."),
				"ref":     refProperty,
			}),
			describe: func(a toolArgs) string {
				if pattern := a.trimmed("pattern"); pattern != "" {
					return "Find files matching " + pattern
				}
				return "List " + displayPath(a.trimmed("path"))
			},
			run: func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.treeTool(ctx, a) },
		},
		{
			name:        "repo_read",
			description: "Read a text file from the repository with line numbers. Reads up to 400 lines by default and at most 800. Use start_line and end_line to page through long files.",
			parameters: objectSchema([]string{"path"}, map[string]any{
				"path":       stringSchema("File path, for example apps/server/internal/ai/ai.go."),
				"ref":        refProperty,
				"start_line": integerSchema("First line to show, starting at 1."),
				"end_line":   integerSchema("Last line to show."),
			}),
			describe: func(a toolArgs) string { return "Read " + a.trimmed("path") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.readTool(ctx, a) },
		},
		{
			name:        "repo_search",
			description: "Search code in the default branch with GitHub code search. It matches whole words and identifiers, not regular expressions. Use repo_read afterwards for the surrounding code.",
			parameters: objectSchema([]string{"query"}, map[string]any{
				"query": stringSchema("Words or an identifier to find, for example AdminAgentHandler."),
				"path":  stringSchema("Optional directory or file name filter, for example apps/web."),
			}),
			describe: func(a toolArgs) string { return "Search code for " + a.trimmed("query") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.searchTool(ctx, a) },
		},
		{
			name:        "repo_edit",
			description: "Replace exact text in one file on a branch and commit it. old_string must match the file exactly once, including whitespace, unless replace_all is true. Prefer this over repo_write for changes to existing files.",
			write:       true,
			parameters: objectSchema([]string{"branch", "path", "old_string", "new_string"}, map[string]any{
				"branch":      branchProperty,
				"path":        stringSchema("File path."),
				"old_string":  stringSchema("Exact text to replace. Include enough surrounding lines to make it unique."),
				"new_string":  stringSchema("Replacement text."),
				"replace_all": booleanSchema("Replace every occurrence instead of requiring exactly one."),
				"message":     messageProperty,
			}),
			describe: func(a toolArgs) string { return "Edit " + a.trimmed("path") + " on " + a.trimmed("branch") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.editTool(ctx, a) },
		},
		{
			name:        "repo_write",
			description: "Create a file or replace a whole file on a branch and commit it. Use repo_edit for small changes to existing files.",
			write:       true,
			parameters: objectSchema([]string{"branch", "path", "content"}, map[string]any{
				"branch":  branchProperty,
				"path":    stringSchema("File path."),
				"content": stringSchema("Complete new file content."),
				"message": messageProperty,
			}),
			describe: func(a toolArgs) string { return "Write " + a.trimmed("path") + " on " + a.trimmed("branch") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.writeTool(ctx, a) },
		},
		{
			name:        "repo_delete",
			description: "Delete one file on a branch and commit the deletion.",
			write:       true,
			parameters: objectSchema([]string{"branch", "path"}, map[string]any{
				"branch":  branchProperty,
				"path":    stringSchema("File path."),
				"message": messageProperty,
			}),
			describe: func(a toolArgs) string { return "Delete " + a.trimmed("path") + " on " + a.trimmed("branch") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.deleteTool(ctx, a) },
		},
	}
}

func displayPath(value string) string {
	if strings.Trim(value, "/") == "" {
		return "repository root"
	}
	return value
}

func (g *githubAPI) treeTool(ctx context.Context, args toolArgs) (string, error) {
	ref := g.refOrDefault(args.trimmed("ref"))
	if pattern := args.trimmed("pattern"); pattern != "" {
		return g.findFiles(ctx, ref, pattern)
	}
	endpoint := g.repoPath("/contents")
	directory := strings.Trim(strings.TrimSpace(args.text("path")), "/")
	if directory != "" {
		cleaned, err := cleanRepoPath(directory)
		if err != nil {
			return "", err
		}
		directory = cleaned
		endpoint += "/" + escapeRepoPath(directory)
	}
	reply, err := g.call(ctx, http.MethodGet, endpoint, url.Values{"ref": {ref}}, nil, "")
	if err != nil {
		return "", err
	}
	if reply.status == http.StatusNotFound {
		return "", fmt.Errorf("%s was not found on %s", displayPath(directory), ref)
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int    `json:"size"`
	}
	if err := json.Unmarshal(reply.body, &entries); err != nil {
		return "", fmt.Errorf("%s is a file; use repo_read", directory)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if (entries[i].Type == "dir") != (entries[j].Type == "dir") {
			return entries[i].Type == "dir"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s on %s (%d entries)\n", displayPath(directory), ref, len(entries))
	for index, entry := range entries {
		if index == maxTreeEntries {
			fmt.Fprintf(&builder, "…%d more entries\n", len(entries)-index)
			break
		}
		if entry.Type == "dir" {
			fmt.Fprintf(&builder, "%s/\n", entry.Name)
			continue
		}
		fmt.Fprintf(&builder, "%s (%d bytes)\n", entry.Name, entry.Size)
	}
	return builder.String(), nil
}

// pathMatcher turns a user pattern into a path predicate. Without wildcards it
// is a case-insensitive substring match; a pattern without a slash matches
// file names only.
func pathMatcher(pattern string) (func(string) bool, error) {
	lower := strings.ToLower(strings.TrimSpace(pattern))
	if !strings.ContainsAny(lower, "*?") {
		return func(candidate string) bool { return strings.Contains(strings.ToLower(candidate), lower) }, nil
	}
	var expression strings.Builder
	nameOnly := !strings.Contains(lower, "/")
	if nameOnly {
		expression.WriteString("^")
	} else {
		// A pattern with a directory part matches from any directory boundary, so
		// internal/**/*.go finds apps/server/internal/ai/chat.go.
		expression.WriteString("(?:^|/)")
	}
	for index := 0; index < len(lower); index++ {
		switch {
		case strings.HasPrefix(lower[index:], "**"):
			expression.WriteString(".*")
			index++
		case lower[index] == '*':
			expression.WriteString("[^/]*")
		case lower[index] == '?':
			expression.WriteString("[^/]")
		default:
			expression.WriteString(regexp.QuoteMeta(string(lower[index])))
		}
	}
	expression.WriteString("$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, errors.New("pattern is not valid")
	}
	return func(candidate string) bool {
		target := strings.ToLower(candidate)
		if nameOnly {
			target = target[strings.LastIndex(target, "/")+1:]
		}
		return compiled.MatchString(target)
	}, nil
}

func (g *githubAPI) findFiles(ctx context.Context, ref, pattern string) (string, error) {
	match, err := pathMatcher(pattern)
	if err != nil {
		return "", err
	}
	paths, truncated, err := g.treePaths(ctx, ref)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	found := 0
	for _, candidate := range paths {
		if !match(candidate) {
			continue
		}
		found++
		if found <= maxFindResults {
			builder.WriteString(candidate + "\n")
		}
	}
	if found == 0 {
		return fmt.Sprintf("No file paths match %q on %s.", pattern, ref), nil
	}
	header := fmt.Sprintf("%d file(s) match %q on %s\n", found, pattern, ref)
	if found > maxFindResults {
		header += fmt.Sprintf("Showing the first %d. Narrow the pattern to see the rest.\n", maxFindResults)
	}
	if truncated {
		header += "The repository listing was truncated by GitHub, so some files may be missing.\n"
	}
	return header + builder.String(), nil
}

func (g *githubAPI) readTool(ctx context.Context, args toolArgs) (string, error) {
	filePath, err := cleanRepoPath(args.text("path"))
	if err != nil {
		return "", err
	}
	ref := g.refOrDefault(args.trimmed("ref"))
	reply, err := g.call(ctx, http.MethodGet, g.repoPath("/contents/"+escapeRepoPath(filePath)),
		url.Values{"ref": {ref}}, nil, rawContentMimeType)
	if err != nil {
		return "", err
	}
	if reply.status == http.StatusNotFound {
		return "", fmt.Errorf("%s was not found on %s", filePath, ref)
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	if strings.HasPrefix(strings.ToLower(reply.header.Get("Content-Type")), "application/json") {
		return "", fmt.Errorf("%s is a directory; use repo_tree", filePath)
	}
	if bytes.IndexByte(reply.body, 0) >= 0 {
		return fmt.Sprintf("%s is a binary file (%d bytes) and cannot be shown as text.", filePath, len(reply.body)), nil
	}
	return formatNumberedLines(filePath, ref, string(reply.body), args.number("start_line", 1), args.number("end_line", 0), reply.truncated), nil
}

func formatNumberedLines(filePath, ref, content string, start, end int, truncated bool) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	total := len(lines)
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start + defaultReadLines - 1
	}
	if end > start+maxReadLines-1 {
		end = start + maxReadLines - 1
	}
	if end > total {
		end = total
	}
	if start > total {
		return fmt.Sprintf("%s has %d lines; start_line %d is past the end.", filePath, total, start)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s on %s, lines %d-%d of %d\n", filePath, ref, start, end, total)
	for number := start; number <= end; number++ {
		line := strings.TrimRight(lines[number-1], "\r")
		if len(line) > maxReadLineWidth {
			line = clipText(line, maxReadLineWidth)
		}
		fmt.Fprintf(&builder, "%d\t%s\n", number, line)
	}
	if end < total {
		fmt.Fprintf(&builder, "…%d more lines. Call again with start_line=%d.\n", total-end, end+1)
	}
	if truncated {
		builder.WriteString("The file is larger than the read limit and was cut off.\n")
	}
	return builder.String()
}

func (g *githubAPI) searchTool(ctx context.Context, args toolArgs) (string, error) {
	query := args.trimmed("query")
	if query == "" {
		return "", errors.New("query is required")
	}
	search := query + " repo:" + g.repository
	if scope := args.trimmed("path"); scope != "" {
		search += " path:" + scope
	}
	reply, err := g.call(ctx, http.MethodGet, "/search/code",
		url.Values{"q": {search}, "per_page": {fmt.Sprint(maxSearchResults)}}, nil, "application/vnd.github.text-match+json")
	if err != nil {
		return "", err
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	var result struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			Path        string `json:"path"`
			TextMatches []struct {
				Fragment string `json:"fragment"`
			} `json:"text_matches"`
		} `json:"items"`
	}
	if err := json.Unmarshal(reply.body, &result); err != nil {
		return "", errors.New("GitHub returned search results this tool could not read")
	}
	if len(result.Items) == 0 {
		return fmt.Sprintf("No matches for %q. Code search covers the default branch and matches whole words; try another word or repo_tree with a pattern.", query), nil
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d match(es) for %q (showing %d)\n", result.TotalCount, query, len(result.Items))
	for _, item := range result.Items {
		builder.WriteString("\n" + item.Path + "\n")
		for index, match := range item.TextMatches {
			if index == 2 {
				break
			}
			builder.WriteString("  " + clipText(strings.Join(strings.Fields(match.Fragment), " "), 240) + "\n")
		}
	}
	return builder.String(), nil
}

// matchLineEndings lets a model that always writes LF edit a CRLF file.
func matchLineEndings(current, oldText, newText string) (string, string) {
	if strings.Contains(current, "\r\n") && !strings.Contains(oldText, "\r\n") {
		return strings.ReplaceAll(oldText, "\n", "\r\n"), strings.ReplaceAll(newText, "\n", "\r\n")
	}
	return oldText, newText
}

// writeTarget validates the branch and path shared by every write tool.
func (g *githubAPI) writeTarget(args toolArgs) (string, string, error) {
	branch, err := g.writableBranch(args.text("branch"))
	if err != nil {
		return "", "", err
	}
	filePath, err := cleanRepoPath(args.text("path"))
	if err != nil {
		return "", "", err
	}
	if err := g.checkWritablePath(filePath); err != nil {
		return "", "", err
	}
	return branch, filePath, nil
}

func (g *githubAPI) editTool(ctx context.Context, args toolArgs) (string, error) {
	branch, filePath, err := g.writeTarget(args)
	if err != nil {
		return "", err
	}
	oldText, newText := args.text("old_string"), args.text("new_string")
	switch {
	case oldText == "":
		return "", errors.New("old_string is required; use repo_write to create or replace a whole file")
	case oldText == newText:
		return "", errors.New("old_string and new_string are identical")
	case containsCommittableSecret(newText):
		return "", errSecretInContent
	}
	if err := g.ensureBranch(ctx, branch); err != nil {
		return "", err
	}
	file, found, err := g.fileAt(ctx, filePath, branch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s does not exist on %s; use repo_write to create it", filePath, branch)
	}
	current := string(file.content)
	oldText, newText = matchLineEndings(current, oldText, newText)
	count := strings.Count(current, oldText)
	replaceAll := args.flag("replace_all")
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string was not found in %s on %s. Re-read the file and copy the text exactly, including whitespace", filePath, branch)
	case count > 1 && !replaceAll:
		return "", fmt.Errorf("old_string appears %d times in %s. Add surrounding lines to make it unique, or set replace_all", count, filePath)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	message := args.trimmed("message")
	if message == "" {
		message = "agent: edit " + filePath
	}
	commit, err := g.commitFile(ctx, branch, filePath, message, []byte(strings.Replace(current, oldText, newText, limit)), file.sha)
	if err != nil {
		return "", err
	}
	replaced := 1
	if replaceAll {
		replaced = count
	}
	return fmt.Sprintf("Edited %s on %s: %d replacement(s). Commit %s.", filePath, branch, replaced, commit), nil
}

func (g *githubAPI) writeTool(ctx context.Context, args toolArgs) (string, error) {
	branch, filePath, err := g.writeTarget(args)
	if err != nil {
		return "", err
	}
	content := args.text("content")
	switch {
	case len(content) > maxWriteFileBytes:
		return "", errors.New("content is larger than 1 MB")
	case strings.ContainsRune(content, 0):
		return "", errors.New("binary content cannot be written with this tool")
	case containsCommittableSecret(content):
		return "", errSecretInContent
	}
	if err := g.ensureBranch(ctx, branch); err != nil {
		return "", err
	}
	existing, found, err := g.fileAt(ctx, filePath, branch)
	if err != nil {
		return "", err
	}
	sha, verb := "", "Created"
	if found {
		sha, verb = existing.sha, "Replaced"
	}
	message := args.trimmed("message")
	if message == "" {
		message = "agent: " + strings.ToLower(verb) + " " + filePath
	}
	commit, err := g.commitFile(ctx, branch, filePath, message, []byte(content), sha)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s on %s (%d bytes). Commit %s.", verb, filePath, branch, len(content), commit), nil
}

func (g *githubAPI) deleteTool(ctx context.Context, args toolArgs) (string, error) {
	branch, filePath, err := g.writeTarget(args)
	if err != nil {
		return "", err
	}
	if err := g.ensureBranch(ctx, branch); err != nil {
		return "", err
	}
	file, found, err := g.fileAt(ctx, filePath, branch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s does not exist on %s", filePath, branch)
	}
	message := args.trimmed("message")
	if message == "" {
		message = "agent: delete " + filePath
	}
	commit, err := g.removeFile(ctx, branch, filePath, message, file.sha)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Deleted %s on %s. Commit %s.", filePath, branch, commit), nil
}
