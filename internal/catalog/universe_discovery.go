package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"lernae/internal/domain"
	"lernae/internal/universe"
)

// AutoDiscover atomically creates or safely reuses one high-confidence local
// Universe and its exact-identity memberships. It never fetches provider data.
func (application *UniverseApplication) AutoDiscover(
	ctx context.Context,
	query string,
	results []domain.MetadataSearchResult,
) (domain.UniverseDiscoveryResult, error) {
	if err := validateBoundedText("Universe discovery query", query, 200); err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	if len(results) > universe.MaxCandidateResults {
		return domain.UniverseDiscoveryResult{}, ErrUniverseResolveBoundsExceeded
	}
	candidate, eligible := universe.AssessAutomaticDiscovery(query, results)
	if !eligible {
		return application.findExistingLocalDiscovery(ctx, query)
	}
	if err := ctx.Err(); err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, fmt.Errorf("begin automatic Universe discovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	states, err := loadDiscoveryUniversesTx(ctx, tx)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	target, existing, ambiguous, err := findDiscoveryUniverseTx(ctx, tx, candidate.QueryKey, states)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	if ambiguous {
		return domain.UniverseDiscoveryResult{State: domain.UniverseDiscoveryAmbiguous}, nil
	}

	eligibleResults, err := filterDiscoveryResultsTx(ctx, tx, results, target.ID)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	candidate, eligible = universe.AssessAutomaticDiscovery(query, eligibleResults)
	if !eligible {
		return domain.UniverseDiscoveryResult{State: domain.UniverseDiscoveryNotEligible}, nil
	}

	state := domain.UniverseDiscoveryReused
	now := time.Now().UTC()
	if !existing {
		state = domain.UniverseDiscoveryCreated
		universeRecord, err := normalizeCreateUniverseMetadata(domain.Universe{
			Title: candidate.Title, ExistenceConfidence: candidate.ExistenceConfidence,
			NamingConfidence: candidate.NamingConfidence, NamingConfidenceLevel: domain.UniverseNamingConfidenceLevel(candidate.NamingConfidenceLevel),
			NamingEvidence: candidate.NamingEvidence,
			Provenance:     domain.UniverseProvenanceAutomatic, ConfirmedByUser: false,
		})
		if err != nil {
			return domain.UniverseDiscoveryResult{}, err
		}
		universeRecord.ID, err = newLocalUniverseID()
		if err != nil {
			return domain.UniverseDiscoveryResult{}, err
		}
		universeRecord.CreatedAt, universeRecord.UpdatedAt = now, now
		if _, err := tx.ExecContext(ctx, `INSERT INTO universes
			(id, title, sort_title, description, artwork, created_at_utc, updated_at_utc,
			existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user)
			VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
			universeRecord.ID, universeRecord.Title, universeRecord.SortTitle, universeRecord.Description,
			formatUniverseTime(now), formatUniverseTime(now), universeRecord.ExistenceConfidence,
			universeRecord.NamingConfidence, universeRecord.NamingConfidenceLevel, universeRecord.NamingEvidence,
			universeRecord.Provenance, universeRecord.ConfirmedByUser); err != nil {
			return domain.UniverseDiscoveryResult{}, fmt.Errorf("create automatically discovered Universe: %w", err)
		}
		target = universeRecord
	} else if target.Provenance == domain.UniverseProvenanceAutomatic {
		existenceConfidence := target.ExistenceConfidence
		if candidate.ExistenceConfidence > existenceConfidence {
			existenceConfidence = candidate.ExistenceConfidence
		}
		if legacyUnknownAutomaticName(target) && candidate.NamingConfidence > 0 &&
			candidate.NamingConfidenceLevel != universe.ConfidenceNone && candidate.NamingEvidence != domain.UniverseNamingEvidenceLegacyUnknown {
			if _, err := tx.ExecContext(ctx, `UPDATE universes SET title = ?, existence_confidence = ?,
				naming_confidence = ?, naming_confidence_level = ?, naming_evidence = ?, updated_at_utc = ? WHERE id = ?`,
				candidate.Title, existenceConfidence, candidate.NamingConfidence, candidate.NamingConfidenceLevel,
				candidate.NamingEvidence, formatUniverseTime(now), target.ID); err != nil {
				return domain.UniverseDiscoveryResult{}, fmt.Errorf("refresh legacy automatic Universe naming evidence: %w", err)
			}
			target.Title = candidate.Title
			target.NamingConfidence = candidate.NamingConfidence
			target.NamingConfidenceLevel = domain.UniverseNamingConfidenceLevel(candidate.NamingConfidenceLevel)
			target.NamingEvidence = candidate.NamingEvidence
			target.ExistenceConfidence = existenceConfidence
		} else if existenceConfidence != target.ExistenceConfidence {
			if _, err := tx.ExecContext(ctx, `UPDATE universes SET existence_confidence = ?, updated_at_utc = ? WHERE id = ?`,
				existenceConfidence, formatUniverseTime(now), target.ID); err != nil {
				return domain.UniverseDiscoveryResult{}, fmt.Errorf("update automatic Universe existence confidence: %w", err)
			}
			target.ExistenceConfidence = existenceConfidence
		}
	}

	added := 0
	for _, proposed := range candidate.Memberships {
		workID, err := materializeExternalWorkTx(ctx, tx, proposed.Work)
		if err != nil {
			return domain.UniverseDiscoveryResult{}, err
		}
		_, hasCurrent, err := membershipForWorkTx(ctx, tx, workID)
		if err != nil {
			return domain.UniverseDiscoveryResult{}, err
		}
		if hasCurrent {
			// Existing membership provenance and user decisions remain authoritative.
			continue
		}
		if proposed.ConfidenceLevel != universe.ConfidenceHigh || proposed.Confidence < 0.85 {
			continue
		}
		excluded, err := isUniverseWorkExcludedTx(ctx, tx, workID, target.ID)
		if err != nil {
			return domain.UniverseDiscoveryResult{}, err
		}
		if excluded {
			continue
		}
		saved, err := setUniverseMembershipTx(ctx, tx, domain.UniverseMembership{
			UniverseID: target.ID, WorkID: workID,
			Provenance: domain.UniverseMembershipProvenanceAutomatic,
			Confidence: proposed.Confidence, Evidence: string(proposed.Evidence), Reason: proposed.Reason, ConfirmedByUser: false,
		}, now)
		if err != nil {
			return domain.UniverseDiscoveryResult{}, err
		}
		if saved.UniverseID == target.ID && saved.Provenance == domain.UniverseMembershipProvenanceAutomatic && !saved.ConfirmedByUser {
			added++
		}
	}

	inserted, err := tx.ExecContext(ctx, `INSERT INTO universe_discovery_keys (normalized_key, universe_id, created_at_utc)
		VALUES (?, ?, ?) ON CONFLICT(normalized_key) DO NOTHING`, candidate.QueryKey, target.ID, formatUniverseTime(now))
	if err != nil {
		return domain.UniverseDiscoveryResult{}, fmt.Errorf("persist normalized Universe discovery key: %w", err)
	}
	insertedCount, err := inserted.RowsAffected()
	if err != nil {
		return domain.UniverseDiscoveryResult{}, fmt.Errorf("read Universe discovery key result: %w", err)
	}
	if insertedCount == 0 {
		var existingKeyUniverseID domain.UniverseID
		if err := tx.QueryRowContext(ctx, `SELECT universe_id FROM universe_discovery_keys WHERE normalized_key = ?`, candidate.QueryKey).Scan(&existingKeyUniverseID); err != nil {
			return domain.UniverseDiscoveryResult{}, fmt.Errorf("resolve concurrent Universe discovery key: %w", err)
		}
		if existingKeyUniverseID != target.ID {
			return domain.UniverseDiscoveryResult{State: domain.UniverseDiscoveryAmbiguous}, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.UniverseDiscoveryResult{}, fmt.Errorf("commit automatic Universe discovery: %w", err)
	}
	return domain.UniverseDiscoveryResult{
		State: state, UniverseID: target.ID, Title: target.Title,
		ExistenceConfidence: target.ExistenceConfidence, NamingConfidence: target.NamingConfidence,
		NamingConfidenceLevel: target.NamingConfidenceLevel, NamingEvidence: target.NamingEvidence, MembershipsAdded: added,
		Provenance: target.Provenance, ConfirmedByUser: target.ConfirmedByUser,
	}, nil
}

func (application *UniverseApplication) findExistingLocalDiscovery(ctx context.Context, query string) (domain.UniverseDiscoveryResult, error) {
	queryKey := universe.NormalizeTitle(query)
	if queryKey == "" {
		return domain.UniverseDiscoveryResult{State: domain.UniverseDiscoveryNotEligible}, nil
	}
	tx, err := application.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, fmt.Errorf("begin local Universe discovery lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	states, err := loadDiscoveryUniversesTx(ctx, tx)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	target, existing, ambiguous, err := findDiscoveryUniverseTx(ctx, tx, queryKey, states)
	if err != nil {
		return domain.UniverseDiscoveryResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.UniverseDiscoveryResult{}, fmt.Errorf("finish local Universe discovery lookup: %w", err)
	}
	if ambiguous {
		return domain.UniverseDiscoveryResult{State: domain.UniverseDiscoveryAmbiguous}, nil
	}
	if !existing {
		return domain.UniverseDiscoveryResult{State: domain.UniverseDiscoveryNotEligible}, nil
	}
	return domain.UniverseDiscoveryResult{
		State: domain.UniverseDiscoveryReused, UniverseID: target.ID, Title: target.Title,
		ExistenceConfidence: target.ExistenceConfidence, NamingConfidence: target.NamingConfidence,
		NamingConfidenceLevel: target.NamingConfidenceLevel, NamingEvidence: target.NamingEvidence, Provenance: target.Provenance,
		ConfirmedByUser: target.ConfirmedByUser,
	}, nil
}

func loadDiscoveryUniversesTx(ctx context.Context, tx *sql.Tx) ([]domain.Universe, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, title, existence_confidence, naming_confidence,
		naming_confidence_level, naming_evidence, provenance, confirmed_by_user
		FROM universes ORDER BY id LIMIT ?`, maxUniverseResolveRows+1)
	if err != nil {
		return nil, fmt.Errorf("load bounded Universe discovery catalog: %w", err)
	}
	var states []domain.Universe
	for rows.Next() {
		var state domain.Universe
		if err := rows.Scan(&state.ID, &state.Title, &state.ExistenceConfidence, &state.NamingConfidence,
			&state.NamingConfidenceLevel, &state.NamingEvidence, &state.Provenance, &state.ConfirmedByUser); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode bounded Universe discovery catalog: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read bounded Universe discovery catalog: %w", err)
	}
	_ = rows.Close()
	if len(states) > maxUniverseResolveRows {
		return nil, ErrUniverseResolveBoundsExceeded
	}
	return states, nil
}

func findDiscoveryUniverseTx(ctx context.Context, tx *sql.Tx, queryKey string, states []domain.Universe) (domain.Universe, bool, bool, error) {
	matches := make(map[domain.UniverseID]domain.Universe)
	var keyedUniverseID domain.UniverseID
	err := tx.QueryRowContext(ctx, `SELECT universe_id FROM universe_discovery_keys WHERE normalized_key = ?`, queryKey).Scan(&keyedUniverseID)
	if err == nil {
		found := false
		var keyedUniverse domain.Universe
		for _, state := range states {
			if state.ID == keyedUniverseID {
				found = true
				keyedUniverse = state
				break
			}
		}
		if !found {
			return domain.Universe{}, false, false, fmt.Errorf("discovery key points to missing Universe %q", keyedUniverseID)
		}
		if universe.HasDistinctiveDiscoveryToken(queryKey) {
			matches[keyedUniverseID] = keyedUniverse
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.Universe{}, false, false, fmt.Errorf("lookup normalized Universe discovery key: %w", err)
	}
	for _, state := range states {
		if discoveryQueryMatches(queryKey, state.Title) {
			matches[state.ID] = state
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT universe_id, alias FROM universe_aliases ORDER BY universe_id, alias LIMIT ?`, maxUniverseResolveAliases+1)
	if err != nil {
		return domain.Universe{}, false, false, fmt.Errorf("load bounded Universe discovery aliases: %w", err)
	}
	count := 0
	for rows.Next() {
		var id domain.UniverseID
		var alias string
		if err := rows.Scan(&id, &alias); err != nil {
			_ = rows.Close()
			return domain.Universe{}, false, false, fmt.Errorf("decode bounded Universe discovery alias: %w", err)
		}
		count++
		if discoveryQueryMatches(queryKey, alias) {
			for _, state := range states {
				if state.ID == id {
					matches[id] = state
					break
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Universe{}, false, false, fmt.Errorf("read bounded Universe discovery aliases: %w", err)
	}
	_ = rows.Close()
	if count > maxUniverseResolveAliases {
		return domain.Universe{}, false, false, ErrUniverseResolveBoundsExceeded
	}
	if len(matches) > 1 {
		return domain.Universe{}, false, true, nil
	}
	for _, state := range matches {
		return state, true, false, nil
	}
	return domain.Universe{}, false, false, nil
}

func discoveryQueryMatches(queryKey, value string) bool {
	valueKey := universe.NormalizeTitle(value)
	if valueKey == "" {
		return false
	}
	if queryKey == valueKey {
		return true
	}
	queryTokens, valueTokens := strings.Fields(queryKey), strings.Fields(valueKey)
	if len(queryTokens) == 1 {
		if !universe.IsDistinctiveSingleTokenQuery(queryKey) {
			return false
		}
		for _, token := range valueTokens {
			if token == queryTokens[0] {
				return true
			}
		}
		return false
	}
	if len(queryTokens) > len(valueTokens) {
		return false
	}
	for start := 0; start <= len(valueTokens)-len(queryTokens); start++ {
		matched := true
		for offset, token := range queryTokens {
			if valueTokens[start+offset] != token {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func filterDiscoveryResultsTx(ctx context.Context, tx *sql.Tx, results []domain.MetadataSearchResult, targetID domain.UniverseID) ([]domain.MetadataSearchResult, error) {
	filtered := make([]domain.MetadataSearchResult, 0, len(results))
	for _, result := range results {
		// Local ownership and exclusions govern every existence signal, including
		// Weak/Related results that are never eligible for automatic membership.
		var workID domain.WorkID
		err := tx.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`, result.Provider, result.ExternalID).Scan(&workID)
		if errors.Is(err, sql.ErrNoRows) {
			filtered = append(filtered, result)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check exact Work identity before automatic discovery: %w", err)
		}
		current, hasCurrent, err := membershipForWorkTx(ctx, tx, workID)
		if err != nil {
			return nil, err
		}
		if hasCurrent && (targetID == "" || current.UniverseID != targetID) {
			continue
		}
		if targetID != "" {
			excluded, err := isUniverseWorkExcludedTx(ctx, tx, workID, targetID)
			if err != nil {
				return nil, err
			}
			if excluded {
				continue
			}
		}
		filtered = append(filtered, result)
	}
	return filtered, nil
}

func membershipForWorkTx(ctx context.Context, tx *sql.Tx, workID domain.WorkID) (domain.UniverseMembership, bool, error) {
	var membership domain.UniverseMembership
	var provenance, acceptedAt, status string
	err := tx.QueryRowContext(ctx, `SELECT universe_id, provenance, confidence, evidence, reason, confirmed_by_user, accepted_at_utc, status
		FROM universe_memberships WHERE work_id = ?`, workID).Scan(
		&membership.UniverseID, &provenance, &membership.Confidence, &membership.Evidence, &membership.Reason, &membership.ConfirmedByUser, &acceptedAt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.UniverseMembership{}, false, nil
	}
	if err != nil {
		return domain.UniverseMembership{}, false, fmt.Errorf("load exact Work membership before automatic discovery: %w", err)
	}
	membership.WorkID = workID
	membership.Provenance = domain.UniverseMembershipProvenance(provenance)
	membership.Status = domain.UniverseMembershipStatus(status)
	membership.AcceptedAt, err = parseUniverseTime(acceptedAt)
	if err != nil {
		return domain.UniverseMembership{}, false, fmt.Errorf("decode exact Work membership before automatic discovery: %w", err)
	}
	return membership, true, nil
}

func legacyUnknownAutomaticName(target domain.Universe) bool {
	return target.Provenance == domain.UniverseProvenanceAutomatic && !target.ConfirmedByUser &&
		target.NamingConfidence == 0 && target.NamingConfidenceLevel == domain.UniverseNamingConfidenceNone &&
		target.NamingEvidence == domain.UniverseNamingEvidenceLegacyUnknown
}

func isUniverseWorkExcludedTx(ctx context.Context, tx *sql.Tx, workID domain.WorkID, universeID domain.UniverseID) (bool, error) {
	var excluded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM universe_exclusions WHERE work_id = ? AND universe_id = ?)`, workID, universeID).Scan(&excluded); err != nil {
		return false, fmt.Errorf("check automatic discovery exclusion: %w", err)
	}
	return excluded, nil
}
