package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"lernae/internal/domain"
)

const (
	maxSeriesDetailCollections = 20
	maxSeriesDetailMembers     = 20
)

// GetWorkSeriesDetails returns only explicit part_of_series memberships for
// the exact provider/external-ID pair. Search titles and author metadata are
// deliberately not consulted. The member list excludes the current Work,
// while KnownTotal counts all distinct persisted Work memberships, including
// the current Work and peers omitted because they lack a usable external
// identity. The peer list is independently capped and contains only Works
// with a nonblank provider and external ID.
func (r *SQLiteRepository) GetWorkSeriesDetails(
	ctx context.Context, provider, externalID string,
) ([]domain.WorkSeriesDetails, bool, error) {
	var workID domain.WorkID
	err := r.db.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`, provider, externalID).Scan(&workID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve exact Work identity for Series Details: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `SELECT c.id, c.title, relation.ordinal,
		(SELECT COUNT(DISTINCT members.work_id) FROM work_relations members
			WHERE members.relation_type = 'part_of_series' AND members.target_collection_id = c.id)
		FROM work_relations relation
		JOIN work_collections c ON c.id = relation.target_collection_id AND c.collection_type = 'series'
		WHERE relation.work_id = ? AND relation.relation_type = 'part_of_series'
		ORDER BY c.id COLLATE BINARY LIMIT ?`, workID, maxSeriesDetailCollections+1)
	if err != nil {
		return nil, false, fmt.Errorf("load exact Work Series Details: %w", err)
	}
	type seriesRow struct {
		collectionID domain.WorkCollectionID
		title        string
		ordinal      sql.NullString
		knownTotal   int64
	}
	var seriesRows []seriesRow
	for rows.Next() {
		var item seriesRow
		if err := rows.Scan(&item.collectionID, &item.title, &item.ordinal, &item.knownTotal); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("decode exact Work Series Details: %w", err)
		}
		seriesRows = append(seriesRows, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, fmt.Errorf("read exact Work Series Details: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, false, fmt.Errorf("close exact Work Series Details: %w", err)
	}
	truncated := len(seriesRows) > maxSeriesDetailCollections
	if truncated {
		seriesRows = seriesRows[:maxSeriesDetailCollections]
	}
	series := make([]domain.WorkSeriesDetails, 0, len(seriesRows))
	for _, item := range seriesRows {
		knownTotal := int(item.knownTotal)
		entry := domain.WorkSeriesDetails{
			CollectionID: item.collectionID,
			Title:        item.title,
			Ordinal:      nullStringPointer(item.ordinal),
			KnownTotal:   &knownTotal,
			MoreInSeries: []domain.WorkSeriesMember{},
		}
		members, membersTruncated, err := r.workSeriesMembers(ctx, item.collectionID, workID)
		if err != nil {
			return nil, false, err
		}
		entry.MoreInSeries = members
		entry.MoreInSeriesTruncated = membersTruncated
		series = append(series, entry)
	}
	return series, truncated, nil
}

func (r *SQLiteRepository) workSeriesMembers(
	ctx context.Context, collectionID domain.WorkCollectionID, currentWorkID domain.WorkID,
) ([]domain.WorkSeriesMember, bool, error) {
	rows, err := r.db.QueryContext(ctx, `WITH display_identities AS (
		SELECT identity.work_id, identity.provider, identity.external_id,
			ROW_NUMBER() OVER (PARTITION BY identity.work_id ORDER BY identity.provider COLLATE BINARY, identity.external_id COLLATE BINARY) AS identity_rank
		FROM external_identities identity
		WHERE length(trim(identity.provider)) > 0 AND length(trim(identity.external_id)) > 0
	)
	SELECT identity.provider, identity.external_id,
		work.title, member.ordinal
		FROM work_relations member JOIN works work ON work.id = member.work_id
		JOIN display_identities identity ON identity.work_id = member.work_id AND identity.identity_rank = 1
		WHERE member.target_collection_id = ? AND member.relation_type = 'part_of_series' AND member.work_id <> ?
		ORDER BY CASE WHEN member.ordinal GLOB '[0-9]*' THEN 0 WHEN member.ordinal IS NULL THEN 2 ELSE 1 END,
			CASE WHEN member.ordinal IS NULL THEN 0 ELSE CAST(member.ordinal AS REAL) END,
			member.ordinal COLLATE BINARY,
			identity.provider COLLATE BINARY,
			identity.external_id COLLATE BINARY,
			member.work_id COLLATE BINARY
		LIMIT ?`, collectionID, currentWorkID, maxSeriesDetailMembers+1)
	if err != nil {
		return nil, false, fmt.Errorf("load bounded members for Work Series %q: %w", collectionID, err)
	}
	defer rows.Close()
	var members []domain.WorkSeriesMember
	for rows.Next() {
		var member domain.WorkSeriesMember
		var ordinal sql.NullString
		if err := rows.Scan(&member.Provider, &member.ExternalID, &member.Title, &ordinal); err != nil {
			return nil, false, fmt.Errorf("decode bounded member for Work Series %q: %w", collectionID, err)
		}
		member.Ordinal = nullStringPointer(ordinal)
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read bounded members for Work Series %q: %w", collectionID, err)
	}
	truncated := len(members) > maxSeriesDetailMembers
	if truncated {
		members = members[:maxSeriesDetailMembers]
	}
	if members == nil {
		members = []domain.WorkSeriesMember{}
	}
	return members, truncated, nil
}
