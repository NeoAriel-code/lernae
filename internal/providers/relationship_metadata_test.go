package providers

import (
	"testing"
	"time"
)

func TestRelationshipMetadataAcceptsOnlyNormalizedAllowlistedClaims(t *testing.T) {
	ordinal := "1b"
	metadata := RelationshipMetadata{
		Provider:       "wikidata",
		ExternalID:     "Q42",
		EntityRevision: "1234",
		ParserVersion:  "wikidata-relationship-v1",
		FetchedAt:      time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Labels:         []RelationshipLocalizedValue{{Language: "en", Value: "Example Series"}},
		Aliases:        []RelationshipLocalizedValue{{Language: "es", Value: "Serie de ejemplo"}},
		Claims: []RelationshipClaim{{
			Property: "P179", SubjectExternalID: "Q42", ObjectExternalID: "Q10", Ordinal: &ordinal,
		}},
		Identifiers: []RelationshipIdentifier{
			{Property: "P8600", Value: "333"},
			{Property: "P648", Value: "OL123W"},
			{Property: "P9043", QualifierProperty: "P5794", Value: "1565"},
		},
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("valid parsed relationship metadata rejected: %v", err)
	}
	if got := *metadata.Claims[0].Ordinal; got != "1b" {
		t.Fatalf("textual ordinal = %q, want provider text preserved", got)
	}
	for _, test := range []struct {
		identifier   RelationshipIdentifier
		wantProvider string
		wantID       string
	}{
		{identifier: metadata.Identifiers[0], wantProvider: "tvmaze", wantID: "333"},
		{identifier: metadata.Identifiers[1], wantProvider: "openlibrary", wantID: "OL123W"},
		{identifier: metadata.Identifiers[2], wantProvider: "igdb", wantID: "1565"},
	} {
		provider, externalID, ok := test.identifier.LocalProviderIdentity()
		if !ok || provider != test.wantProvider || externalID != test.wantID {
			t.Errorf("identifier %#v local identity = %q:%q, %t; want %q:%q", test.identifier, provider, externalID, ok, test.wantProvider, test.wantID)
		}
	}

	metadata.Claims[0].Property = "P155"
	if err := metadata.Validate(); err == nil {
		t.Fatal("unsupported P155 relationship passed validation")
	}
}

func TestRelationshipMetadataRejectsUnsafeProviderIdentifiers(t *testing.T) {
	base := RelationshipMetadata{
		Provider: "wikidata", ExternalID: "Q42", EntityRevision: "1234",
		ParserVersion: "wikidata-relationship-v2", FetchedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name       string
		identifier RelationshipIdentifier
	}{
		{name: "TVMaze IDs must be positive decimal", identifier: RelationshipIdentifier{Property: "P8600", Value: "0"}},
		{name: "TVMaze IDs reject nonnumeric text", identifier: RelationshipIdentifier{Property: "P8600", Value: "slug-333"}},
		{name: "Open Library Work IDs only", identifier: RelationshipIdentifier{Property: "P648", Value: "OL123M"}},
		{name: "Open Library rejects edition IDs", identifier: RelationshipIdentifier{Property: "P648", Value: "OL123A"}},
		{name: "IGDB requires P5794 qualifier context", identifier: RelationshipIdentifier{Property: "P9043", Value: "1565"}},
		{name: "IGDB qualifier must be P5794", identifier: RelationshipIdentifier{Property: "P9043", QualifierProperty: "P9999", Value: "1565"}},
		{name: "unknown property rejected", identifier: RelationshipIdentifier{Property: "P9999", Value: "1565"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := base
			metadata.Identifiers = []RelationshipIdentifier{test.identifier}
			if err := metadata.Validate(); err == nil {
				t.Fatalf("Validate() accepted unsafe provider identifier %#v", test.identifier)
			}
		})
	}
}
