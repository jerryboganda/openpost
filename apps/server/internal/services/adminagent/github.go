package adminagent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	githubMaxBody      = 8 << 20
	githubMaxEditBytes = 1 << 20
	githubAPIVersion   = "2022-11-28"
	githubUserAgent    = "openpost-agent/1.0"
	maxTreeCacheSets   = 4
)

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	agentIdentity     = map[string]string{"name": "OpenPost Agent", "email": "openpost-agent@users.noreply.github.com"}

	errSecretInContent = errors.New("the content looks like it contains a credential, so it was not committed; remove the secret and read it from configuration instead")
)

// githubAPI is a small GitHub REST client scoped to one repository. It also
// owns the guard rails for writes: nothing is written to the default branch,
// and credential-like content never reaches a commit.
type githubAPI struct {
	token      string
	repository string
	branch     string
	base       string
	client     *http.Client

	mu        sync.Mutex
	treeCache map[string][]string
}

func newGitHubAPI(token, repository, defaultBranch string, client *http.Client) (*githubAPI, error) {
	token = strings.TrimSpace(token)
	repository = strings.TrimSpace(repository)
	defaultBranch = strings.TrimSpace(defaultBranch)
	switch {
	case token == "":
		return nil, errors.New("GitHub token is not configured")
	case !repositoryPattern.MatchString(repository):
		return nil, errors.New("GitHub repository must look like owner/name")
	case defaultBranch == "":
		defaultBranch = "main"
	}
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	return &githubAPI{
		token:      token,
		repository: repository,
		branch:     defaultBranch,
		base:       githubAPIBase,
		client:     client,
		treeCache:  make(map[string][]string),
	}, nil
}

// SetGitHub enables the repository, pull request, and Actions tools.
func (s *Service) SetGitHub(token, repository, defaultBranch string) error {
	api, err := newGitHubAPI(token, repository, defaultBranch, nil)
	if err != nil {
		return err
	}
	s.github = api
	return nil
}

type githubReply struct {
	status    int
	body      []byte
	header    http.Header
	truncated bool
}

func (r githubReply) ok() bool {
	return r.status >= 200 && r.status < 300
}

func (r githubReply) failure() error {
	if (r.status == http.StatusForbidden || r.status == http.StatusTooManyRequests) &&
		r.header.Get("X-Ratelimit-Remaining") == "0" {
		return fmt.Errorf("GitHub rate limit reached; it resets at unix time %s", r.header.Get("X-Ratelimit-Reset"))
	}
	var envelope struct {
		Message string            `json:"message"`
		Errors  []json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(r.body, &envelope)
	message := strings.TrimSpace(envelope.Message)
	if message == "" {
		message = http.StatusText(r.status)
	}
	if len(envelope.Errors) > 0 {
		message += " " + clipText(string(envelope.Errors[0]), 300)
	}
	return fmt.Errorf("GitHub returned %d: %s", r.status, message)
}

func (g *githubAPI) call(
	ctx context.Context,
	method, endpoint string,
	query url.Values,
	payload any,
	accept string,
) (githubReply, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return githubReply{}, fmt.Errorf("encode GitHub request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	target := g.base + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return githubReply{}, err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	request.Header.Set("User-Agent", githubUserAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := g.client.Do(request)
	if err != nil {
		return githubReply{}, fmt.Errorf("GitHub request failed: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if err != nil {
		return githubReply{}, errors.New("GitHub response could not be read")
	}
	truncated := len(raw) > githubMaxBody
	if truncated {
		raw = raw[:githubMaxBody]
	}
	return githubReply{status: response.StatusCode, body: raw, header: response.Header, truncated: truncated}, nil
}

// getJSON expects a 2xx answer and decodes it into target.
func (g *githubAPI) getJSON(ctx context.Context, endpoint string, query url.Values, target any) error {
	reply, err := g.call(ctx, http.MethodGet, endpoint, query, nil, "")
	if err != nil {
		return err
	}
	if !reply.ok() {
		return reply.failure()
	}
	if err := json.Unmarshal(reply.body, target); err != nil {
		return errors.New("GitHub returned a response this tool could not read")
	}
	return nil
}

func (g *githubAPI) repoPath(suffix string) string {
	return "/repos/" + g.repository + suffix
}

func escapeRepoPath(value string) string {
	segments := strings.Split(value, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return strings.Join(segments, "/")
}

func (g *githubAPI) refOrDefault(ref string) string {
	ref = strings.TrimSpace(strings.TrimPrefix(ref, "refs/heads/"))
	if ref == "" {
		return g.branch
	}
	return ref
}

// writableBranch validates a branch for writing. The default branch is never
// written directly: changes reach it through a reviewed pull request.
func (g *githubAPI) writableBranch(branch string) (string, error) {
	branch = strings.TrimSpace(strings.TrimPrefix(branch, "refs/heads/"))
	switch {
	case branch == "":
		return "", errors.New("branch is required")
	case strings.EqualFold(branch, g.branch):
		return "", fmt.Errorf("writing directly to %s is not allowed; use another branch such as agent/<topic> and open a pull request", g.branch)
	case !branchPattern.MatchString(branch), strings.Contains(branch, ".."), strings.HasSuffix(branch, "/"), strings.HasSuffix(branch, ".lock"):
		return "", errors.New("branch name is not valid; use letters, digits, dots, dashes, underscores, and slashes")
	}
	return branch, nil
}

func cleanRepoPath(raw string) (string, error) {
	cleaned := strings.Trim(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"), "/")
	if cleaned == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(cleaned, 0) {
		return "", errors.New("path is not valid")
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("path must not contain empty, . or .. segments")
		}
	}
	return cleaned, nil
}

// checkWritablePath keeps credentials and repository internals out of commits.
func checkWritablePath(repoPath string) error {
	lower := strings.ToLower(repoPath)
	base := path.Base(lower)
	switch {
	case lower == ".git" || strings.HasPrefix(lower, ".git/"):
		return errors.New("the .git directory cannot be written")
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return errors.New("environment files hold secrets and are never committed")
	case strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || base == "id_rsa" || base == "id_ed25519":
		return errors.New("private key files are never committed")
	}
	return nil
}

func (g *githubAPI) headSHA(ctx context.Context, branch string) (string, bool, error) {
	reply, err := g.call(ctx, http.MethodGet, g.repoPath("/git/ref/heads/"+escapeRepoPath(branch)), nil, nil, "")
	if err != nil {
		return "", false, err
	}
	if reply.status == http.StatusNotFound {
		return "", false, nil
	}
	if !reply.ok() {
		return "", false, reply.failure()
	}
	var payload struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(reply.body, &payload); err != nil || payload.Object.SHA == "" {
		return "", false, errors.New("GitHub returned a branch this tool could not read")
	}
	return payload.Object.SHA, true, nil
}

// ensureBranch creates the branch from the default branch when it is missing.
func (g *githubAPI) ensureBranch(ctx context.Context, branch string) error {
	if _, found, err := g.headSHA(ctx, branch); err != nil || found {
		return err
	}
	base, found, err := g.headSHA(ctx, g.branch)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("the default branch %s was not found", g.branch)
	}
	reply, err := g.call(ctx, http.MethodPost, g.repoPath("/git/refs"), nil,
		map[string]string{"ref": "refs/heads/" + branch, "sha": base}, "")
	if err != nil {
		return err
	}
	// A concurrent creation answers 422 "Reference already exists", which is fine.
	if !reply.ok() && reply.status != http.StatusUnprocessableEntity {
		return reply.failure()
	}
	return nil
}

type githubFile struct {
	sha     string
	content []byte
}

// fileAt reads a file through the contents API. found is false for a missing
// path, so callers can tell "create" from "update".
func (g *githubAPI) fileAt(ctx context.Context, repoPath, ref string) (*githubFile, bool, error) {
	query := url.Values{}
	if ref != "" {
		query.Set("ref", ref)
	}
	reply, err := g.call(ctx, http.MethodGet, g.repoPath("/contents/"+escapeRepoPath(repoPath)), query, nil, "")
	if err != nil {
		return nil, false, err
	}
	if reply.status == http.StatusNotFound {
		return nil, false, nil
	}
	if !reply.ok() {
		return nil, false, reply.failure()
	}
	var payload struct {
		Type     string `json:"type"`
		SHA      string `json:"sha"`
		Size     int    `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(reply.body, &payload); err != nil {
		return nil, false, fmt.Errorf("%s is a directory, not a file", repoPath)
	}
	if payload.Type != "file" {
		return nil, false, fmt.Errorf("%s is not a regular file", repoPath)
	}
	if payload.Size > githubMaxEditBytes || payload.Encoding != "base64" {
		return nil, false, fmt.Errorf("%s is larger than 1 MB, which this tool cannot edit", repoPath)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(payload.Content))
	if err != nil {
		return nil, false, errors.New("the file content could not be decoded")
	}
	return &githubFile{sha: payload.SHA, content: decoded}, true, nil
}

// commitFile creates or updates one file on a branch and returns the commit.
func (g *githubAPI) commitFile(ctx context.Context, branch, repoPath, message string, content []byte, sha string) (string, error) {
	payload := map[string]any{
		"message":   message,
		"content":   base64.StdEncoding.EncodeToString(content),
		"branch":    branch,
		"committer": agentIdentity,
		"author":    agentIdentity,
	}
	if sha != "" {
		payload["sha"] = sha
	}
	return g.writeContents(ctx, http.MethodPut, repoPath, payload)
}

func (g *githubAPI) removeFile(ctx context.Context, branch, repoPath, message, sha string) (string, error) {
	return g.writeContents(ctx, http.MethodDelete, repoPath, map[string]any{
		"message": message, "sha": sha, "branch": branch, "committer": agentIdentity, "author": agentIdentity,
	})
}

func (g *githubAPI) writeContents(ctx context.Context, method, repoPath string, payload map[string]any) (string, error) {
	reply, err := g.call(ctx, method, g.repoPath("/contents/"+escapeRepoPath(repoPath)), nil, payload, "")
	if err != nil {
		return "", err
	}
	if !reply.ok() {
		return "", reply.failure()
	}
	var result struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	_ = json.Unmarshal(reply.body, &result)
	if len(result.Commit.SHA) >= 7 {
		return result.Commit.SHA[:7], nil
	}
	return "unknown", nil
}

// treePaths lists every file path at a ref. Results are cached by commit, which
// never changes, so repeated searches do not repeat the large request.
func (g *githubAPI) treePaths(ctx context.Context, ref string) ([]string, bool, error) {
	sha := ref
	if !commitPattern.MatchString(ref) {
		head, found, err := g.headSHA(ctx, ref)
		if err != nil {
			return nil, false, err
		}
		if found {
			sha = head
		}
	}
	g.mu.Lock()
	cached, hit := g.treeCache[sha]
	g.mu.Unlock()
	if hit {
		return cached, false, nil
	}
	var payload struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := g.getJSON(ctx, g.repoPath("/git/trees/"+url.PathEscape(sha)), url.Values{"recursive": {"1"}}, &payload); err != nil {
		return nil, false, err
	}
	paths := make([]string, 0, len(payload.Tree))
	for _, entry := range payload.Tree {
		if entry.Type == "blob" {
			paths = append(paths, entry.Path)
		}
	}
	g.mu.Lock()
	if len(g.treeCache) >= maxTreeCacheSets {
		g.treeCache = make(map[string][]string)
	}
	g.treeCache[sha] = paths
	g.mu.Unlock()
	return paths, payload.Truncated, nil
}

// apiGuard blocks the few requests that could destroy the repository or bypass
// the pull request flow. Everything else the administrator may do is allowed.
func (g *githubAPI) apiGuard(method, endpoint string, body map[string]any) error {
	prefix := "/repos/" + g.repository
	if endpoint != prefix && !strings.HasPrefix(endpoint, prefix+"/") &&
		!strings.HasPrefix(endpoint, "/search/") && endpoint != "/rate_limit" {
		return fmt.Errorf("only %s, /search, and /rate_limit endpoints are available", prefix)
	}
	if method == http.MethodGet {
		return nil
	}
	lower := strings.ToLower(strings.TrimSuffix(endpoint, "/"))
	switch {
	case lower == strings.ToLower(prefix):
		return errors.New("changing, transferring, or deleting the repository itself is not allowed")
	case strings.Contains(lower, "/actions/secrets") || strings.Contains(lower, "/actions/variables"):
		return errors.New("secrets and variables are managed by the administrator, not by the agent")
	case strings.Contains(lower, "/git/refs/heads/"+strings.ToLower(g.branch)),
		strings.Contains(lower, "/branches/"+strings.ToLower(g.branch)+"/protection"):
		return fmt.Errorf("the %s branch is changed only through merged pull requests", g.branch)
	case strings.Contains(lower, "/contents/"):
		branch, _ := body["branch"].(string)
		if strings.TrimSpace(branch) == "" || strings.EqualFold(strings.TrimSpace(branch), g.branch) {
			return fmt.Errorf("file changes need a branch other than %s", g.branch)
		}
	}
	return nil
}
