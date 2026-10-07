package acquisition

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ProwlarrExecutionResolver is explicit execution authority, separate from the
// discovery adapter. Option A does no remote work before the durable claim.
// The concrete Server only registers it when qBittorrent is locally configured.
type ProwlarrExecutionResolver struct {
	provider *Prowlarr
}

func NewProwlarrExecutionResolver(provider *Prowlarr) *ProwlarrExecutionResolver {
	return &ProwlarrExecutionResolver{provider: provider}
}

func (*ProwlarrExecutionResolver) ID() string { return ProwlarrProviderID }
func (r *ProwlarrExecutionResolver) Resolve(ctx context.Context, selection Selection) (ExecutionPlan, error) {
	plan := PlanForSelection(selection)
	if r == nil || r.provider == nil || !validCandidate(selection.Candidate) {
		return ExecutionPlan{}, ErrUnresolved
	}
	if _, err := r.provider.executionLocator(ctx, plan); err != nil {
		return ExecutionPlan{}, ErrUnresolved
	}
	return plan, nil
}

func (p *Prowlarr) executionLocator(ctx context.Context, plan ExecutionPlan) (ProwlarrLocator, error) {
	if !p.configured() || plan.ProviderID != ProwlarrProviderID || !localIDPattern.MatchString(plan.JobID) || !localIDPattern.MatchString(string(plan.EditionID)) || !handlePattern.MatchString(plan.Handle) {
		return ProwlarrLocator{}, ErrUnresolved
	}
	record, err := p.Lookup(ctx, plan.ExecutionRef)
	if err != nil || record.CandidateID != plan.CandidateID || record.Protocol != ProwlarrProtocolTorrent {
		return ProwlarrLocator{}, ErrUnresolved
	}
	// One deterministic locator, never fallback after a fetch failure. Lookup
	// already revalidates schema/hash/instance and same-origin/indexer/UrlBase.
	for _, locator := range []ProwlarrLocator{record.Download, record.Magnet} {
		if locator.Route != "" && locator.Link != "" && strings.TrimSpace(locator.File) != "" {
			return locator, nil
		}
	}
	return ProwlarrLocator{}, ErrUnresolved
}

func (p *Prowlarr) fetchTorrent(ctx context.Context, plan ExecutionPlan) (torrentPayload, error) {
	locator, err := p.executionLocator(ctx, plan)
	if err != nil {
		return torrentPayload{}, ErrUnresolved
	}
	u := *p.base
	u.Path = locator.Route
	u.RawQuery = url.Values{"link": {locator.Link}, "file": {locator.File}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return torrentPayload{}, ErrProvider
	}
	request.Header.Set("X-Api-Key", p.config.APIKey)
	request.Header.Set("Accept", "application/x-bittorrent")
	response, err := p.client.Do(request)
	if err != nil {
		return torrentPayload{}, ErrProvider
	}
	defer response.Body.Close()
	// Only the official 301 magnet transport is supported. HTTP redirects,
	// including same-origin redirects, never navigate or select another release.
	if response.StatusCode == http.StatusMovedPermanently {
		return parseMagnet(response.Header.Get("Location"))
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > torrentMaxBytes {
		return torrentPayload{}, ErrInvalidResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, torrentMaxBytes+1))
	if err != nil || len(data) > torrentMaxBytes {
		return torrentPayload{}, ErrInvalidResponse
	}
	return parseTorrent(data)
}
