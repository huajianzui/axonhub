import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import ts from 'typescript';

const componentsDir = import.meta.dirname;
const srcRoot = join(componentsDir, '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

// Isolate the Codex render branch (between the Codex and XAI branch markers)
// so assertions about the usage bars cannot bleed into neighboring providers.
function isolateCodexBlock(source) {
  const start = source.indexOf("{channel.type === 'codex' &&");
  const end = source.indexOf("{channel.type === 'xai_subscription' &&", start);

  assert.ok(start !== -1, 'Codex render branch should exist in quota-badges source');
  assert.ok(end !== -1 && end > start, 'XAI render branch should follow the Codex branch');

  return source.slice(start, end);
}

test('Codex usage windows render as combined usage and time bars', () => {
  const quotaBadges = read('components/quota-badges.tsx');
  const codexBlock = isolateCodexBlock(quotaBadges);

  // Each window has one shared bar whose fill reflects usage and whose marker
  // reflects the reset-window elapsed time.
  assert.equal(
    (codexBlock.match(/<UsageTimeBar\s/g) || []).length,
    1,
    'Codex normalized limits should use one combined bar'
  );
  assert.match(
    codexBlock,
    /quota\.limits\s*\.filter\([\s\S]*?\.map\(\(limit,\s*index\)/,
    'Codex bars should render normalized limits'
  );
  assert.match(
    codexBlock,
    /limit\.window === '5h'/,
    'Codex five-hour label should be selected from the normalized window'
  );
  assert.match(
    codexBlock,
    /limit\.window === '7d'/,
    'Codex seven-day label should be selected from the normalized window'
  );
  assert.match(
    codexBlock,
    /WINDOW_LABEL_KEYS\[limit\.window\]/,
    'Codex labels should resolve through the shared translation map'
  );
  assert.match(
    codexBlock,
    /t\('quota\.label\.token_usage'\)/,
    'Codex unknown windows should use the neutral quota label'
  );
  assert.doesNotMatch(codexBlock, /quota\.label\.primary_window/);
  assert.doesNotMatch(codexBlock, /quota\.label\.secondary_window/);
});

test('quota popover has no standalone progress-bar renders', () => {
  const quotaBadges = read('components/quota-badges.tsx');

  assert.doesNotMatch(quotaBadges, /\bProgressBar\b/, 'all quota bars should use the combined UsageTimeBar component');
});

test('Command Code monthly hover matches the other windows', () => {
  const source = read('components/quota-badges.tsx');
  const commandCodeBlock = source.slice(source.indexOf("{isCommandCodeType(channel.type) &&"), source.indexOf("{channel.type === 'moonshot_coding' &&"));

  assert.match(commandCodeBlock, /monthlyDurationPct[\s\S]*quota\.label\.time_elapsed/);
  assert.doesNotMatch(commandCodeBlock, /quota\.label\.commandcode\.monthly_remaining/);
  assert.doesNotMatch(commandCodeBlock, /subscription_status/);
});

test('Codex reset can be attempted after a transient reset-list failure', () => {
  const quotaBadges = read('components/quota-badges.tsx');
  const codexBlock = isolateCodexBlock(quotaBadges);

  assert.match(
    codexBlock,
    /const canAttemptReset =\s*qd\._resets\?\.supported === true && \(Boolean\(qd\._resets\.error\) \|\| availableResetCount > 0\)/,
    'a supported provider should allow a fresh reset attempt when reset-list metadata failed'
  );
  assert.match(
    codexBlock,
    /disabled=\{isResetting \|\| !canAttemptReset\}/,
    'the reset button should use the retry-aware availability condition'
  );
});

test('Ollama badge derives percentage from the heavier of the 5h/weekly windows', () => {
  const source = read('components/quota-badges.tsx');
  const start = source.indexOf('} else if (isOllamaType(channel.type)) {');
  const end = source.indexOf('} else if (', start + 5);
  const percentBlock = source.slice(start, end);

  assert.match(
    percentBlock,
    /Math\.max\(\s*qd\?\.windows\?\.\[['"]5h['"]\]\?\.usage_percent \?\? 0,\s*qd\?\.windows\?\.weekly\?\.usage_percent \?\? 0\s*\)/,
    'ollama badge percentage should be the max of the 5h and weekly window usage'
  );
});

test('Ollama badge renders both the 5h and weekly windows with a reset countdown', () => {
  const source = read('components/quota-badges.tsx');
  const start = source.indexOf("{isOllamaType(channel.type) &&");
  const end = source.indexOf("{isCommandCodeType(channel.type) &&", start);
  const ollamaBlock = source.slice(start, end);

  assert.match(ollamaBlock, /QuotaWindow5h|'5h'/);
  assert.match(ollamaBlock, /'quota\.window\.5h'/);
  assert.match(ollamaBlock, /'quota\.window\.weekly'/);
  assert.match(ollamaBlock, /formatTimeToReset\(window\.reset_time\)/);
  assert.doesNotMatch(ollamaBlock, /durationPercent/);
});

test('quota window identifiers resolve to localized labels', () => {
  const quotaBadges = read('components/quota-badges.tsx');

  assert.match(quotaBadges, /payg:\s*'quota\.label\.token_usage'/);
  assert.match(quotaBadges, /credits:\s*'quota\.label\.credits_remaining'/);
  assert.match(quotaBadges, /'5h':\s*'quota\.window\.5h'/);
  assert.match(quotaBadges, /'7d':\s*'quota\.window\.7d'/);
  assert.match(quotaBadges, /limit\.window === 'primary' \|\| limit\.window === 'secondary'/);
});

test('Wafer and Apertis duration markers share timestamp validation', () => {
  const quotaBadges = read('components/quota-badges.tsx');
  const waferStart = quotaBadges.indexOf("{isOpenaiType(channel.type) && channel.providerType === 'wafer' &&");
  const waferEnd = quotaBadges.indexOf("{isOpenaiType(channel.type) && channel.providerType === 'synthetic' &&");
  const apertisStart = quotaBadges.indexOf("{isOpenaiType(channel.type) && channel.providerType === 'apertis' &&");
  const apertisEnd = quotaBadges.indexOf("{isOpenaiType(channel.type) && channel.providerType === 'charm_hyper' &&");

  assert.match(quotaBadges, /function getDurationPercent\([\s\S]*?Number\.isFinite\(start\)[\s\S]*?end <= start/);
  assert.match(quotaBadges.slice(waferStart, waferEnd), /durationPercent=\{getDurationPercent\(qd\.window_start, qd\.window_end\)\}/);
  assert.match(
    quotaBadges.slice(apertisStart, apertisEnd),
    /durationPercent=\{getDurationPercent\(qd\.subscription\.cycle_start, qd\.subscription\.cycle_end\)\}/
  );
});


// --- Mode-aware quota badges (quota routing) ---

// Extract the pure mode helpers from quota-badges.tsx without loading React.
const badgesSource = read('components/quota-badges.tsx');
const badgesAst = ts.createSourceFile('quota-badges.tsx', badgesSource, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const modeHelperNames = ['resolveEffectiveRoutingMode', 'mostRestrictiveRoutingMode'];
const modeHelperNodes = badgesAst.statements.filter((node) => ts.isFunctionDeclaration(node) && modeHelperNames.includes(node.name?.text));
assert.equal(modeHelperNodes.length, 2, 'mode helpers must stay in quota-badges.tsx');
const modeHelpers = modeHelperNodes.map((node) => `export ${node.getText(badgesAst).replace(/^export\s+/, '')}`).join('\n');

function loadTsModule(sourceText) {
  const { outputText } = ts.transpileModule(sourceText, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
  });
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
}

const { resolveEffectiveRoutingMode, mostRestrictiveRoutingMode } = await loadTsModule(modeHelpers);

test('per-channel mode falls back to the global default when the channel defers via INHERIT', () => {
  // parseChannelNode maps channels without settings.quotaRoutingMode to INHERIT.
  assert.match(read('features/system/data/quotas.ts'), /quotaRoutingMode: node\.settings\?\.quotaRoutingMode \?\? 'INHERIT'/);
  assert.equal(resolveEffectiveRoutingMode('INHERIT', 'BACKPRESSURE'), 'BACKPRESSURE');
  assert.equal(resolveEffectiveRoutingMode('INHERIT', 'REMOVE_ON_EXHAUSTED'), 'REMOVE_ON_EXHAUSTED');
  // An explicit channel mode wins over the global default.
  assert.equal(resolveEffectiveRoutingMode('IGNORE_QUOTA', 'BACKPRESSURE'), 'IGNORE_QUOTA');
});

test('without read_settings scope data the mode label degrades gracefully', () => {
  // The scope-gated hook returns undefined data; an INHERIT channel defers to
  // a global default the viewer cannot see, so resolution yields null instead
  // of guessing a mode. Explicit channel modes still resolve.
  assert.equal(resolveEffectiveRoutingMode('INHERIT', undefined), null);
  assert.equal(resolveEffectiveRoutingMode('BACKPRESSURE', undefined), 'BACKPRESSURE');
  // QuotaBadges consumes the hook through optional chaining and QuotaRow
  // omits the badge entirely when no mode resolves — no crash, no label.
  assert.match(badgesSource, /const \{ data: routingSettings \} = useQuotaRoutingSettings\(\)/);
  assert.match(badgesSource, /routingSettings\?\.defaultMode/);
  assert.doesNotMatch(badgesSource, /useQuotaEnforcementSettings|QuotaEnforcementMode|allowedChannelIDs|enforcementEffect/);
  assert.match(badgesSource, /\{modeBadge && \(/);
});

test('account-grouped label uses the most restrictive member mode', () => {
  assert.equal(mostRestrictiveRoutingMode(['BACKPRESSURE', 'REMOVE_ON_EXHAUSTED']), 'BACKPRESSURE');
  assert.equal(mostRestrictiveRoutingMode(['REMOVE_ON_EXHAUSTED', 'IGNORE_QUOTA']), 'REMOVE_ON_EXHAUSTED');
  assert.equal(mostRestrictiveRoutingMode(['IGNORE_QUOTA', null]), 'IGNORE_QUOTA');
  // Unresolvable members are ignored; an all-unresolvable group shows no badge.
  assert.equal(mostRestrictiveRoutingMode([null, null]), null);
  assert.equal(mostRestrictiveRoutingMode([]), null);
});

test('grouped representatives render the group mode, standalone channels their own', () => {
  // The row list must stay a real .map over groupedChannels (guards against
  // the opener being eaten by an edit while tsc-visible JSX text survives).
  assert.match(badgesSource, /groupedChannels\.map\(\(channel: ProviderQuotaChannel\) => \(\n\s*<QuotaRow key=\{channel\.id\} channel=\{channel\} effectiveMode=\{effectiveModeFor\(channel\)\}/);
  assert.match(badgesSource, /effectiveMode=\{effectiveModeFor\(channel\)\}/);
  assert.match(badgesSource, /if \(!channel\.sharedAccountNames\) return resolveEffectiveRoutingMode\(channel\.quotaRoutingMode, routingSettings\?\.defaultMode\)/);
  assert.match(badgesSource, /\.filter\(\(c\) => c\.accountKey === channel\.accountKey\)/);
  assert.match(badgesSource, /mostRestrictiveRoutingMode\(groupModes\)/);
});

test('mode badge labels are locale-complete and legacy enforcement keys are gone', () => {
  const enSystem = JSON.parse(read('locales/en/system.json'));
  const zhSystem = JSON.parse(read('locales/zh-CN/system.json'));
  for (const key of ['quota.status.ignore_quota', 'quota.status.remove_on_exhausted', 'quota.status.backpressure']) {
    assert.ok(enSystem[key], `en/system.json missing ${key}`);
    assert.ok(zhSystem[key], `zh-CN/system.json missing ${key}`);
  }
  assert.equal(zhSystem['quota.status.ignore_quota'], '忽略限额');
  assert.equal(zhSystem['quota.status.remove_on_exhausted'], '已摘除');
  assert.equal(zhSystem['quota.status.backpressure'], '限流调度');
  for (const [name, obj] of [['en', enSystem], ['zh-CN', zhSystem]]) {
    const legacy = Object.keys(obj).filter(
      (key) => key.startsWith('quota.status.blocked') || key.startsWith('quota.status.deprioritized') || key.startsWith('quota.status.bypassed')
    );
    assert.deepEqual(legacy, [], `${name}/system.json still has legacy quota status keys`);
  }
});

// --- Antigravity windowed quota ---

// Antigravity publishes the same 5h/weekly windows for several capacity pools,
// so the render branch must keep the pools apart instead of drawing one
// indistinguishable row per window.
function isolateAntigravityBlock(source) {
  const start = source.indexOf("{channel.type === 'antigravity' &&");
  const end = source.indexOf("{channel.type === 'cline' &&", start);

  assert.ok(start !== -1, 'Antigravity render branch should exist in quota-badges source');
  assert.ok(end !== -1 && end > start, 'Cline render branch should follow the Antigravity branch');

  return source.slice(start, end);
}

test('Antigravity draws only the Gemini pool', () => {
  const source = read('components/quota-badges.tsx');
  const block = isolateAntigravityBlock(source);

  // Only this channel's actual capacity is drawn; the other pool is deliberately
  // left out of the panel.
  assert.match(block, /limit\.group === ANTIGRAVITY_QUOTA_GROUP/, 'limits should be narrowed to the Gemini pool');
  assert.match(source, /const ANTIGRAVITY_QUOTA_GROUP = 'Gemini'/, 'the pool constant should name Gemini');
  // Upstream could rename the pool; the panel must still show the data it has
  // rather than rendering nothing while limits exist.
  assert.match(block, /geminiLimits\.length > 0 \? geminiLimits : limits/, 'an unmatched pool should fall back to all limits');
  assert.match(block, /quota\.limits\.filter\(\(limit\) => limit\.type === 'token'\)/);
  assert.match(block, /WINDOW_LABEL_KEYS\[limit\.window\]/, 'windows should resolve through the shared label map');
  assert.match(block, /formatTimeToReset\(limit\.nextResetAt\)/);
});

// Hiding the second pool from the panel must not hide it from the quota system:
// the percentage badge and the routing alerts read every limit on the channel.
test('Antigravity keeps every pool on the data path, not just the drawn one', () => {
  const source = read('components/quota-badges.tsx');
  const start = source.indexOf("} else if (channel.type === 'antigravity') {");
  const end = source.indexOf('} else if (', start + 5);

  assert.ok(start !== -1, 'Antigravity badge percentage branch should exist');
  assert.match(
    source.slice(start, end),
    /channel\.quotaStatus\.limits\.map\(\(limit\) => limit\.usageRatio \* 100\)/,
    'the badge must consider every pool, not only the one the panel draws'
  );
  // The panel narrowing lives in the render branch only.
  assert.doesNotMatch(source.slice(start, end), /ANTIGRAVITY_QUOTA_GROUP/);
});

test('Antigravity badge percentage follows the busiest window', () => {
  const source = read('components/quota-badges.tsx');
  const start = source.indexOf("} else if (channel.type === 'antigravity') {");
  const end = source.indexOf('} else if (', start + 5);

  assert.ok(start !== -1, 'Antigravity badge percentage branch should exist');
  assert.match(
    source.slice(start, end),
    /Math\.max\(0, \.\.\.channel\.quotaStatus\.limits\.map\(\(limit\) => limit\.usageRatio \* 100\)\)/
  );
});

test('quota limit groups survive the console parser and the channel union', () => {
  const quotas = read('features/system/data/quotas.ts');

  assert.match(quotas, /group\?: string;/, 'the limit type should carry the pool');
  assert.match(quotas, /const group = optionalString\(limit\.group\)/, 'the parser should read the pool');
  assert.match(quotas, /group,\s*\n\s*nextResetAt,/, 'the parser should return the pool');
  // The channel union is discriminated on type; an unlisted antigravity member
  // makes every channel.type === 'antigravity' comparison a type error.
  assert.match(quotas, /type: 'antigravity';/, 'antigravity needs a channel union member');
});