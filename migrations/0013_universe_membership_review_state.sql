ALTER TABLE universe_memberships
    ADD COLUMN status TEXT NOT NULL DEFAULT 'accepted'
    CHECK (status IN ('accepted', 'review_needed'));

-- Legacy automatic links without supporting evidence remain durable rows, but
-- they are suggestions until an explicit confirmation or stronger authority.
UPDATE universe_memberships
SET status = 'review_needed'
WHERE provenance = 'automatic'
  AND confirmed_by_user = 0
  AND (confidence < 0.85 OR evidence NOT IN (
      'exact_title_anchor',
      'whole_phrase_anchor',
      'explicit_alias',
      'distinctive_whole_token'
  ));

CREATE TRIGGER universe_memberships_automatic_evidence_insert
BEFORE INSERT ON universe_memberships
WHEN NEW.status = 'accepted'
  AND NEW.provenance = 'automatic'
  AND NEW.confirmed_by_user = 0
  AND (NEW.confidence < 0.85 OR NEW.evidence NOT IN (
      'exact_title_anchor',
      'whole_phrase_anchor',
      'explicit_alias',
      'distinctive_whole_token'
  ))
BEGIN
    SELECT RAISE(ABORT, 'automatic Universe membership requires high-confidence supported evidence');
END;

CREATE TRIGGER universe_memberships_automatic_evidence_update
BEFORE UPDATE OF provenance, confidence, evidence, confirmed_by_user, status ON universe_memberships
WHEN NEW.status = 'accepted'
  AND NEW.provenance = 'automatic'
  AND NEW.confirmed_by_user = 0
  AND (NEW.confidence < 0.85 OR NEW.evidence NOT IN (
      'exact_title_anchor',
      'whole_phrase_anchor',
      'explicit_alias',
      'distinctive_whole_token'
  ))
BEGIN
    SELECT RAISE(ABORT, 'automatic Universe membership requires high-confidence supported evidence');
END;
