package adminagent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	githubAPIBase           = "https://api.github.com"
	githubRunsPerPage       = 100
	githubRunSearchPages    = 5
	githubResponseMaxBytes  = 2 << 20
	callbackSkew            = 5 * time.Minute
	CallbackTimestampHeader = "X-OpenPost-Agent-Timestamp"
	CallbackSignatureHeader = "X-OpenPost-Agent-Signature"
)

type GitHubConfig struct {
	Repository     string
	Workflow       string
	Ref            string
	Token          string
	CallbackURL    string
	CallbackSecret string
	HTTPClient     *http.Client
}

type GitHubDispatcher struct {
	config GitHubConfig
	client *http.Client
}

func NewGitHubDispatcher(config GitHubConfig) (*GitHubDispatcher, error) {
	config.Repository = strings.TrimSpace(config.Repository)
	config.Workflow = strings.TrimSpace(config.Workflow)
	config.Ref = strings.TrimSpace(config.Ref)
	config.Token = strings.TrimSpace(config.Token)
	config.CallbackURL = strings.TrimSpace(config.CallbackURL)
	config.CallbackSecret = strings.TrimSpace(config.CallbackSecret)
	switch {
	case config.Repository == "":
		return nil, errors.New("agent repository is not configured")
	case config.Workflow == "":
		return nil, errors.New("agent workflow is not configured")
	case config.Ref == "":
		return nil, errors.New("agent ref is not configured")
	case config.Token == "":
		return nil, errors.New("agent GitHub token is not configured")
	case config.CallbackURL == "":
		return nil, errors.New("agent callback URL is not configured")
	case !strings.HasPrefix(config.CallbackURL, "https://") && !strings.HasPrefix(config.CallbackURL, "http://"):
		return nil, errors.New("agent callback URL must be absolute")
	case config.CallbackSecret == "":
		return nil, errors.New("agent callback secret is not configured")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubDispatcher{config: config, client: client}, nil
}

func (d *GitHubDispatcher) Dispatch(ctx context.Context, request DispatchRequest) (DispatchResult, error) {
	body, err := json.Marshal(map[string]any{
		"ref": d.config.Ref,
		"inputs": map[string]string{
			"run_id":       request.RunID,
			"callback_url": d.config.CallbackURL,
			"context_url":  strings.TrimSuffix(d.config.CallbackURL, "/callback") + "/runs/" + request.RunID + "/context",
		},
	})
	if err != nil {
		return DispatchResult{}, err
	}
	url := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", githubAPIBase, d.config.Repository, d.config.Workflow)
	response, err := d.do(ctx, http.MethodPost, url, body)
	if err != nil {
		return DispatchResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return DispatchResult{}, fmt.Errorf("GitHub Actions dispatch failed with status %d", response.StatusCode)
	}
	return DispatchResult{
		Ref: d.config.Ref,
		URL: fmt.Sprintf("https://github.com/%s/actions/workflows/%s", d.config.Repository, d.config.Workflow),
	}, nil
}

func (d *GitHubDispatcher) Cancel(ctx context.Context, run AgentRun) error {
	if run.GitHubRunID <= 0 {
		return ErrGitHubRunIDPending
	}
	url := fmt.Sprintf("%s/repos/%s/actions/runs/%d/cancel", githubAPIBase, d.config.Repository, run.GitHubRunID)
	response, err := d.do(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("GitHub Actions cancel failed with status %d", response.StatusCode)
	}
	return nil
}

// LookupRun trusts a stored, signed run ID. Before registration, it accepts
// only an unambiguous exact workflow title within a bounded dispatch search.
//nolint:gocyclo // Each branch classifies one GitHub run lookup outcome.
func (d *GitHubDispatcher) LookupRun(ctx context.Context, run AgentRun) (GitHubRunState, error) {
	title := "openpost-agent-" + run.ID
	if run.GitHubRunID > 0 {
		endpoint := fmt.Sprintf("%s/repos/%s/actions/runs/%d", githubAPIBase, d.config.Repository, run.GitHubRunID)
		var remote GitHubRunState
		if err := d.getJSON(ctx, endpoint, &remote); err != nil {
			return GitHubRunState{}, err
		}
		if remote.ID != run.GitHubRunID || remote.DisplayTitle != title {
			return GitHubRunState{}, ErrGitHubRunUnidentified
		}
		return remote, nil
	}

	var matched GitHubRunState
	for page := 1; page <= githubRunSearchPages; page++ {
		query := url.Values{}
		query.Set("event", "workflow_dispatch")
		query.Set("created", ">="+run.CreatedAt.Add(-5*time.Minute).UTC().Format(time.RFC3339))
		query.Set("per_page", fmt.Sprint(githubRunsPerPage))
		query.Set("page", fmt.Sprint(page))
		endpoint := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?%s", githubAPIBase, d.config.Repository, d.config.Workflow, query.Encode())
		var listing struct {
			TotalCount   int              `json:"total_count"`
			WorkflowRuns []GitHubRunState `json:"workflow_runs"`
		}
		if err := d.getJSON(ctx, endpoint, &listing); err != nil {
			return GitHubRunState{}, err
		}
		if listing.TotalCount > githubRunsPerPage*githubRunSearchPages {
			return GitHubRunState{}, ErrGitHubRunSearchLimit
		}
		for _, remote := range listing.WorkflowRuns {
			if remote.DisplayTitle != title || remote.ID <= 0 {
				continue
			}
			if matched.ID != 0 {
				return GitHubRunState{}, ErrGitHubRunAmbiguous
			}
			matched = remote
		}
		if len(listing.WorkflowRuns) < githubRunsPerPage || (listing.TotalCount > 0 && page*githubRunsPerPage >= listing.TotalCount) {
			if matched.ID == 0 {
				return GitHubRunState{}, ErrGitHubRunUnidentified
			}
			return matched, nil
		}
	}
	return GitHubRunState{}, ErrGitHubRunSearchLimit
}

func (d *GitHubDispatcher) getJSON(ctx context.Context, endpoint string, target any) error {
	response, err := d.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("query GitHub Actions: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub Actions lookup returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, githubResponseMaxBytes+1))
	if err != nil {
		return fmt.Errorf("read GitHub Actions response: %w", err)
	}
	if len(body) > githubResponseMaxBytes {
		return errors.New("GitHub Actions response exceeded recovery limit")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode GitHub Actions response: %w", err)
	}
	return nil
}

func (d *GitHubDispatcher) do(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Authorization", "Bearer "+d.config.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return d.client.Do(request)
}

func SignCallback(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyCallback(secret, timestamp, signature string, body []byte, now time.Time) error {
	if strings.TrimSpace(secret) == "" {
		return ErrInvalidCallback
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(timestamp))
	if err != nil {
		return ErrInvalidCallback
	}
	if now.Sub(parsed) > callbackSkew || parsed.Sub(now) > callbackSkew {
		return ErrInvalidCallback
	}
	expected := SignCallback(secret, timestamp, body)
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature))) {
		return ErrInvalidCallback
	}
	return nil
}
