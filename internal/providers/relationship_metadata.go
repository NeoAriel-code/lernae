package providers

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const relationshipMetadataProvider = "wikidata"

var ErrInvalidRelationshipMetadata = errors.New("invalid normalized relationship metadata")

const MaxRelationshipIdentifiers = 128

// RelationshipLocalizedValue is a normalized provider label or alias.
type RelationshipLocalizedValue struct {
	Language string `json:"language"`
	Value    string `json:"value"`
}

// RelationshipClaim is an allowlisted, parsed provider statement. Property
// retains the source property instead of converting it to stronger semantics.
type RelationshipClaim struct {
	Property          string  `json:"property"`
	SubjectExternalID string  `json:"subject_external_id"`
	ObjectExternalID  string  `json:"object_external_id"`
	Ordinal           *string `json:"ordinal,omitempty"`
}

// RelationshipIdentifier is a parsed allowlisted external identifier. A
// qualifier property is retained only where it gives the identifier its
// required context, as with IGDB P9043 qualified by P5794.
type RelationshipIdentifier struct {
	Property          string `json:"property"`
	QualifierProperty string `json:"qualifier_property,omitempty"`
	Value             string `json:"value"`
}

// LocalProviderIdentity returns the exact provider identity represented by
// this allowlisted Wikidata identifier. It deliberately does not use titles,
// slugs, or unqualified IGDB identifiers.
func (identifier RelationshipIdentifier) LocalProviderIdentity() (provider, externalID string, ok bool) {
	if !validRelationshipIdentifier(identifier) {
		return "", "", false
	}
	switch identifier.Property {
	case "P8600":
		return "tvmaze", identifier.Value, true
	case "P648":
		return "openlibrary", identifier.Value, true
	case "P9043":
		return "igdb", identifier.Value, true
	default:
		return "", "", false
	}
}

// RelationshipMetadata contains normalized claims for one exact provider
// entity. It intentionally has no field for a provider payload or raw JSON.
type RelationshipMetadata struct {
	Provider       string                       `json:"provider"`
	ExternalID     string                       `json:"external_id"`
	EntityRevision string                       `json:"entity_revision"`
	ParserVersion  string                       `json:"parser_version"`
	FetchedAt      time.Time                    `json:"fetched_at"`
	Labels         []RelationshipLocalizedValue `json:"labels,omitempty"`
	Aliases        []RelationshipLocalizedValue `json:"aliases,omitempty"`
	Claims         []RelationshipClaim          `json:"claims,omitempty"`
	Identifiers    []RelationshipIdentifier     `json:"identifiers,omitempty"`
}

// Validate rejects unnormalized, unsupported, or unbounded provider metadata
// before it crosses into the durable relationship cache.
func (metadata RelationshipMetadata) Validate() error {
	if !boundedToken(metadata.Provider, 64) || !boundedToken(metadata.ExternalID, 128) ||
		!boundedToken(metadata.EntityRevision, 128) || !boundedToken(metadata.ParserVersion, 64) ||
		metadata.FetchedAt.IsZero() || metadata.FetchedAt.Year() < 1 || metadata.FetchedAt.Year() > 9999 {
		return fmt.Errorf("%w: provider identity or cache version fields are invalid", ErrInvalidRelationshipMetadata)
	}
	isWikidata := metadata.Provider == relationshipMetadataProvider
	if isWikidata && (!validWikidataEntityID(metadata.ExternalID) || !decimalRevision(metadata.EntityRevision)) {
		return fmt.Errorf("%w: Wikidata entity identity or revision is invalid", ErrInvalidRelationshipMetadata)
	}
	if len(metadata.Labels) > 50 || len(metadata.Aliases) > 250 || len(metadata.Claims) > 256 || len(metadata.Identifiers) > MaxRelationshipIdentifiers {
		return fmt.Errorf("%w: parsed metadata exceeds collection limits", ErrInvalidRelationshipMetadata)
	}
	if len(metadata.Identifiers) > 0 && !isWikidata {
		return fmt.Errorf("%w: provider identifiers are supported only for Wikidata metadata", ErrInvalidRelationshipMetadata)
	}
	for _, values := range [][]RelationshipLocalizedValue{metadata.Labels, metadata.Aliases} {
		for _, value := range values {
			if !validLanguageTag(value.Language) || !normalizedText(value.Value, 512) {
				return fmt.Errorf("%w: localized value is invalid or unnormalized", ErrInvalidRelationshipMetadata)
			}
		}
	}
	for _, claim := range metadata.Claims {
		if claim.SubjectExternalID != metadata.ExternalID || !boundedToken(claim.Property, 32) || !boundedToken(claim.ObjectExternalID, 128) {
			return fmt.Errorf("%w: claim identity is not anchored to the fetched QID", ErrInvalidRelationshipMetadata)
		}
		if isWikidata {
			if !validWikidataEntityID(claim.SubjectExternalID) || !validWikidataEntityID(claim.ObjectExternalID) {
				return fmt.Errorf("%w: Wikidata claim does not use exact QIDs", ErrInvalidRelationshipMetadata)
			}
			switch claim.Property {
			case "P179", "P527":
				if claim.Ordinal != nil && !normalizedOrdinal(*claim.Ordinal) {
					return fmt.Errorf("%w: claim ordinal is invalid", ErrInvalidRelationshipMetadata)
				}
			case "P144", "P155":
				if claim.Ordinal != nil {
					return fmt.Errorf("%w: %s does not carry an ordinal", ErrInvalidRelationshipMetadata, claim.Property)
				}
			default:
				return fmt.Errorf("%w: unsupported Wikidata property %q", ErrInvalidRelationshipMetadata, claim.Property)
			}
		} else if claim.Ordinal != nil && !normalizedOrdinal(*claim.Ordinal) {
			return fmt.Errorf("%w: claim ordinal is invalid", ErrInvalidRelationshipMetadata)
		}
	}
	seenIdentifiers := make(map[RelationshipIdentifier]struct{}, len(metadata.Identifiers))
	for _, identifier := range metadata.Identifiers {
		if !validRelationshipIdentifier(identifier) {
			return fmt.Errorf("%w: provider identifier is unsupported or malformed", ErrInvalidRelationshipMetadata)
		}
		if _, exists := seenIdentifiers[identifier]; exists {
			return fmt.Errorf("%w: provider identifiers contain duplicates", ErrInvalidRelationshipMetadata)
		}
		seenIdentifiers[identifier] = struct{}{}
	}
	return nil
}

func validRelationshipIdentifier(identifier RelationshipIdentifier) bool {
	switch identifier.Property {
	case "P8600":
		return identifier.QualifierProperty == "" && positiveDecimalIdentifier(identifier.Value)
	case "P648":
		return identifier.QualifierProperty == "" && openLibraryWorkIdentifier(identifier.Value)
	case "P9043":
		return identifier.QualifierProperty == "P5794" && positiveDecimalIdentifier(identifier.Value)
	default:
		return false
	}
}

func positiveDecimalIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 32 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func openLibraryWorkIdentifier(value string) bool {
	if len(value) < 4 || len(value) > 32 || !strings.HasPrefix(value, "OL") || value[len(value)-1] != 'W' {
		return false
	}
	number := value[2 : len(value)-1]
	if number == "" || number[0] < '1' || number[0] > '9' {
		return false
	}
	for _, character := range number {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validWikidataEntityID(value string) bool {
	if len(value) < 2 || len(value) > 20 || value[0] != 'Q' || value[1] == '0' {
		return false
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func decimalRevision(value string) bool {
	if value == "" || len(value) > 20 || value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func boundedToken(value string, max int) bool {
	if strings.TrimSpace(value) != value || value == "" || len(value) > max {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func validLanguageTag(value string) bool {
	if len(value) < 2 || len(value) > 35 || strings.TrimSpace(value) != value {
		return false
	}
	for index, part := range strings.Split(value, "-") {
		if len(part) == 0 || len(part) > 8 || (index == 0 && len(part) < 2) {
			return false
		}
		for _, character := range part {
			isLetter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
			isDigit := character >= '0' && character <= '9'
			if !isLetter && !(index > 0 && isDigit) {
				return false
			}
		}
	}
	return true
}

func normalizedText(value string, max int) bool {
	return value != "" && len(value) <= max && strings.TrimSpace(value) == value && strings.Join(strings.Fields(value), " ") == value
}

func normalizedOrdinal(value string) bool {
	return value != "" && len(value) <= 64 && strings.TrimSpace(value) != ""
}
