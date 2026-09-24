/**
 * Display rules for Antigravity quota.
 *
 * Antigravity reports its rate limits per capacity pool: upstream returns a
 * Gemini group and a Claude and GPT group, each with the same 5h and weekly
 * windows. Antigravity channels are used for Gemini, so only that pool is shown.
 *
 * The other pool deliberately stays on the channel data. The percentage badge
 * and the quota routing alerts read every limit, so a Claude/GPT window still
 * raises an alarm even though no bar is drawn for it.
 */

export const ANTIGRAVITY_QUOTA_GROUP = 'Gemini';

/**
 * Narrow limits to the pool the UI draws.
 *
 * Falls back to every limit when no pool matches, so a rename upstream leaves the
 * panel showing the data it has instead of an empty box.
 */
export function selectAntigravityDisplayLimits<T extends { readonly group?: string }>(
  limits: readonly T[]
): readonly T[] {
  const geminiLimits = limits.filter((limit) => limit.group === ANTIGRAVITY_QUOTA_GROUP);
  return geminiLimits.length > 0 ? geminiLimits : limits;
}
