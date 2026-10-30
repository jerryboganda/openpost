package ai

import (
	"context"
	"regexp"
	"slices"
	"strings"
)

// reasoningLadder orders every effort the application understands, lowest
// first. Providers accept different subsets of it.
var reasoningLadder = []ReasoningEffort{
	ReasoningEffortNone,
	ReasoningEffortMinimal,
	ReasoningEffortLow,
	ReasoningEffortMedium,
	ReasoningEffortHigh,
	ReasoningEffortXHigh,
	ReasoningEffortMax,
}

// ParseReasoningEffort reads a reasoning choice. An empty value and "auto" mean
// the provider's own default and return an empty effort.
func ParseReasoningEffort(value string) (ReasoningEffort, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "auto" {
		return "", true
	}
	effort := ReasoningEffort(value)
	if !slices.Contains(reasoningLadder, effort) {
		return "", false
	}
	return effort, true
}

func reasoningRank(effort ReasoningEffort) int {
	return slices.Index(reasoningLadder, effort)
}

// ReasoningProfile lists the efforts one model accepts, lowest first. Auto,
// which sends nothing and lets the provider choose, always works and is not
// listed. A model that cannot reason on demand has an empty profile.
type ReasoningProfile struct {
	efforts []ReasoningEffort
}

// NewReasoningProfile keeps each known effort once, in ladder order.
func NewReasoningProfile(efforts ...ReasoningEffort) ReasoningProfile {
	kept := make([]ReasoningEffort, 0, len(efforts))
	for _, candidate := range reasoningLadder {
		if slices.Contains(efforts, candidate) {
			kept = append(kept, candidate)
		}
	}
	return ReasoningProfile{efforts: kept}
}

// ParseReasoningProfile reads a comma-separated list such as "low,high,max". It
// reports false when the list names something that is not an effort.
func ParseReasoningProfile(list string) (ReasoningProfile, bool) {
	fields := strings.Split(list, ",")
	efforts := make([]ReasoningEffort, 0, len(fields))
	for _, field := range fields {
		effort, ok := ParseReasoningEffort(field)
		if !ok {
			return ReasoningProfile{}, false
		}
		if effort != "" {
			efforts = append(efforts, effort)
		}
	}
	return NewReasoningProfile(efforts...), true
}

// DefaultReasoningProfile is offered for models nothing more specific is known
// about. It is the set almost every reasoning endpoint accepts.
func DefaultReasoningProfile() ReasoningProfile {
	return NewReasoningProfile(ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh)
}

// Names returns the accepted efforts as request values. It is never nil.
func (p ReasoningProfile) Names() []string {
	names := make([]string, 0, len(p.efforts))
	for _, effort := range p.efforts {
		names = append(names, string(effort))
	}
	return names
}

// Supports reports whether the model accepts effort. Auto always works.
func (p ReasoningProfile) Supports(effort ReasoningEffort) bool {
	return effort == "" || slices.Contains(p.efforts, effort)
}

// Nearest returns the effort to use when effort itself is not accepted: the
// highest accepted effort that is not above it, otherwise the lowest accepted
// one, otherwise auto.
func (p ReasoningProfile) Nearest(effort ReasoningEffort) ReasoningEffort {
	if p.Supports(effort) {
		return effort
	}
	if len(p.efforts) == 0 {
		return ""
	}
	rank := reasoningRank(effort)
	chosen := p.efforts[0]
	for _, candidate := range p.efforts {
		if reasoningRank(candidate) <= rank {
			chosen = candidate
		}
	}
	return chosen
}

// ReasoningSource finds the efforts a model accepts. It reports false when it
// knows nothing about the model, so the next source can answer.
type ReasoningSource interface {
	ReasoningProfile(ctx context.Context, model string) (ReasoningProfile, bool)
}

// ReasoningSources asks each source in order and keeps the first answer.
type ReasoningSources []ReasoningSource

func (s ReasoningSources) ReasoningProfile(ctx context.Context, model string) (ReasoningProfile, bool) {
	for _, source := range s {
		if source == nil {
			continue
		}
		if profile, ok := source.ReasoningProfile(ctx, model); ok {
			return profile, true
		}
	}
	return ReasoningProfile{}, false
}

// FixedReasoning answers every model with one profile. Operators use it when
// they know what their endpoint accepts.
type FixedReasoning struct {
	Profile ReasoningProfile
}

func (f FixedReasoning) ReasoningProfile(context.Context, string) (ReasoningProfile, bool) {
	return f.Profile, true
}

// OpenAIReasoning knows the published efforts of OpenAI's own models.
type OpenAIReasoning struct{}

func (OpenAIReasoning) ReasoningProfile(_ context.Context, model string) (ReasoningProfile, bool) {
	return OpenAIReasoningProfile(model)
}

var openAIOSeries = regexp.MustCompile(`^o[0-9]`)

// openAIRule lists the efforts OpenAI documents for models whose name starts
// with prefix. A "-pro" or "-codex" variant uses its own list when it has one.
type openAIRule struct {
	prefix string
	plain  []ReasoningEffort
	pro    []ReasoningEffort
	codex  []ReasoningEffort
}

// openAIRules are checked in order, so a more specific prefix comes first.
var openAIRules = []openAIRule{
	{
		prefix: "gpt-6",
		plain: []ReasoningEffort{
			ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax,
		},
		pro: []ReasoningEffort{
			ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax,
		},
	},
	{
		prefix: "gpt-5.6",
		plain: []ReasoningEffort{
			ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax,
		},
		pro: []ReasoningEffort{
			ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax,
		},
	},
	{
		prefix: "gpt-5.1-codex",
		plain:  []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	},
	{
		prefix: "gpt-5.1",
		plain:  []ReasoningEffort{ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	},
	{
		prefix: "gpt-5.",
		plain:  []ReasoningEffort{ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh},
		pro:    []ReasoningEffort{ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh},
		codex:  []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh},
	},
	{
		prefix: "gpt-5",
		plain:  []ReasoningEffort{ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
		pro:    []ReasoningEffort{ReasoningEffortHigh},
	},
}

// openAIDoesNotReason reports whether a model name belongs to a family that
// takes no reasoning effort at all.
func openAIDoesNotReason(id string) bool {
	if strings.HasPrefix(id, "gpt-4") || strings.HasPrefix(id, "gpt-3") {
		return true
	}
	for _, marker := range []string{"-chat", "-image", "-audio"} {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}

// OpenAIReasoningProfile returns the efforts OpenAI documents for its models.
// Models that do not reason have an empty profile, so no effort is sent to
// them. It reports false for names it does not recognize.
func OpenAIReasoningProfile(model string) (ReasoningProfile, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	id = strings.TrimPrefix(id, "openai/")
	if cut := strings.IndexByte(id, ':'); cut >= 0 {
		id = id[:cut]
	}
	if id == "" {
		return ReasoningProfile{}, false
	}
	if openAIDoesNotReason(id) {
		return ReasoningProfile{}, true
	}
	for _, rule := range openAIRules {
		if !strings.HasPrefix(id, rule.prefix) {
			continue
		}
		efforts := rule.plain
		if rule.pro != nil && strings.Contains(id, "-pro") {
			efforts = rule.pro
		} else if rule.codex != nil && strings.Contains(id, "-codex") {
			efforts = rule.codex
		}
		return NewReasoningProfile(efforts...), true
	}
	if openAIOSeries.MatchString(id) {
		return NewReasoningProfile(ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh), true
	}
	return ReasoningProfile{}, false
}
