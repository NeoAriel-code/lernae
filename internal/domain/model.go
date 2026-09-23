// Package domain contains the small, persistence-independent vocabulary shared
// by the Foundation contracts.
package domain

// Identifiers are opaque within the domain. Their storage representation is
// intentionally left to a later phase.
type (
	UniverseID  string
	WorkID      string
	EditionID   string
	AssetID     string
	AssetPartID string
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
	ID    UniverseID
	Title string
}

type Work struct {
	ID         WorkID
	UniverseID UniverseID
	Title      string
	Medium     Medium
	WorkType   string
}

type Edition struct {
	ID               EditionID
	WorkID           WorkID
	Label            string
	PlatformOrFormat string
}

// Asset is the complete consumable/preservable unit. AssetPart represents its
// physical member files; neither type implies that any path is launchable.
type Asset struct {
	ID        AssetID
	EditionID EditionID
	Kind      string
}

type AssetPart struct {
	ID           AssetPartID
	AssetID      AssetID
	PartIndex    *int
	Role         string
	Filename     string
	RelativePath string
}

type AssetLocation struct {
	AssetID           AssetID
	StorageProviderID string
	Locator           string
	LocationClass     string
}
