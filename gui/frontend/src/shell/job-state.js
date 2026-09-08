/** Completion belongs to the captured draft revisions, never a copied bundle
 * or the selected target's display label. Missing revision evidence is unknown.
 */
export function buildFulfilsDraft(status, generation, selectionRevision) {
  return Boolean(
    status?.finished && !status.running && !status.cancelled && !status.error &&
    status.summary?.exit_class === 'success' && status.summary?.bundle_path &&
    !status.summary.unresolved?.length && !status.summary.fetch_failed?.length &&
    status.target_generation != null && status.target_generation === generation &&
    status.selection_revision != null && status.selection_revision === selectionRevision
  );
}

/** Key-set revisions gate counts; build-input revisions also include digest
 * edits and therefore must never be compared against SelectionSummary.revision.
 */
export function acceptSelectionSummary(state, summary) {
  if (!summary || typeof summary.total !== 'number') return false;
  if (typeof summary.revision === 'number' && summary.revision < (state.selectionKeyRevision ?? 0)) return false;
  state.selectionTotal = summary.total;
  if (typeof summary.revision === 'number') state.selectionKeyRevision = summary.revision;
  return true;
}

/** Terminal events request a read; only a current status snapshot can announce
 * an outcome. Consume foreground outcomes too, so replay cannot toast them
 * after the operator leaves the owning screen.
 */
export function createTerminalNoticeTracker() {
  const pending = new Set(), seen = new Map();
  return {
    request(route) { pending.add(route); },
    hasPending(route) { return pending.has(route); },
    accept(route, status) {
      if (!status || ['running', 'finished', 'cancelled'].some(key => typeof status[key] !== 'boolean')) return null;
      if (status.running) { pending.delete(route); seen.delete(route); return null; }
      if (!pending.delete(route) || !status.finished) return null;
      const key = JSON.stringify([status.started_at, status.finished_at, status.target_generation,
        status.selection_revision, status.target_id, status.destination_path, status.bundle_path,
        status.summary?.bundle_path, status.cancelled, status.error?.code, status.summary?.exit_class,
        status.verified, status.verify_skipped, status.ok]);
      if (seen.get(route) === key) return null;
      seen.set(route, key);
      const label = route === 'build' ? 'The build' : route === 'export' ? 'The copy' : 'Verification';
      if (status.cancelled) return { kind: 'info', text: `${label} was cancelled.` };
      if (status.error) return { kind: 'danger', text: status.error.message || `${label} failed.` };
      if (route === 'build') {
        const summary = status.summary;
        if (summary?.exit_class === 'success' && summary.bundle_path && !summary.unresolved?.length && !summary.fetch_failed?.length) {
          return { kind: 'success', text: 'The build finished.' };
        }
        return { kind: 'warning', text: 'The build finished without a complete bundle.' };
      }
      if (route === 'export') {
        return status.verified && !status.verify_skipped && !status.mismatch_count
          ? { kind: 'success', text: 'The copy finished and its files were checked.' }
          : { kind: 'warning', text: 'The copy finished without verified integrity.' };
      }
      return status.ok ? { kind: 'success', text: 'Verification passed.' }
        : { kind: 'warning', text: 'Verification failed.' };
    },
  };
}
