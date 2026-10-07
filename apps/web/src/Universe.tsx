import {
  universeMediaLabel,
  universeProposalLabel,
  proposalSelectionIdentity,
  workIdentity,
  type UniverseCandidate,
  type UniverseDetailState,
  type UniverseExclusion,
  type UniverseMembership,
  type UniverseProposal,
  type UniverseResolverState,
  type UniverseDiscovery,
  type UniverseSummary,
  type UniverseSuggestion,
  type UniverseWorkInput,
} from "./universe-model";

export function UniverseMatches({
  discovery,
  onOpen,
}: {
  discovery?: UniverseDiscovery;
  onOpen: (universe: UniverseSummary) => void;
}) {
  if (!discovery || (discovery.state !== "created" && discovery.state !== "reused") ||
    !discovery.universe_id || !discovery.title) return null;
  const universeID = discovery.universe_id;
  const universeTitle = discovery.title;
  const automaticallyDiscovered = discovery.provenance === "automatic" && !discovery.confirmed_by_user;
  const label = automaticallyDiscovered
    ? "Discovered Universe"
    : discovery.confirmed_by_user ? "Confirmed Universe" : "Local Universe";
  return (
    <section className="universe-matches" aria-labelledby="universe-matches-title">
      <div>
        <p className="eyebrow">{label}</p>
        <h3 id="universe-matches-title">{universeTitle}</h3>
        <p className="universe-discovery-copy">
          {automaticallyDiscovered
            ? "Discovered automatically from matching Search results. Review Works to confirm this grouping."
            : "A matching Universe in your local catalog."}
        </p>
      </div>
      <button
        className="universe-match"
        data-search-focus-key={`universe:${universeID}`}
        onClick={() => onOpen({ id: universeID, title: universeTitle })}
        type="button"
        aria-label={`Open Universe: ${universeTitle}`}
      >
        <span className="universe-match-title">Open Universe</span>
        <span className="universe-match-meta">View Works and manage membership <span aria-hidden="true">→</span></span>
      </button>
    </section>
  );
}

export function UniverseExactNameMatch({
  query,
  universe,
  onOpen,
}: {
  query: string;
  universe: UniverseSummary;
  onOpen: (universe: UniverseSummary) => void;
}) {
  return (
    <section aria-labelledby="universe-exact-name-title" className="universe-matches universe-exact-name-match">
      <div>
        <p className="eyebrow">Exact Universe name</p>
        <h3 id="universe-exact-name-title">{universe.title}</h3>
        <p className="universe-discovery-copy">The submitted name “{query}” matches this local Universe. Search relevance and Work membership remain separate.</p>
      </div>
      <button
        className="universe-match"
        data-search-focus-key={`universe:${universe.id}`}
        onClick={() => onOpen(universe)}
        type="button"
        aria-label={`Open Universe: ${universe.title}`}
      >
        <span className="universe-match-title">Open Universe</span>
        <span className="universe-match-meta">View its stored names and Works <span aria-hidden="true">→</span></span>
      </button>
    </section>
  );
}

export function UniverseResolverReview({
  state,
  selected,
  busyIdentity,
  statusMessage,
  onSelect,
  onConfirm,
  onOpenUniverse,
  onExclude,
  onResetExclusion,
}: {
  state: UniverseResolverState;
  selected: Set<string>;
  busyIdentity: string | null;
  statusMessage: string;
  onSelect: (identity: string, checked: boolean) => void;
  onConfirm: (candidate: UniverseCandidate, proposals: UniverseProposal[]) => void;
  onOpenUniverse: (universe: UniverseSummary) => void;
  onExclude: (universeID: string, proposal: UniverseProposal) => void;
  onResetExclusion: (universeID: string, proposal: UniverseProposal) => void;
}) {
  if (state.kind === "idle") return null;
  return (
    <section aria-labelledby="resolver-review-title" className="resolver-review">
      <div className="resolver-heading">
        <div>
          <p className="eyebrow">Suggestions only</p>
          <h3 id="resolver-review-title">Possible groups</h3>
        </div>
        <p>Choose Works to add. Nothing changes until you confirm.</p>
      </div>
      {state.kind === "loading" && <p className="search-message" role="status">Reviewing related results…</p>}
      {state.kind === "error" && <p className="search-message" role="alert">Proposals could not be loaded. Your Search results are unchanged.</p>}
      {state.kind === "ready" && state.candidates.length === 0 && <p className="search-message" role="status">No conservative grouping was found for these results.</p>}
      {state.kind === "ready" && state.candidates.map((candidate, index) => {
        const selectable = candidate.memberships.filter((proposal) => proposal.existing_state === "none" && !proposal.suppressed);
        const chosen = selectable.filter((proposal) => selected.has(proposalSelectionIdentity(candidate, index, proposal)));
        const groupedProposals = new Map<string, UniverseProposal[]>();
        for (const proposal of candidate.memberships) {
          const label = universeProposalLabel(proposal);
          groupedProposals.set(label, [...(groupedProposals.get(label) ?? []), proposal]);
        }
        return (
          <article className="resolver-candidate" key={`${candidate.id ?? "new"}:${candidate.title}:${index}`}>
            <div className="resolver-candidate-heading">
              <div>
                <p className="eyebrow">{candidate.existing ? "Existing Universe" : "Possible new Universe"}</p>
                <h4>{candidate.title}</h4>
              </div>
              <span className={`confidence-badge confidence-${candidate.confidence_level}`}>
                {candidate.confidence_level} evidence · {Math.round(candidate.confidence * 100)}%
              </span>
            </div>
            <p className="resolver-reason"><strong>Reason:</strong> {candidate.reason}</p>
            <p className="resolver-evidence">Evidence: {candidate.evidence.replaceAll("_", " ")}</p>
            {candidate.id && (
              <button className="quiet-button" onClick={() => onOpenUniverse({ id: candidate.id!, title: candidate.title })} type="button">
                Open Universe
              </button>
            )}
            <div className="resolver-media-groups">
              {[...groupedProposals.entries()].map(([mediaLabel, proposals]) => (
                <section className="resolver-media-group" aria-label={`${mediaLabel} proposals`} key={mediaLabel}>
                  <h5 className="resolver-media-type">{mediaLabel}</h5>
                  <ul className="resolver-work-list">
                    {proposals.map((proposal) => {
                      const identity = workIdentity(proposal.provider, proposal.external_id);
                      const selectionIdentity = proposalSelectionIdentity(candidate, index, proposal);
                      const blocked = proposal.existing_state !== "none" || proposal.suppressed;
                      const selectedHere = selected.has(selectionIdentity);
                      const stateLabel = proposal.confirmed_by_user
                        ? "Confirmed by you"
                        : proposal.existing_state === "accepted_elsewhere"
                          ? "Accepted in another Universe"
                          : proposal.existing_state === "accepted"
                            ? "Already accepted"
                            : proposal.suppressed || proposal.existing_state === "excluded"
                              ? "Excluded from this Universe"
                              : "Needs your confirmation";
                      return (
                        <li className="resolver-work" key={identity}>
                          <div className="resolver-work-main">
                            <strong>{proposal.title}</strong>
                            <span className="resolver-identity">{proposal.provider} · {proposal.external_id}</span>
                            <span className={`membership-state${blocked ? " is-blocked" : ""}`}>{stateLabel}</span>
                            <span className="resolver-reason"><strong>Reason:</strong> {proposal.reason}</span>
                          </div>
                          <div className="resolver-work-controls">
                            <label className="resolver-include">
                              <input
                                aria-label={`Include ${proposal.title} from ${proposal.provider} (${proposal.external_id})`}
                                checked={selectedHere}
                                disabled={blocked || busyIdentity !== null}
                                onChange={(event) => onSelect(selectionIdentity, event.currentTarget.checked)}
                                type="checkbox"
                              />
                              Include
                            </label>
                            {candidate.id && proposal.existing_state === "none" && !proposal.suppressed && (
                              <button
                                className="quiet-button"
                                disabled={busyIdentity === identity}
                                onClick={() => onExclude(candidate.id!, proposal)}
                                type="button"
                                aria-label={`Exclude ${proposal.title} from ${proposal.provider} (${proposal.external_id})`}
                              >Exclude</button>
                            )}
                            {candidate.id && (proposal.suppressed || proposal.existing_state === "excluded") && (
                              <button
                                className="quiet-button"
                                disabled={busyIdentity === identity}
                                onClick={() => onResetExclusion(candidate.id!, proposal)}
                                type="button"
                                aria-label={`Reset exclusion for ${proposal.title} from ${proposal.provider} (${proposal.external_id})`}
                              >Reset exclusion</button>
                            )}
                          </div>
                        </li>
                      );
                    })}
                  </ul>
                </section>
              ))}
            </div>
            <button
              className="primary-button"
              disabled={chosen.length === 0 || busyIdentity !== null}
              onClick={() => onConfirm(candidate, chosen)}
              type="button"
            >{candidate.existing ? "Confirm selected Works" : "Create Universe with selected Works"}</button>
          </article>
        );
      })}
      {statusMessage && <p className="universe-action-status" role="status">{statusMessage}</p>}
    </section>
  );
}

export function UniverseDetailView({
  state,
  managing,
  workResults,
  busyIdentity,
  statusMessage,
  onConfirmUniverse,
  onToggleManage,
  onRetry,
  onConfirmWork,
  onRemoveWork,
  onRejectWork,
  onResetExclusion,
}: {
  state: UniverseDetailState;
  managing: boolean;
  workResults: UniverseWorkInput[];
  busyIdentity: string | null;
  statusMessage: string;
  onConfirmUniverse: () => void;
  onToggleManage: () => void;
  onRetry: () => void;
  onConfirmWork: (work: UniverseWorkInput) => void;
  onRemoveWork: (work: UniverseMembership) => void;
  onRejectWork: (work: UniverseWorkInput) => void;
  onResetExclusion: (work: UniverseExclusion) => void;
}) {
  if (state.kind === "loading") {
    return <section className="universe-page" aria-label="Universe"><p role="status">Loading Universe…</p></section>;
  }
  if (state.kind === "error") {
    return (
      <section className="universe-page" aria-label="Universe">
        <div className="search-error" role="alert">
          <p>Universe details are temporarily unavailable.</p>
          <button onClick={onRetry} type="button">Try again</button>
        </div>
      </section>
    );
  }

  const detail = state.detail;
  const automaticUniverse = detail.provenance === "automatic";
  const universeConfirmed = detail.confirmed_by_user === true;
  const automaticallyDiscovered = automaticUniverse && detail.confirmed_by_user === false;
  const allMembershipsConfirmed = detail.memberships.every((membership) => membership.confirmed_by_user);
  const membershipCount = detail.memberships.length;
  const membershipSummary = allMembershipsConfirmed
    ? `${membershipCount} confirmed ${membershipCount === 1 ? "Work" : "Works"}`
    : `${membershipCount} ${membershipCount === 1 ? "Work" : "Works"} in this Universe`;
  const memberIdentities = new Set(detail.memberships.map((item) => workIdentity(item.provider, item.external_id)));
  const exclusionByIdentity = new Map(detail.exclusions.map((item) => [workIdentity(item.provider, item.external_id), item]));
  const suggestionIdentities = new Set(detail.suggestions.map((item) => workIdentity(item.provider, item.external_id)));
  const availableWorks = workResults.filter((work) => {
    const identity = workIdentity(work.provider, work.external_id);
    return !memberIdentities.has(identity) && (!suggestionIdentities.has(identity) || exclusionByIdentity.has(identity));
  });
  const availableIdentities = new Set(availableWorks.map((work) => workIdentity(work.provider, work.external_id)));
  const categories = new Map<string, UniverseMembership[]>();
  for (const membership of detail.memberships) {
    const label = universeMediaLabel(membership.medium, membership.work_type);
    categories.set(label, [...(categories.get(label) ?? []), membership]);
  }

  return (
    <section className="universe-page" aria-labelledby="universe-title">
      <div className="universe-page-heading">
        <div>
          <p className="eyebrow">{universeConfirmed ? "Confirmed Universe" : automaticUniverse ? "Discovered Universe" : "Your local collection"}</p>
          <h2 id="universe-title">{detail.title}</h2>
          <p>{membershipSummary}</p>
          {automaticallyDiscovered && (
            <div>
              <p className="universe-discovery-copy">Discovered automatically. Confirm this Universe only if the collection is real; Works remain individually unconfirmed until you confirm them.</p>
              <button
                className="primary-button"
                disabled={busyIdentity === "universe-confirm"}
                onClick={onConfirmUniverse}
                type="button"
              >{busyIdentity === "universe-confirm" ? "Confirming Universe…" : "Confirm Universe"}</button>
            </div>
          )}
          {automaticUniverse && detail.confirmed_by_user && (
            <p className="universe-discovery-copy">Universe confirmed by you. Works still need separate confirmation.</p>
          )}
        </div>
        <button aria-pressed={managing} className="quiet-button" onClick={onToggleManage} type="button">
          {managing ? "Done managing" : "Manage Works"}
        </button>
      </div>

      {detail.aliases.length > 0 && (
        <section aria-labelledby="universe-aliases-title" className="universe-aliases">
          <div>
            <p className="eyebrow">Other names</p>
            <h3 id="universe-aliases-title">Aliases</h3>
            <p className="universe-aliases-description">Names stored for this Universe, separate from its Works and relationships.</p>
          </div>
          <ul className="universe-alias-list">
            {detail.aliases.map((alias, index) => (
              <li className="universe-alias-item" key={`${alias.language ?? "und"}:${alias.value}:${index}`}>
                <span className="universe-alias-value">{alias.value}</span>
                <span className="universe-alias-language">{alias.language ?? "Language not specified"}</span>
              </li>
            ))}
          </ul>
          {detail.aliases_truncated && <p className="universe-aliases-more">Some aliases are not shown.</p>}
        </section>
      )}

      {state.kind === "ready" && detail.memberships.length === 0 && (
        <p className="search-message">This Universe has no accepted Works yet.</p>
      )}
      {[...categories.entries()].map(([category, memberships]) => (
        <section className="universe-category" aria-labelledby={`universe-category-${category}`} key={category}>
          <h3 id={`universe-category-${category}`}>{category}</h3>
          <ul className="universe-work-list">
            {memberships.map((membership) => {
              const identity = workIdentity(membership.provider, membership.external_id);
              return (
                <li className="universe-work-item" key={identity}>
                  <div>
                    <strong>{membership.title}</strong>
                    <span className="universe-work-identity">{membership.provider} · {membership.external_id}</span>
                    <span className="universe-work-provenance">
                      {membership.confirmed_by_user
                        ? "Confirmed by you"
                        : membership.provenance === "automatic" ? "Discovered automatically" : `Accepted via ${membership.provenance}`}
                      {` · ${Math.round(membership.confidence * 100)}% evidence`}
                    </span>
                  </div>
                  {managing && (
                    <div className="universe-work-controls">
                      {!membership.confirmed_by_user && (
                        <button
                          className="primary-button"
                          disabled={busyIdentity === identity}
                          onClick={() => onConfirmWork(membership)}
                          type="button"
                          aria-label={`Confirm ${membership.title} membership in this Universe (${membership.provider} ${membership.external_id})`}
                        >Confirm</button>
                      )}
                      <button
                        className="quiet-button"
                        disabled={busyIdentity === identity}
                        onClick={() => onRemoveWork(membership)}
                        type="button"
                        aria-label={`Remove ${membership.title} from this Universe (${membership.provider} ${membership.external_id})`}
                      >Remove</button>
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        </section>
      ))}
      {detail.memberships_truncated && <p className="search-message">More Works exist than can be shown in this bounded view.</p>}

      {detail.suggestions.length > 0 && (
        <section aria-labelledby="universe-suggestions-title" className="universe-category">
          <p className="eyebrow">Suggestions only</p>
          <h3 id="universe-suggestions-title">Review-needed suggestions</h3>
          <p>These Works are not active memberships. Confirm each exact Work explicitly before it is added.</p>
          <ul className="universe-work-list">
            {detail.suggestions.map((suggestion) => renderUniverseSuggestion({
              suggestion,
              managing,
              excluded: exclusionByIdentity.has(workIdentity(suggestion.provider, suggestion.external_id)),
              busyIdentity,
              onConfirmWork,
            }))}
          </ul>
        </section>
      )}
      {detail.suggestions_truncated && <p className="search-message">More review-needed suggestions exist than can be shown in this bounded view.</p>}

      {managing && (
        <section aria-labelledby="manage-works-title" className="manage-works-panel">
          <div>
            <p className="eyebrow">Explicit corrections</p>
            <h3 id="manage-works-title">Manage Works</h3>
            <p>Only exact Works from your current Search are available here. Adding requires a separate confirmation.</p>
          </div>
          {availableWorks.length === 0 && <p className="search-message">Search for a Work to add or exclude it from this Universe.</p>}
          <ul className="universe-work-list">
            {availableWorks.map((work) => {
              const identity = workIdentity(work.provider, work.external_id);
              const exclusion = exclusionByIdentity.get(identity);
              return (
                <li className="universe-work-item" key={identity}>
                  <div>
                    <span className="resolver-media-type">{universeMediaLabel(work.medium, work.work_type)}</span>
                    <strong>{work.title}</strong>
                    <span className="universe-work-identity">{work.provider} · {work.external_id}</span>
                    {exclusion && <span className="membership-state is-blocked">Excluded</span>}
                  </div>
                  <div className="universe-work-controls">
                    {exclusion ? (
                      <button
                        className="quiet-button"
                        disabled={busyIdentity === identity}
                        onClick={() => onResetExclusion(exclusion)}
                        type="button"
                        aria-label={`Reset exclusion for ${work.title} from ${work.provider} (${work.external_id})`}
                      >Reset exclusion</button>
                    ) : (
                      <>
                        <button
                          className="primary-button"
                          disabled={busyIdentity === identity}
                          onClick={() => onConfirmWork(work)}
                          type="button"
                          aria-label={`Add and confirm ${work.title} from ${work.provider} (${work.external_id})`}
                        >Add and confirm</button>
                        <button
                          className="quiet-button"
                          disabled={busyIdentity === identity}
                          onClick={() => onRejectWork(work)}
                          type="button"
                          aria-label={`Exclude ${work.title} from this Universe (${work.provider} ${work.external_id})`}
                        >Exclude</button>
                      </>
                    )}
                  </div>
                </li>
              );
            })}
          </ul>
        </section>
      )}
      {detail.exclusions.length > 0 && (
        <section aria-labelledby="universe-exclusions-title" className="universe-category">
          <h3 id="universe-exclusions-title">Excluded Works</h3>
          <ul className="universe-work-list">
            {detail.exclusions.map((exclusion) => {
              const identity = workIdentity(exclusion.provider, exclusion.external_id);
              return (
                <li className="universe-work-item" key={identity}>
                  <div>
                    <strong>{exclusion.title} — Excluded</strong>
                    <span className="universe-work-identity">{exclusion.provider} · {exclusion.external_id}</span>
                  </div>
                  {managing && !availableIdentities.has(identity) && (
                    <button
                      className="quiet-button"
                      disabled={busyIdentity === identity}
                      onClick={() => onResetExclusion(exclusion)}
                      type="button"
                      aria-label={`Reset exclusion for ${exclusion.title} from ${exclusion.provider} (${exclusion.external_id})`}
                    >Reset exclusion</button>
                  )}
                </li>
              );
            })}
          </ul>
        </section>
      )}
      {detail.exclusions_truncated && <p className="search-message">More exclusions exist than can be shown in this bounded view.</p>}
      {statusMessage && <p className="universe-action-status" role="status">{statusMessage}</p>}
    </section>
  );
}

function renderUniverseSuggestion({
  suggestion,
  managing,
  excluded,
  busyIdentity,
  onConfirmWork,
}: {
  suggestion: UniverseSuggestion;
  managing: boolean;
  excluded: boolean;
  busyIdentity: string | null;
  onConfirmWork: (work: UniverseWorkInput) => void;
}) {
  const identity = workIdentity(suggestion.provider, suggestion.external_id);
  return (
    <li className="universe-work-item" key={identity}>
      <div>
        <span className="resolver-media-type">Media: {suggestion.media_type}</span>
        <strong>{suggestion.title}</strong>
        <span className="universe-work-identity">{suggestion.provider} · {suggestion.external_id}</span>
        <span className="universe-work-provenance">
          {suggestion.provenance === "automatic" ? "Automatic suggestion" : `Suggestion via ${suggestion.provenance}`} · Not confirmed by you
        </span>
        <span className="resolver-evidence">
          {Math.round(suggestion.confidence * 100)}% stored confidence · evidence {suggestion.evidence}
        </span>
        <span>{suggestion.reason}</span>
        {excluded && <span className="membership-state is-blocked">This Work is excluded from this Universe.</span>}
      </div>
      {managing && !excluded && (
        <div className="universe-work-controls">
          <button
            className="primary-button"
            disabled={busyIdentity === identity}
            onClick={() => onConfirmWork(suggestion)}
            type="button"
            aria-label={`Confirm ${suggestion.title} membership in this Universe (${suggestion.provider} ${suggestion.external_id})`}
          >Confirm</button>
        </div>
      )}
    </li>
  );
}
