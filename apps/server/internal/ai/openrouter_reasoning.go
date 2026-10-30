package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	openRouterReasoningTTL     = 6 * time.Hour
	openRouterReasoningBackoff = 5 * time.Minute
	openRouterReasoningTimeout = 10 * time.Second
	openRouterReasoningMaxBody = 16 << 20
)

// OpenRouterReasoning reads each model's accepted efforts from OpenRouter's
// model list, so a model it routes to offers the tiers it really has. A failed
// refresh keeps serving the last list.
type OpenRouterReasoning struct {
	client  HTTPClient
	baseURL string
	apiKey  string
	headers map[string]string
	now     func() time.Time

	mu        sync.Mutex
	models    map[string]ReasoningProfile
	fetchedAt time.Time
	failedAt  time.Time
}

var _ ReasoningSource = (*OpenRouterReasoning)(nil)

// NewOpenRouterReasoning reuses the provider configuration of the other AI
// features, so a proxied or self-hosted OpenRouter endpoint is followed.
func NewOpenRouterReasoning(config OpenRouterConfig) *OpenRouterReasoning {
	baseURL, err := resolveProviderBaseURL(config.BaseURL, ProviderModeOpenRouter)
	if err != nil {
		baseURL = defaultOpenRouterBaseURL
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	headers := make(map[string]string, len(config.Headers))
	for key, value := range config.Headers {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			headers[key] = value
		}
	}
	return &OpenRouterReasoning{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  strings.TrimSpace(config.APIKey),
		headers: headers,
		now:     time.Now,
	}
}

// ReasoningProfile answers for models OpenRouter lists. A listed model without
// effort controls has an empty profile. Unlisted models are left to the next
// source.
func (r *OpenRouterReasoning) ReasoningProfile(ctx context.Context, model string) (ReasoningProfile, bool) {
	models, _ := r.load(ctx)
	id := strings.ToLower(strings.TrimSpace(model))
	if profile, ok := models[id]; ok {
		return profile, true
	}
	if cut := strings.IndexByte(id, ':'); cut > 0 {
		profile, ok := models[id[:cut]]
		return profile, ok
	}
	return ReasoningProfile{}, false
}

// load returns the cached list, refreshing it when it is old. After a failed
// refresh it keeps the previous list and waits before trying again.
func (r *OpenRouterReasoning) load(ctx context.Context) (map[string]ReasoningProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.models != nil && now.Sub(r.fetchedAt) < openRouterReasoningTTL {
		return r.models, nil
	}
	if !r.failedAt.IsZero() && now.Sub(r.failedAt) < openRouterReasoningBackoff {
		return r.models, errors.New("OpenRouter model list is unavailable")
	}
	models, err := r.fetch(ctx)
	if err != nil {
		r.failedAt = now
		return r.models, err
	}
	r.models, r.fetchedAt, r.failedAt = models, now, time.Time{}
	return models, nil
}

func (r *OpenRouterReasoning) fetch(ctx context.Context) (map[string]ReasoningProfile, error) {
	ctx, cancel := context.WithTimeout(ctx, openRouterReasoningTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", chatUserAgent)
	if r.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	for key, value := range r.headers {
		request.Header.Set(key, value)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenRouter model list answered %d", response.StatusCode)
	}
	return parseOpenRouterReasoning(io.LimitReader(response.Body, openRouterReasoningMaxBody))
}

// parseOpenRouterReasoning reads the reasoning object of each listed model.
// supported_efforts lists what the model accepts, including "none" when
// reasoning can be turned off. A null list means every effort is accepted, and a
// missing one means the model has no effort control.
func parseOpenRouterReasoning(body io.Reader) (map[string]ReasoningProfile, error) {
	var payload struct {
		Data []struct {
			ID        string `json:"id"`
			Reasoning *struct {
				Mandatory        bool            `json:"mandatory"`
				SupportedEfforts json.RawMessage `json:"supported_efforts"`
			} `json:"reasoning"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, errors.New("OpenRouter model list is empty")
	}
	models := make(map[string]ReasoningProfile, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id == "" {
			continue
		}
		var efforts []ReasoningEffort
		if entry.Reasoning != nil {
			var err error
			efforts, err = openRouterEfforts(entry.Reasoning.Mandatory, entry.Reasoning.SupportedEfforts)
			if err != nil {
				return nil, err
			}
		}
		models[id] = NewReasoningProfile(efforts...)
	}
	return models, nil
}

// openRouterEfforts reads one model's supported_efforts. A null list means every
// effort is accepted, except that a model which must reason cannot turn it off.
func openRouterEfforts(mandatory bool, raw json.RawMessage) ([]ReasoningEffort, error) {
	switch strings.TrimSpace(string(raw)) {
	case "":
		return nil, nil
	case "null":
		efforts := make([]ReasoningEffort, 0, len(reasoningLadder))
		for _, effort := range reasoningLadder {
			if effort != ReasoningEffortNone || !mandatory {
				efforts = append(efforts, effort)
			}
		}
		return efforts, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	efforts := make([]ReasoningEffort, 0, len(names))
	for _, name := range names {
		if effort, ok := ParseReasoningEffort(name); ok && effort != "" {
			efforts = append(efforts, effort)
		}
	}
	return efforts, nil
}
