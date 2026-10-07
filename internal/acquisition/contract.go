// Package acquisition discovers possibilities, records a manual exact choice,
// and orchestrates typed execution acceptance. It has no inventory or Asset authority.
package acquisition

import (
	"context"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/domain"
)

// Error is an allowlisted category. Raw provider/infrastructure errors are never
// wrapped into these values, persisted, or returned to callers.
type Error string

func (e Error) Error() string { return string(e) }

const (
	ErrInvalidID        Error = "invalid_id"
	ErrRegistry         Error = "invalid_registry"
	ErrNotFound         Error = "acquisition_not_found"
	ErrNotQueued        Error = "acquisition_not_queued"
	ErrNoSelection      Error = "selection_not_found"
	ErrUnknownCandidate Error = "unknown_candidate"
	ErrSnapshotExpired  Error = "rediscovery_required"
	ErrConflict         Error = "selection_conflict"
	ErrUnavailable      Error = "acquisition_unavailable"
	ErrCancelled        Error = "request_cancelled"
	ErrTimeout          Error = "provider_timeout"
	ErrProvider         Error = "provider_unavailable"
	ErrInvalidResponse  Error = "invalid_provider_response"
)

const (
	MaxProviders          = 16
	MaxProviderCandidates = 32
	MaxExecutionRefBytes  = 512
	maxSnapshots          = 128
	snapshotTTL           = 5 * time.Minute
	providerTimeout       = 2 * time.Second
)

// Target reuses catalog identity and context. Work is derived from Edition,
// never persisted again by acquisition. Providers must treat it as read-only.
type Target struct {
	Edition domain.Edition
	Work    domain.Work
}

// Provider implementations MUST return when ctx is cancelled/deadlined, and
// MUST join any work they start before returning. Eligibility is local, cheap,
// side-effect-free, and must not perform I/O. Discovery may run concurrently.
// Core cannot forcibly stop a Go implementation that violates this contract.
type Provider interface {
	ID() string
	Eligible(Target) bool
	Discover(context.Context, Target) ([]Option, error)
}

// ContentVerificationTimeouts is an optional capability contract for adapters
// that perform bounded remote discovery or verify actual content during discovery,
// resolution, or dispatch preflight. The historical name retains compatibility.
// It changes only these call deadlines, never accepted Execution.Wait, persistence,
// or lifecycle ownership. Zero fields retain the generic two-second default.
// Core accepts nonzero fields only in [2s, 10m]; an invalid policy falls back to
// generic deadlines in its entirety. Adapters must still honor cancellation/join.
type ContentVerificationTimeouts interface {
	ContentVerificationTimeouts() ContentVerificationPolicy
}

// ContentVerificationPolicy contains only typed time budgets, not provider data.
type ContentVerificationPolicy struct {
	Discovery  time.Duration
	Resolution time.Duration
	Dispatch   time.Duration
}

// Metadata is presentation only, not protocol data or arbitrary descriptions.
// Adapters are responsible for supplying non-sensitive human-readable text;
// validation cannot determine whether arbitrary ordinary text is a secret.
type Metadata struct {
	Title    string
	Label    string
	Language string
}

// Option is provider-local identity plus a bounded opaque lookup token. A token
// must reference provider-owned execution state, not encode a URL, credentials,
// or a raw payload. It is private even when JSON is used accidentally.
type Option struct {
	ID           string
	Metadata     Metadata
	ExecutionRef string `json:"-"`
}

type Candidate struct {
	JobID      string
	EditionID  domain.EditionID
	ProviderID string
	Handle     string
	Option     Option
}

type Selection struct {
	Candidate  Candidate
	SelectedAt time.Time
}

type Failure struct {
	ProviderID string
	Category   Error
}

type Outcome string

const (
	OutcomeUnconfigured Outcome = "no_providers_configured"
	OutcomeIneligible   Outcome = "no_eligible_providers"
	OutcomeEmpty        Outcome = "empty"
	OutcomeAvailable    Outcome = "available"
	OutcomePartial      Outcome = "partial_failure"
	OutcomeFailed       Outcome = "providers_failed"
)

type Discovery struct {
	JobID      string
	EditionID  domain.EditionID
	Outcome    Outcome
	Candidates []Candidate
	Failures   []Failure
}

type registeredProvider struct {
	id       string
	provider Provider
}

// Registry freezes validated stable IDs at explicit Server composition. It is
// immutable; zero providers is valid. Duplicate IDs are a configuration error.
type Registry struct {
	entries []registeredProvider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	if len(providers) > MaxProviders {
		return nil, ErrRegistry
	}
	registry := &Registry{}
	seen := make(map[string]bool)
	for _, provider := range providers {
		if nilProvider(provider) {
			return nil, ErrRegistry
		}
		id := provider.ID()
		if !providerIDPattern.MatchString(id) || seen[id] {
			return nil, ErrRegistry
		}
		seen[id] = true
		registry.entries = append(registry.entries, registeredProvider{id, provider})
	}
	sort.Slice(registry.entries, func(i, j int) bool {
		return registry.entries[i].id < registry.entries[j].id
	})
	return registry, nil
}

func nilProvider(provider Provider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (r *Registry) Lookup(id string) (Provider, bool) {
	if r != nil {
		for _, entry := range r.entries {
			if entry.id == id {
				return entry.provider, true
			}
		}
	}
	return nil, false
}

var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	localIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	refPattern        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)
	handlePattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	languagePattern   = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)
	urlPattern        = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*:[^\s]|www\.|//)`)
)

func validText(value string, maxBytes int, required bool) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) || (required && value == "") ||
		strings.TrimSpace(value) != value || urlPattern.MatchString(value) {
		return false
	}
	meaningful := value == "" && !required
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			meaningful = true
		}
	}
	return meaningful
}

func validOption(option Option) bool {
	return localIDPattern.MatchString(option.ID) && refPattern.MatchString(option.ExecutionRef) &&
		validText(option.Metadata.Title, 256, true) && validText(option.Metadata.Label, 128, false) &&
		(option.Metadata.Language == "" || (len(option.Metadata.Language) <= 35 && languagePattern.MatchString(option.Metadata.Language)))
}
