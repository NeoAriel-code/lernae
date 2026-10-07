// Package domain contains the small, persistence-independent vocabulary shared
// by the Foundation contracts.
package domain

import (
	"math"
	"time"
)

// Identifiers are opaque within the domain; repository boundaries own their
// durable storage representation.
type (
	UniverseID          string
	UniverseOperationID string
	WorkID              string
	WorkCollectionID    string
	EditionID           string
	AssetID             string
	AssetPartID         string
	AssetLocationID     string
	ExternalIdentityID  string
	SessionID           string
)

// Medium is the broad medium of a Work. Edition carries concrete platform or
// format differences; WorkType remains open-ended by design.
type Medium string

const (
	MediumGame       Medium = "game"
	MediumVideo      Medium = "video"
	MediumLiterature Medium = "literature"
	MediumAudio      Medium = "audio"
)

// Valid reports whether m is one of Foundation's broad Work media.
func (m Medium) Valid() bool {
	switch m {
	case MediumGame, MediumVideo, MediumLiterature, MediumAudio:
		return true
	default:
		return false
	}
}

type Universe struct {
	ID                    UniverseID
	Title                 string
	SortTitle             *string
	Description           *string
	Artwork               *string
	ExistenceConfidence   float64
	NamingConfidence      float64
	NamingConfidenceLevel UniverseNamingConfidenceLevel
	NamingEvidence        UniverseNamingEvidence
	Provenance            UniverseProvenance
	ConfirmedByUser       bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type UniverseNamingConfidenceLevel string

const (
	UniverseNamingConfidenceNone     UniverseNamingConfidenceLevel = "none"
	UniverseNamingConfidenceModerate UniverseNamingConfidenceLevel = "moderate"
	UniverseNamingConfidenceHigh     UniverseNamingConfidenceLevel = "high"
)

type UniverseNamingEvidence string

const (
	UniverseNamingEvidenceConfirmedTitle   UniverseNamingEvidence = "confirmed_universe_title"
	UniverseNamingEvidenceConfirmedAlias   UniverseNamingEvidence = "confirmed_local_alias"
	UniverseNamingEvidenceDistinctiveQuery UniverseNamingEvidence = "distinctive_normalized_query"
	UniverseNamingEvidenceSharedAnchor     UniverseNamingEvidence = "shared_high_confidence_anchor"
	UniverseNamingEvidenceUserAuthored     UniverseNamingEvidence = "user_authored_title"
	UniverseNamingEvidenceLegacyUnknown    UniverseNamingEvidence = "legacy_unknown"
)

// UniverseProvenance distinguishes a user-authored Universe from an
// automatically discovered one. Confirmation is stored independently.
type UniverseProvenance string

const (
	UniverseProvenanceManual    UniverseProvenance = "manual"
	UniverseProvenanceAutomatic UniverseProvenance = "automatic"
)

func (provenance UniverseProvenance) Valid() bool {
	return provenance == UniverseProvenanceManual || provenance == UniverseProvenanceAutomatic
}

// UniverseMembershipProvenance is the bounded source category for an accepted
// relationship. User confirmation is stored independently on the membership.
type UniverseMembershipProvenance string

const (
	UniverseMembershipProvenanceProvider  UniverseMembershipProvenance = "provider"
	UniverseMembershipProvenanceRule      UniverseMembershipProvenance = "rule"
	UniverseMembershipProvenanceManual    UniverseMembershipProvenance = "manual"
	UniverseMembershipProvenanceAutomatic UniverseMembershipProvenance = "automatic"
)

// Valid reports whether the provenance is one of the persisted source kinds.
func (p UniverseMembershipProvenance) Valid() bool {
	switch p {
	case UniverseMembershipProvenanceProvider, UniverseMembershipProvenanceRule, UniverseMembershipProvenanceManual, UniverseMembershipProvenanceAutomatic:
		return true
	default:
		return false
	}
}

// UniverseMembershipStatus distinguishes accepted relationships from
// preserved automatic candidates that need human review.
type UniverseMembershipStatus string

const (
	UniverseMembershipStatusAccepted     UniverseMembershipStatus = "accepted"
	UniverseMembershipStatusReviewNeeded UniverseMembershipStatus = "review_needed"
)

func (status UniverseMembershipStatus) Valid() bool {
	return status == UniverseMembershipStatusAccepted || status == UniverseMembershipStatusReviewNeeded
}

type Work struct {
	ID       WorkID
	Title    string
	Summary  string
	Medium   Medium
	WorkType string
}

// WorkMaterialization is the allowlisted provider-neutral input required to
// persist a Work. Provider payloads and acquisition metadata do not cross this
// boundary.
type WorkMaterialization struct {
	Provider   string
	ExternalID string
	Title      string
	Summary    string
	Medium     Medium
	WorkType   string
}

// UniverseMembership preserves one Work-to-Universe relationship. Rejected
// candidates are represented by UniverseExclusion instead.
type UniverseMembership struct {
	UniverseID      UniverseID
	WorkID          WorkID
	Status          UniverseMembershipStatus
	Provenance      UniverseMembershipProvenance
	Confidence      float64
	Evidence        string
	Reason          string
	ConfirmedByUser bool
	AcceptedAt      time.Time
}

// UniverseAlias is a persistent normalized localized alternate name for a Universe.
type UniverseAlias struct {
	UniverseID      UniverseID
	Alias           string
	Language        *string
	Provenance      string
	ProviderVersion *string
	Confidence      float64
	ConfirmedByUser bool
	CreatedAt       time.Time
}

// WorkCollection is a provider-neutral grouping identity. A Series is a
// collection, not a Universe: Works may belong to more than one Series.
type WorkCollection struct {
	ID    WorkCollectionID
	Title string
	Type  WorkCollectionType
}

type WorkCollectionType string

const WorkCollectionTypeSeries WorkCollectionType = "series"

func (kind WorkCollectionType) Valid() bool {
	return kind == WorkCollectionTypeSeries
}

// WorkRelationType names an explicit typed relationship. These values are
// descriptive metadata, never inferred from titles.
type WorkRelationType string

const (
	WorkRelationPartOfSeries    WorkRelationType = "part_of_series"
	WorkRelationAdaptationOf    WorkRelationType = "adaptation_of"
	WorkRelationSequelOf        WorkRelationType = "sequel_of"
	WorkRelationFollows         WorkRelationType = "follows"
	WorkRelationSpinOffOf       WorkRelationType = "spin_off_of"
	WorkRelationCompanionTo     WorkRelationType = "companion_to"
	WorkRelationFranchiseMember WorkRelationType = "franchise_member"
)

func (kind WorkRelationType) Valid() bool {
	switch kind {
	case WorkRelationPartOfSeries, WorkRelationAdaptationOf, WorkRelationSequelOf,
		WorkRelationFollows, WorkRelationSpinOffOf, WorkRelationCompanionTo,
		WorkRelationFranchiseMember:
		return true
	default:
		return false
	}
}

// WorkRelationProvenance identifies the authority behind one explicit edge.
type WorkRelationProvenance string

const (
	WorkRelationProvenanceProvider  WorkRelationProvenance = "provider"
	WorkRelationProvenanceRule      WorkRelationProvenance = "rule"
	WorkRelationProvenanceManual    WorkRelationProvenance = "manual"
	WorkRelationProvenanceAutomatic WorkRelationProvenance = "automatic"
)

func (provenance WorkRelationProvenance) Valid() bool {
	switch provenance {
	case WorkRelationProvenanceProvider, WorkRelationProvenanceRule,
		WorkRelationProvenanceManual, WorkRelationProvenanceAutomatic:
		return true
	default:
		return false
	}
}

// WorkRelation links a source Work to exactly one Work or one collection.
// Ordinal remains textual so provider sequence forms such as "5.0" survive.
type WorkRelation struct {
	WorkID             WorkID
	Type               WorkRelationType
	TargetWorkID       *WorkID
	TargetCollectionID *WorkCollectionID
	Ordinal            *string
	Provenance         WorkRelationProvenance
	ProviderVersion    *string
	Confidence         float64
	Evidence           string
	ConfirmedByUser    bool
}

// Valid checks the provider-neutral relationship contract without relying on
// display titles or provider payloads.
func (relation WorkRelation) Valid() bool {
	if relation.WorkID == "" || !relation.Type.Valid() || !relation.Provenance.Valid() ||
		math.IsNaN(relation.Confidence) || math.IsInf(relation.Confidence, 0) ||
		relation.Confidence < 0 || relation.Confidence > 1 {
		return false
	}
	hasWorkTarget := relation.TargetWorkID != nil && *relation.TargetWorkID != ""
	hasCollectionTarget := relation.TargetCollectionID != nil && *relation.TargetCollectionID != ""
	if hasWorkTarget == hasCollectionTarget {
		return false
	}
	if relation.Type == WorkRelationPartOfSeries && !hasCollectionTarget {
		return false
	}
	return relation.Ordinal == nil || *relation.Ordinal != ""
}

// UniverseExclusion is a durable decision not to associate a Work with a
// Universe. It is intentionally separate from accepted membership.
type UniverseExclusion struct {
	UniverseID UniverseID
	WorkID     WorkID
	Reason     string
	RecordedAt time.Time
}

// Platform is the normalized identity of a concrete game platform. Provider
// payload identifiers remain inside the provider adapter.
type Platform struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// MetadataSearchResult is a provider-neutral search hit that can be displayed
// or explicitly bound to a catalog Work later.
type MetadataSearchResult struct {
	Provider              string                 `json:"provider"`
	ExternalID            string                 `json:"external_id"`
	Title                 string                 `json:"title"`
	Aliases               []string               `json:"aliases,omitempty"`
	Subtitle              string                 `json:"subtitle,omitempty"`
	Creators              []string               `json:"creators,omitempty"`
	MediaType             string                 `json:"media_type"`
	Year                  int                    `json:"year,omitempty"`
	Network               string                 `json:"network,omitempty"`
	WebChannel            string                 `json:"web_channel,omitempty"`
	ArtworkURL            string                 `json:"artwork_url,omitempty"`
	SourceURL             string                 `json:"source_url,omitempty"`
	SourceRank            int                    `json:"source_rank"`
	Relevance             SearchRelevance        `json:"relevance,omitempty"`
	Score                 *float64               `json:"score,omitempty"`
	ReleaseDate           *time.Time             `json:"release_date,omitempty"`
	ReleaseYear           int                    `json:"release_year,omitempty"`
	Summary               string                 `json:"summary,omitempty"`
	CoverReference        string                 `json:"cover_reference,omitempty"`
	RepresentativeEdition *RepresentativeEdition `json:"representative_edition,omitempty"`
	Platforms             []Platform             `json:"platforms,omitempty"`
	Medium                Medium                 `json:"medium"`
	WorkType              string                 `json:"work_type"`
}

// SearchRelevance is Lernae's bounded, provider-neutral relevance category.
// It is based on title evidence, not provider score scales.
type SearchRelevance string

const (
	SearchRelevanceBest    SearchRelevance = "best"
	SearchRelevanceRelated SearchRelevance = "related"
	SearchRelevanceWeak    SearchRelevance = "weak"
)

// SearchSourceState is the safe, provider-neutral outcome of one search call.
type SearchSourceState string

const (
	SearchSourceFresh       SearchSourceState = "fresh"
	SearchSourceAvailable   SearchSourceState = SearchSourceFresh
	SearchSourceEmpty       SearchSourceState = "empty"
	SearchSourceThrottled   SearchSourceState = "throttled"
	SearchSourceUnavailable SearchSourceState = "unavailable"
	SearchSourceStale       SearchSourceState = "stale"
)

// SearchSourceStatus describes one provider without exposing its response body
// or implementation-specific error text.
type SearchSourceStatus struct {
	Provider    string            `json:"provider"`
	State       SearchSourceState `json:"state"`
	ResultCount int               `json:"result_count"`
	ErrorCode   string            `json:"error_code,omitempty"`
}

// SearchResponse is the versioned shape returned by the universal search API.
type SearchResponse struct {
	Results           []MetadataSearchResult   `json:"results"`
	Sources           []SearchSourceStatus     `json:"sources"`
	UniverseDiscovery *UniverseDiscoveryResult `json:"universe_discovery,omitempty"`
}

type UniverseDiscoveryState string

const (
	UniverseDiscoveryNotEligible UniverseDiscoveryState = "not_eligible"
	UniverseDiscoveryCreated     UniverseDiscoveryState = "created"
	UniverseDiscoveryReused      UniverseDiscoveryState = "reused"
	UniverseDiscoveryAmbiguous   UniverseDiscoveryState = "ambiguous"
)

// UniverseDiscoveryResult reports group-level discovery without collapsing
// its confidence into the per-Work membership confidence values.
type UniverseDiscoveryResult struct {
	State                 UniverseDiscoveryState        `json:"state"`
	UniverseID            UniverseID                    `json:"universe_id,omitempty"`
	Title                 string                        `json:"title,omitempty"`
	ExistenceConfidence   float64                       `json:"existence_confidence,omitempty"`
	NamingConfidence      float64                       `json:"naming_confidence,omitempty"`
	NamingConfidenceLevel UniverseNamingConfidenceLevel `json:"naming_confidence_level,omitempty"`
	NamingEvidence        UniverseNamingEvidence        `json:"naming_evidence,omitempty"`
	MembershipsAdded      int                           `json:"memberships_added"`
	Provenance            UniverseProvenance            `json:"provenance,omitempty"`
	ConfirmedByUser       bool                          `json:"confirmed_by_user"`
}

// WorkDetails is provider-neutral canonical metadata loaded on demand after a
// search result is opened. ExternalID identifies the Work, not a local
// catalog row or one of its edition-specific records.
type WorkDetails struct {
	Provider               string                 `json:"provider"`
	ExternalID             string                 `json:"external_id"`
	Title                  string                 `json:"title"`
	ReleaseDate            *time.Time             `json:"release_date,omitempty"`
	ReleaseYear            int                    `json:"release_year,omitempty"`
	Summary                string                 `json:"summary,omitempty"`
	CoverReference         string                 `json:"cover_reference,omitempty"`
	FallbackCoverReference string                 `json:"fallback_cover_reference,omitempty"`
	SourceURL              string                 `json:"source_url,omitempty"`
	Language               string                 `json:"language,omitempty"`
	Status                 string                 `json:"status,omitempty"`
	Genres                 []string               `json:"genres,omitempty"`
	Network                string                 `json:"network,omitempty"`
	WebChannel             string                 `json:"web_channel,omitempty"`
	RepresentativeEdition  *RepresentativeEdition `json:"representative_edition,omitempty"`
	Medium                 Medium                 `json:"medium"`
	WorkType               string                 `json:"work_type"`
	PlatformCandidates     []PlatformCandidate    `json:"platform_candidates"`
	Series                 []WorkSeriesDetails    `json:"series,omitempty"`
	SeriesTruncated        bool                   `json:"series_truncated,omitempty"`
	SeasonCount            *int                   `json:"season_count,omitempty"`
}

// WorkSeriesDetails presents one explicit part_of_series collection for the
// exact Work identity opened by a Details request. CollectionID is a local,
// opaque identity; Ordinal and KnownTotal remain nullable when not known.
type WorkSeriesDetails struct {
	CollectionID          WorkCollectionID   `json:"collection_id"`
	Title                 string             `json:"title"`
	Ordinal               *string            `json:"ordinal"`
	KnownTotal            *int               `json:"known_total"`
	MoreInSeries          []WorkSeriesMember `json:"more_in_series"`
	MoreInSeriesTruncated bool               `json:"more_in_series_truncated"`
}

// WorkSeriesMember is a safe display record for one exact external Work
// identity in an ordered series. It excludes same-author recommendations.
type WorkSeriesMember struct {
	Provider   string  `json:"provider"`
	ExternalID string  `json:"external_id"`
	Title      string  `json:"title"`
	Ordinal    *string `json:"ordinal"`
}

// RepresentativeEdition contains normalized presentation metadata for one
// provider-selected edition. ExternalID is not the canonical Work identity.
type RepresentativeEdition struct {
	ExternalID      string `json:"external_id,omitempty"`
	Title           string `json:"title,omitempty"`
	Language        string `json:"language,omitempty"`
	PublicationDate string `json:"publication_date,omitempty"`
	CoverReference  string `json:"cover_reference,omitempty"`
}

// PlatformCandidate is a deduplicated edition/platform choice with the
// provider records that supplied evidence for it.
type PlatformCandidate struct {
	Platform   Platform             `json:"platform"`
	Provenance []PlatformProvenance `json:"provenance"`
}

// PlatformProvenance identifies a provider record and relationship that
// contributed one platform candidate.
type PlatformProvenance struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Relation   string `json:"relation"`
}

const (
	PlatformRelationCanonicalWork   = "canonical_work"
	PlatformRelationParentGameChild = "parent_game_child"
)

type Edition struct {
	ID       EditionID
	WorkID   WorkID
	Label    string
	Platform string
	Region   string
	Format   string
}

// Asset is the complete consumable/preservable unit. AssetPart represents its
// physical member files; neither type implies that any path is launchable.
type Asset struct {
	ID             AssetID
	EditionID      EditionID
	Kind           string
	TotalSizeBytes int64
}

type AssetPart struct {
	ID           AssetPartID
	AssetID      AssetID
	PartIndex    *int
	Role         string
	Filename     string
	RelativePath string
	SizeBytes    int64
}

type AssetLocation struct {
	ID                AssetLocationID
	AssetID           AssetID
	StorageProviderID string
	Locator           string
	LocationClass     string
}

// SessionOutcome is the bounded terminal result of one confirmed launch.
type SessionOutcome string

const (
	SessionOutcomeNormalExit  SessionOutcome = "normal_exit"
	SessionOutcomeNonZeroExit SessionOutcome = "nonzero_exit"
	SessionOutcomeInterrupted SessionOutcome = "interrupted"
)

// Session records one confirmed play-process lifetime. An empty Outcome and
// nil EndedAt together represent an active Session.
type Session struct {
	ID        SessionID
	WorkID    WorkID
	EditionID EditionID
	AssetID   AssetID
	StartedAt time.Time
	EndedAt   *time.Time
	Outcome   SessionOutcome
}

// ExternalIdentity links a Work to one stable external provider record.
type ExternalIdentity struct {
	ID         ExternalIdentityID
	WorkID     WorkID
	Provider   string
	ExternalID string
}
