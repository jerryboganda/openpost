package adminagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxPullRequestFiles = 60
	maxCheckNames       = 15
	dispatchLookups     = 6
	maxLogContextLines  = 150
	maxLogErrorLines    = 40
)

var (
	ansiPattern     = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	logStampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)
	workflowFile    = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)

	// dispatchLookupDelay is how long a started workflow gets to appear in the
	// run list. Tests shorten it.
	dispatchLookupDelay = 1500 * time.Millisecond
)

func (s *Service) pullRequestTools() []*toolSpec {
	g := s.github
	return []*toolSpec{
		{
			name:        "pr_open",
			description: "Open a pull request from a branch into the default branch. Write a Conventional Commit style title and a body that says what changed, why, and how it was verified.",
			write:       true,
			parameters: objectSchema([]string{"branch", "title"}, map[string]any{
				"branch": stringSchema("Branch that holds the changes."),
				"title":  stringSchema("Pull request title."),
				"body":   stringSchema("Description in Markdown."),
				"draft":  booleanSchema("Open as a draft."),
			}),
			describe: func(a toolArgs) string { return "Open a pull request from " + a.trimmed("branch") },
			run: func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) {
				return g.openPullRequest(ctx, a)
			},
		},
		{
			name:        "pr_get",
			description: "Show a pull request: state, mergeability, changed files, and the status of its checks.",
			parameters: objectSchema([]string{"number"}, map[string]any{
				"number": integerSchema("Pull request number."),
			}),
			describe: func(a toolArgs) string { return "Check pull request #" + a.trimmed("number") },
			run: func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) {
				return g.showPullRequest(ctx, a)
			},
		},
		{
			name:        "pr_merge",
			description: "Merge a pull request into the default branch. Merging starts the production deployment: the image is built, PostgreSQL is backed up, the digest is deployed, readiness is verified, and a failure rolls back. By default the merge is refused while checks are failing or still running.",
			write:       true,
			parameters: objectSchema([]string{"number"}, map[string]any{
				"number":        integerSchema("Pull request number."),
				"method":        enumSchema("Merge method. Defaults to squash.", "squash", "merge", "rebase"),
				"ignore_checks": booleanSchema("Merge even though checks fail or are pending. Use only when the administrator asked for it."),
			}),
			describe: func(a toolArgs) string { return "Merge pull request #" + a.trimmed("number") },
			run: func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) {
				return g.mergePullRequest(ctx, a)
			},
		},
	}
}

func (g *githubAPI) openPullRequest(ctx context.Context, args toolArgs) (string, error) {
	branch, err := g.writableBranch(args.text("branch"))
	if err != nil {
		return "", err
	}
	title := args.trimmed("title")
	if title == "" {
		return "", errors.New("title is required")
	}
	reply, err := g.call(ctx, http.MethodPost, g.repoPath("/pulls"), nil, map[string]any{
		"title": title, "head": branch, "base": g.branch, "body": args.text("body"), "draft": args.flag("draft"),
	}, "")
	if err != nil {
		return "", err
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(reply.body, &created); err != nil {
		return "", errors.New("the pull request was created but GitHub's answer could not be read")
	}
	return fmt.Sprintf("Opened pull request #%d from %s into %s: %s", created.Number, branch, g.branch, created.HTMLURL), nil
}

type pullRequestHead struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	HTMLURL        string `json:"html_url"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	ChangedFiles   int    `json:"changed_files"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (g *githubAPI) loadPullRequest(ctx context.Context, number int) (*pullRequestHead, error) {
	if number <= 0 {
		return nil, errors.New("number is required")
	}
	pr := new(pullRequestHead)
	if err := g.getJSON(ctx, g.repoPath("/pulls/"+strconv.Itoa(number)), nil, pr); err != nil {
		return nil, err
	}
	return pr, nil
}

type checkSummary struct {
	passed, failed, pending int
	failing, waiting        []string
}

func (g *githubAPI) checkRuns(ctx context.Context, sha string) (checkSummary, error) {
	var payload struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := g.getJSON(ctx, g.repoPath("/commits/"+url.PathEscape(sha)+"/check-runs"), url.Values{"per_page": {"100"}}, &payload); err != nil {
		return checkSummary{}, err
	}
	var summary checkSummary
	for _, run := range payload.CheckRuns {
		switch {
		case run.Status != "completed":
			summary.pending++
			summary.waiting = append(summary.waiting, run.Name)
		case run.Conclusion == "success" || run.Conclusion == "neutral" || run.Conclusion == "skipped":
			summary.passed++
		default:
			summary.failed++
			summary.failing = append(summary.failing, run.Name+" ("+run.Conclusion+")")
		}
	}
	return summary, nil
}

func (c checkSummary) brief() string {
	if c.passed+c.failed+c.pending == 0 {
		return "no checks have reported yet"
	}
	return fmt.Sprintf("%d passed, %d failed, %d pending", c.passed, c.failed, c.pending)
}

func (c checkSummary) detail() string {
	var builder strings.Builder
	builder.WriteString("Checks: " + c.brief() + "\n")
	for index, name := range c.failing {
		if index == maxCheckNames {
			break
		}
		builder.WriteString("  failed: " + name + "\n")
	}
	for index, name := range c.waiting {
		if index == maxCheckNames {
			break
		}
		builder.WriteString("  pending: " + name + "\n")
	}
	return builder.String()
}

func (g *githubAPI) showPullRequest(ctx context.Context, args toolArgs) (string, error) {
	pr, err := g.loadPullRequest(ctx, args.number("number", 0))
	if err != nil {
		return "", err
	}
	state := pr.State
	switch {
	case pr.Merged:
		state = "merged"
	case pr.Draft:
		state += " (draft)"
	}
	mergeable := "unknown yet"
	if pr.Mergeable != nil {
		mergeable = strconv.FormatBool(*pr.Mergeable)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "#%d %s\n%s\nState: %s by %s, %s into %s, head %s\n", pr.Number, pr.Title, pr.HTMLURL, state, pr.User.Login, pr.Head.Ref, pr.Base.Ref, shortSHA(pr.Head.SHA))
	fmt.Fprintf(&builder, "Mergeable: %s (%s). %d file(s), +%d −%d\n", mergeable, pr.MergeableState, pr.ChangedFiles, pr.Additions, pr.Deletions)
	var files []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	if err := g.getJSON(ctx, g.repoPath("/pulls/"+strconv.Itoa(pr.Number)+"/files"), url.Values{"per_page": {"100"}}, &files); err == nil {
		for index, file := range files {
			if index == maxPullRequestFiles {
				fmt.Fprintf(&builder, "…%d more files\n", len(files)-index)
				break
			}
			fmt.Fprintf(&builder, "  %s %s (+%d −%d)\n", file.Status, file.Filename, file.Additions, file.Deletions)
		}
	}
	if summary, err := g.checkRuns(ctx, pr.Head.SHA); err == nil {
		builder.WriteString(summary.detail())
	}
	return builder.String(), nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (g *githubAPI) mergePullRequest(ctx context.Context, args toolArgs) (string, error) {
	pr, err := g.loadPullRequest(ctx, args.number("number", 0))
	if err != nil {
		return "", err
	}
	switch {
	case pr.Merged:
		return "", fmt.Errorf("pull request #%d is already merged", pr.Number)
	case pr.State != "open":
		return "", fmt.Errorf("pull request #%d is %s", pr.Number, pr.State)
	case pr.Base.Ref != g.branch:
		return "", fmt.Errorf("pull request #%d targets %s, not %s", pr.Number, pr.Base.Ref, g.branch)
	}
	if !args.flag("ignore_checks") {
		summary, err := g.checkRuns(ctx, pr.Head.SHA)
		if err != nil {
			return "", err
		}
		if summary.failed > 0 || summary.pending > 0 {
			return "", fmt.Errorf("not merged: %s.\n%sWait for the checks or fix them. Set ignore_checks only when the administrator asked you to merge anyway", summary.brief(), summary.detail())
		}
	}
	method := strings.ToLower(args.trimmed("method"))
	switch method {
	case "":
		method = "squash"
	case "squash", "merge", "rebase":
	default:
		return "", errors.New("method must be squash, merge, or rebase")
	}
	reply, err := g.call(ctx, http.MethodPut, g.repoPath("/pulls/"+strconv.Itoa(pr.Number)+"/merge"), nil,
		map[string]string{"merge_method": method, "sha": pr.Head.SHA}, "")
	if err != nil {
		return "", err
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	var merged struct {
		SHA string `json:"sha"`
	}
	_ = json.Unmarshal(reply.body, &merged)
	return fmt.Sprintf("Merged pull request #%d into %s (%s, commit %s). The production deployment starts automatically from %s; follow it with ci_status workflow=fork-release.yml.", pr.Number, g.branch, method, shortSHA(merged.SHA), g.branch), nil
}

func (s *Service) actionsTools() []*toolSpec {
	g := s.github
	return []*toolSpec{
		{
			name:        "ci_status",
			description: "Show GitHub Actions runs. With run_id, show that run with its jobs and failing steps (job ids feed ci_logs). Without it, list recent runs, optionally filtered by workflow file, branch, or status.",
			parameters: objectSchema(nil, map[string]any{
				"run_id":   integerSchema("Run id to inspect."),
				"workflow": stringSchema("Workflow file name, for example fork-release.yml."),
				"branch":   stringSchema("Only runs for this branch."),
				"status":   stringSchema("queued, in_progress, completed, success, failure, or cancelled."),
				"limit":    integerSchema("Runs to list, 1 to 20. Defaults to 8."),
			}),
			describe: func(a toolArgs) string {
				if id := a.trimmed("run_id"); id != "" {
					return "Check workflow run " + id
				}
				return "List workflow runs"
			},
			run: func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.statusTool(ctx, a) },
		},
		{
			name:        "ci_logs",
			description: "Read the log of one GitHub Actions job. Without grep it returns the error lines found anywhere in the log plus the last lines. With grep it returns matching lines with context.",
			parameters: objectSchema([]string{"job_id"}, map[string]any{
				"job_id": integerSchema("Job id from ci_status."),
				"grep":   stringSchema("Case-insensitive text to find in the log."),
				"tail":   integerSchema("Last lines to include, 20 to 400. Defaults to 120."),
			}),
			describe: func(a toolArgs) string { return "Read logs of job " + a.trimmed("job_id") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.logsTool(ctx, a) },
		},
		{
			name:        "ci_dispatch",
			description: "Start a workflow that has a workflow_dispatch trigger. Useful ones: fork-regenerate.yml (inputs: branch) formats the code and regenerates OpenAPI and generated types on a branch and commits the result; fork-quick-check.yml (inputs: ref) runs the fast Go and web checks; fork-release.yml deploys.",
			write:       true,
			parameters: objectSchema([]string{"workflow"}, map[string]any{
				"workflow": stringSchema("Workflow file name, for example fork-regenerate.yml."),
				"ref":      stringSchema("Branch or tag that holds the workflow file. Defaults to the default branch."),
				"inputs":   objectProperty("Workflow inputs as string values."),
			}),
			describe: func(a toolArgs) string { return "Start workflow " + a.trimmed("workflow") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.dispatchTool(ctx, a) },
		},
		{
			name:        "ci_control",
			description: "Cancel a run, re-run a run or only its failed jobs, or approve a run that waits for approval.",
			write:       true,
			parameters: objectSchema([]string{"run_id", "action"}, map[string]any{
				"run_id": integerSchema("Run id."),
				"action": enumSchema("What to do.", "cancel", "rerun", "rerun_failed", "approve"),
			}),
			describe: func(a toolArgs) string { return "Workflow run " + a.trimmed("run_id") + ": " + a.trimmed("action") },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return g.controlTool(ctx, a) },
		},
		{
			name:        "github_api",
			description: "Call any other GitHub REST endpoint for this repository (issues, labels, releases, workflow runs, branches, comments). Use {repo} for owner/name. Reading is always allowed. Changes need full access and never touch the default branch, repository settings, or secrets.",
			parameters: objectSchema([]string{"path"}, map[string]any{
				"method": enumSchema("HTTP method. Defaults to GET.", "GET", "POST", "PUT", "PATCH", "DELETE"),
				"path":   stringSchema("Endpoint path such as /repos/{repo}/issues."),
				"query":  objectProperty("Query string parameters."),
				"body":   objectProperty("JSON request body."),
			}),
			describe: func(a toolArgs) string {
				method := strings.ToUpper(a.trimmed("method"))
				if method == "" {
					method = http.MethodGet
				}
				return "GitHub " + method + " " + a.trimmed("path")
			},
			run: func(ctx context.Context, rt *toolRuntime, a toolArgs) (string, error) { return g.apiTool(ctx, rt, a) },
		},
	}
}

type workflowRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
}

func (r workflowRun) outcome() string {
	if r.Status == "completed" && r.Conclusion != "" {
		return r.Conclusion
	}
	return r.Status
}

func (r workflowRun) line() string {
	return fmt.Sprintf("#%d %s | %s | %s | %s | %s | %s | %s",
		r.ID, r.Name, r.HeadBranch, r.Event, r.outcome(), humanAge(r.CreatedAt), shortSHA(r.HeadSHA), firstLine(r.DisplayTitle, 80))
}

func humanAge(at time.Time) string {
	if at.IsZero() {
		return "unknown time"
	}
	age := time.Since(at)
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%d min ago", int(age.Minutes()))
	case age < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(age.Hours()/24))
	}
}

func (g *githubAPI) statusTool(ctx context.Context, args toolArgs) (string, error) {
	if id := args.number("run_id", 0); id > 0 {
		return g.runDetails(ctx, id)
	}
	query := url.Values{"per_page": {strconv.Itoa(max(1, min(args.number("limit", 8), 20)))}}
	if branch := args.trimmed("branch"); branch != "" {
		query.Set("branch", branch)
	}
	if status := args.trimmed("status"); status != "" {
		query.Set("status", status)
	}
	endpoint := g.repoPath("/actions/runs")
	if workflow := args.trimmed("workflow"); workflow != "" {
		if !workflowFile.MatchString(workflow) {
			return "", errors.New("workflow must be a workflow file name such as fork-release.yml")
		}
		endpoint = g.repoPath("/actions/workflows/" + url.PathEscape(workflow) + "/runs")
	}
	var payload struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := g.getJSON(ctx, endpoint, query, &payload); err != nil {
		return "", err
	}
	if len(payload.WorkflowRuns) == 0 {
		return "No workflow runs match.", nil
	}
	lines := make([]string, 0, len(payload.WorkflowRuns))
	for _, run := range payload.WorkflowRuns {
		lines = append(lines, run.line())
	}
	return "id name | branch | event | outcome | age | commit | title\n" + strings.Join(lines, "\n"), nil
}

func (g *githubAPI) runDetails(ctx context.Context, id int) (string, error) {
	var run workflowRun
	if err := g.getJSON(ctx, g.repoPath("/actions/runs/"+strconv.Itoa(id)), nil, &run); err != nil {
		return "", err
	}
	var jobs struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := g.getJSON(ctx, g.repoPath("/actions/runs/"+strconv.Itoa(id)+"/jobs"), url.Values{"per_page": {"100"}}, &jobs); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString(run.line() + "\n" + run.HTMLURL + "\n")
	for _, job := range jobs.Jobs {
		outcome := job.Status
		if job.Status == "completed" {
			outcome = job.Conclusion
		}
		fmt.Fprintf(&builder, "job %d %s: %s\n", job.ID, job.Name, outcome)
		for _, step := range job.Steps {
			if step.Conclusion == "failure" {
				fmt.Fprintf(&builder, "    failed step: %s\n", step.Name)
			}
		}
	}
	return builder.String(), nil
}

func cleanLogLines(raw string) []string {
	lines := strings.Split(strings.TrimPrefix(raw, "\ufeff"), "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = ansiPattern.ReplaceAllString(strings.TrimRight(line, "\r"), "")
		cleaned = append(cleaned, logStampPattern.ReplaceAllString(line, ""))
	}
	return cleaned
}

var logErrorMarkers = []string{"##[error]", "--- fail", "panic:", " error:", "error ts", "exit code 1"}

func isLogErrorLine(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range logErrorMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (g *githubAPI) logsTool(ctx context.Context, args toolArgs) (string, error) {
	jobID := args.number("job_id", 0)
	if jobID <= 0 {
		return "", errors.New("job_id is required")
	}
	reply, err := g.call(ctx, http.MethodGet, g.repoPath("/actions/jobs/"+strconv.Itoa(jobID)+"/logs"), nil, nil, "")
	if err != nil {
		return "", err
	}
	switch reply.status {
	case http.StatusNotFound:
		return "", errors.New("no log for this job yet; it may not have started, or the log expired")
	case http.StatusGone:
		return "", errors.New("the log for this job expired")
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	lines := cleanLogLines(string(reply.body))
	if pattern := strings.ToLower(args.trimmed("grep")); pattern != "" {
		return grepLog(lines, pattern), nil
	}
	tail := max(20, min(args.number("tail", 120), 400))
	var builder strings.Builder
	var errorLines []string
	for _, line := range lines {
		if isLogErrorLine(line) {
			errorLines = append(errorLines, clipText(strings.TrimSpace(line), 400))
		}
	}
	if len(errorLines) > 0 {
		builder.WriteString("Error lines found in the log:\n")
		for index, line := range errorLines {
			if index == maxLogErrorLines {
				fmt.Fprintf(&builder, "…%d more error lines\n", len(errorLines)-index)
				break
			}
			builder.WriteString(line + "\n")
		}
		builder.WriteString("\n")
	}
	start := max(0, len(lines)-tail)
	fmt.Fprintf(&builder, "Last %d of %d lines:\n%s", len(lines)-start, len(lines), strings.Join(lines[start:], "\n"))
	return builder.String(), nil
}

func grepLog(lines []string, pattern string) string {
	keep := make(map[int]bool)
	matches := 0
	for index, line := range lines {
		if !strings.Contains(strings.ToLower(line), pattern) {
			continue
		}
		matches++
		for around := max(0, index-2); around <= min(len(lines)-1, index+2); around++ {
			keep[around] = true
		}
	}
	if matches == 0 {
		return fmt.Sprintf("No log lines contain %q.", pattern)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d line(s) contain %q\n", matches, pattern)
	shown, previous := 0, -2
	for index, line := range lines {
		if !keep[index] {
			continue
		}
		if shown == maxLogContextLines {
			builder.WriteString("…more matches not shown\n")
			break
		}
		if index != previous+1 {
			builder.WriteString("--\n")
		}
		builder.WriteString(clipText(line, 400) + "\n")
		previous = index
		shown++
	}
	return builder.String()
}

func (g *githubAPI) dispatchTool(ctx context.Context, args toolArgs) (string, error) {
	workflow := args.trimmed("workflow")
	if !workflowFile.MatchString(workflow) {
		return "", errors.New("workflow must be a workflow file name such as fork-regenerate.yml")
	}
	ref := g.refOrDefault(args.trimmed("ref"))
	inputs := make(map[string]string)
	for key, value := range args.object("inputs") {
		inputs[key] = scalarString(value)
	}
	started := time.Now()
	payload := map[string]any{"ref": ref}
	if len(inputs) > 0 {
		payload["inputs"] = inputs
	}
	reply, err := g.call(ctx, http.MethodPost, g.repoPath("/actions/workflows/"+url.PathEscape(workflow)+"/dispatches"), nil, payload, "")
	if err != nil {
		return "", err
	}
	if reply.status != http.StatusNoContent {
		return "", reply.failure()
	}
	for attempt := 0; attempt < dispatchLookups; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(dispatchLookupDelay):
		}
		var listing struct {
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		query := url.Values{"event": {"workflow_dispatch"}, "per_page": {"5"}}
		if err := g.getJSON(ctx, g.repoPath("/actions/workflows/"+url.PathEscape(workflow)+"/runs"), query, &listing); err != nil {
			continue
		}
		for _, run := range listing.WorkflowRuns {
			if run.CreatedAt.After(started.Add(-15 * time.Second)) {
				return fmt.Sprintf("Started %s on %s.\n%s\n%s", workflow, ref, run.line(), run.HTMLURL), nil
			}
		}
	}
	return fmt.Sprintf("Started %s on %s. The run is not visible yet; check it with ci_status workflow=%s.", workflow, ref, workflow), nil
}

func (g *githubAPI) controlTool(ctx context.Context, args toolArgs) (string, error) {
	id := args.number("run_id", 0)
	if id <= 0 {
		return "", errors.New("run_id is required")
	}
	suffix := map[string]string{
		"cancel": "/cancel", "rerun": "/rerun", "rerun_failed": "/rerun-failed-jobs", "approve": "/approve",
	}[strings.ToLower(args.trimmed("action"))]
	if suffix == "" {
		return "", errors.New("action must be cancel, rerun, rerun_failed, or approve")
	}
	reply, err := g.call(ctx, http.MethodPost, g.repoPath("/actions/runs/"+strconv.Itoa(id)+suffix), nil, nil, "")
	if err != nil {
		return "", err
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	return fmt.Sprintf("Done: %s for run %d (HTTP %d).", args.trimmed("action"), id, reply.status), nil
}

func (g *githubAPI) apiTool(ctx context.Context, rt *toolRuntime, args toolArgs) (string, error) {
	method := strings.ToUpper(args.trimmed("method"))
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", errors.New("method must be GET, POST, PUT, PATCH, or DELETE")
	}
	if method != http.MethodGet && rt.access == AccessReadOnly {
		return "", errReadOnlyAccess
	}
	endpoint := "/" + strings.TrimLeft(strings.ReplaceAll(args.trimmed("path"), "{repo}", g.repository), "/")
	body := args.object("body")
	if err := g.apiGuard(method, endpoint); err != nil {
		return "", err
	}
	if body != nil && containsCommittableSecret(fmt.Sprint(body)) {
		return "", errSecretInContent
	}
	query := url.Values{}
	for key, value := range args.object("query") {
		query.Set(key, scalarString(value))
	}
	var payload any
	if body != nil {
		payload = body
	}
	reply, err := g.call(ctx, method, endpoint, query, payload, "")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("HTTP %d\n%s", reply.status, clipText(string(reply.body), maxToolOutput)), nil
}
