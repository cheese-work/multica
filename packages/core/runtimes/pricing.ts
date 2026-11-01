import type { RuntimeUsage } from "../types";
import { getCustomPricing } from "./custom-pricing-store";

// Pricing per million tokens (USD). Sources, each authoritative for the
// rows tagged under it — keep in sync when providers release new models
// or adjust prices.
//
//   Anthropic: https://platform.claude.com/docs/en/about-claude/pricing
//   OpenAI:    https://openai.com/api/pricing
//   DeepSeek:  https://api-docs.deepseek.com/quick_start/pricing
//   Moonshot:  https://www.kimi.com/resources/kimi-k2-6-pricing
//   Zhipu:     https://docs.z.ai/guides/overview/pricing
//   xAI:       https://docs.x.ai/developers/pricing
//
// Anthropic's cacheWrite reflects the 5-minute cache TTL (1.25× input); the
// daemon reports cache_creation_input_tokens without TTL metadata, so 5m is
// the safest / cheapest assumption (matches the API default). DeepSeek,
// Moonshot, Zhipu and xAI do not bill cache writes separately (cached input
// is just discounted on subsequent reads), so cacheWrite mirrors input there.
// OpenAI historically did the same, but its GPT-5.6+ generation bills cache
// writes at 1.25× input (cache reads generally get the 90% cached-input
// discount; GPT-6.1 Sol and Terra get 95%), so those rows carry a distinct cacheWrite.
// Codex usage doesn't yet stream cache-write tokens, so that rate isn't
// exercised today.
//
// The resolver matches exact keys after stripping a trailing date snapshot
// (see `resolvePricing` below). It deliberately does NOT do startsWith
// fallbacks: every catalog SKU needs its own row. That keeps unfamiliar
// variants (`gpt-5.5-mini`, hypothetical `gpt-5.4-foo`) from silently
// inheriting the price of a near-named relative; they surface in the
// unmapped diagnostic instead. Mirror new entries in
// `server/pkg/agent/models.go` so the catalog and pricing stay in sync.
//
// Provider-qualified keys: a model id that is NOT vendor-prefixed
// (`claude-*`, `gpt-*`, `o3*`/`o4*`, `glm-*`, `deepseek-*`, `kimi-*`,
// `grok-*`) and is
// not the provider name itself can collide across providers — more than one
// provider may report the same generic id like `auto`. Such generic ids MUST be keyed as
// `${provider}/${model}` (e.g. `cursor/auto`). `resolvePricing` tries the
// `${provider}/…` form first, then the bare form, so vendor-prefixed SKUs
// stay unqualified and still resolve.
const MODEL_PRICING: Record<
  string,
  { input: number; output: number; cacheRead: number; cacheWrite: number }
> = {
  // -- Anthropic: current generation. Sonnet 5 uses Anthropic's published
  //    intro launch rate ($2 / $10 through 2026-08-31). This static map has
  //    no future-dated pricing support yet, so update the row when the
  //    post-intro $3 / $15 rate takes effect. Fable 5 and 5.1 are Mythos-class
  //    SKUs at 10/50 (5.1 prices cache reads at 0.025x input, a quarter of the
  //    usual 0.1x); Opus 4.5 through Opus 5 stay on the lower 5/25 Opus tier,
  //    and Opus 5.5 drops to 4/20 (cache reads at 0.05x input). --
  "claude-sonnet-5":     { input: 2,    output: 10,   cacheRead: 0.20, cacheWrite: 2.50 },
  // Sonnet 5.5 keeps the 2/10 tier but halves the cache-read rate.
  "claude-sonnet-5-5":   { input: 2,    output: 10,   cacheRead: 0.10, cacheWrite: 2.50 },
  // Haiku 5.5 uses the <=100K prompt tier. Above 100K (including cache
  // reads/writes), all four rates are 5x higher. Rollups lack per-request
  // prompt sizes, so long requests are underestimated. Mirror the backend
  // estimate in server/internal/metrics/pricing.go.
  "claude-haiku-5-5":    { input: 0.10, output: 0.50, cacheRead: 0.01, cacheWrite: 0.125 },
  "claude-fable-5-1":   { input: 10,   output: 50,   cacheRead: 0.25, cacheWrite: 12.50 },
  "claude-fable-5":     { input: 10,   output: 50,   cacheRead: 1.00, cacheWrite: 12.50 },
  "claude-opus-5-5":    { input: 4,    output: 20,   cacheRead: 0.20, cacheWrite: 5.00 },
  "claude-opus-5":      { input: 5,    output: 25,   cacheRead: 0.50, cacheWrite: 6.25 },
  "claude-haiku-4-5":   { input: 1,    output: 5,    cacheRead: 0.10, cacheWrite: 1.25 },
  "claude-sonnet-4-5":  { input: 3,    output: 15,   cacheRead: 0.30, cacheWrite: 3.75 },
  "claude-sonnet-4-6":  { input: 3,    output: 15,   cacheRead: 0.30, cacheWrite: 3.75 },
  "claude-opus-4-5":    { input: 5,    output: 25,   cacheRead: 0.50, cacheWrite: 6.25 },
  "claude-opus-4-6":    { input: 5,    output: 25,   cacheRead: 0.50, cacheWrite: 6.25 },
  "claude-opus-4-7":    { input: 5,    output: 25,   cacheRead: 0.50, cacheWrite: 6.25 },
  "claude-opus-4-8":    { input: 5,    output: 25,   cacheRead: 0.50, cacheWrite: 6.25 },

  // -- Anthropic: pre-4.5 Opus (legacy, still served at original price tier) --
  "claude-opus-4-1":    { input: 15,   output: 75,   cacheRead: 1.50, cacheWrite: 18.75 },
  "claude-opus-4":      { input: 15,   output: 75,   cacheRead: 1.50, cacheWrite: 18.75 },

  // -- Anthropic: Sonnet 4.0 (deprecated; same price as the 4.x family) --
  "claude-sonnet-4":    { input: 3,    output: 15,   cacheRead: 0.30, cacheWrite: 3.75 },

  // -- Anthropic: older Haiku tier (defensive entry for the rare runtime still on it) --
  "claude-haiku-3-5":   { input: 0.80, output: 4,    cacheRead: 0.08, cacheWrite: 1.00 },

  // -- OpenAI: dotted-minor Codex catalog SKUs. Each generation is priced
  //    independently — no fallback to `gpt-5`. Entries track
  //    `server/pkg/agent/models.go` (Codex provider list).
  //    gpt-6-astra and gpt-5.6 (sol/terra/luna) use OpenAI's official rates.
  //    5.6+ is the first OpenAI generation to bill cache writes separately:
  //    cacheRead generally = 0.1x input, cacheWrite = 1.25x
  //    input (see the header note above). Codex usage doesn't yet report
  //    cache-write tokens, so cacheWrite isn't exercised today, but the rate
  //    is kept correct for when it is.
  "gpt-6-astra":        { input: 10,   output: 50,   cacheRead: 1.00,  cacheWrite: 12.50 },
  // Standard short-context rates: developers.openai.com/api/docs/models/.
  // GPT-6.1 Sol and Terra's cached input is 0.05x input; GPT-6 Sol's is 0.1x.
  "gpt-6.1-sol":        { input: 2,    output: 10,   cacheRead: 0.10,  cacheWrite: 2.50 },
  "gpt-6.1-terra":      { input: 1,    output: 5,    cacheRead: 0.05,  cacheWrite: 1.25 },
  // GPT-6 Sol / Luna (2026-09-22): permanent Standard-tier rates, same cache
  // shape as 5.6. Mirror server/internal/metrics/pricing.go.
  "gpt-6-sol":          { input: 2,    output: 10,   cacheRead: 0.20,  cacheWrite: 2.50 },
  "gpt-6-luna":         { input: 0.10, output: 0.50, cacheRead: 0.01,  cacheWrite: 0.125 },
  "gpt-5.6-sol":        { input: 5,    output: 30,   cacheRead: 0.50,  cacheWrite: 6.25 },
  "gpt-5.6-terra":      { input: 2.50, output: 15,   cacheRead: 0.25,  cacheWrite: 3.125 },
  "gpt-5.6-luna":       { input: 1,    output: 6,    cacheRead: 0.10,  cacheWrite: 1.25 },
  "gpt-5.5":            { input: 5,    output: 30,   cacheRead: 0.50,  cacheWrite: 5 },
  "gpt-5.4-mini":       { input: 0.75, output: 4.50, cacheRead: 0.075, cacheWrite: 0.75 },
  "gpt-5.4":            { input: 2.50, output: 15,   cacheRead: 0.25,  cacheWrite: 2.50 },
  "gpt-5.3-codex":      { input: 1.75, output: 14,   cacheRead: 0.175, cacheWrite: 1.75 },

  // -- Google: https://ai.google.dev/gemini-api/docs/pricing (2026-10-19).
  //    Standard API-equivalent token estimates, not subscription charges.
  //    Argon's intro rate ends 2026-11-30; Flash 3.6/3.8, Robotics and
  //    3.8 TTS promotions end 2026-12-31. Update the static rows at expiry.
  //    Google's deprecations page documents 3.5 Flash -> 3.6 and 3.7 -> 3.8
  //    routing; those aliases use their destination's current rate.
  //    Pro uses the <=200K request tier; totals lack per-request context
  //    sizes. Flash/Lite use text/image/video input rates (audio can differ).
  //    Cache writes estimate input processing only, excluding cache storage,
  //    grounding and service-tier surcharges. Unsupported cache buckets are 0.
  //    Speech-only SKUs have one input/output rate; mixed-modality Live,
  //    Image, Omni and Embedding SKUs stay unmapped because aggregate tokens
  //    cannot distinguish their rates. Mirror server/internal/metrics/pricing.go. --
  "gemini-4-argon": { input: 2, output: 10, cacheRead: 0.1, cacheWrite: 2 },
  "gemini-3.8-flash": { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0.75 },
  "gemini-3.7-flash": { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0.75 },
  "gemini-3.6-flash": { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0.75 },
  "gemini-3.5-flash": { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0.75 },
  "gemini-3.5-flash-lite": { input: 0.3, output: 2.5, cacheRead: 0.03, cacheWrite: 0.3 },
  "gemini-3.1-flash-lite": { input: 0.25, output: 1.5, cacheRead: 0.025, cacheWrite: 0.25 },
  "gemini-3.1-pro": { input: 2, output: 12, cacheRead: 0.2, cacheWrite: 2 },
  "gemini-3.1-pro-preview": { input: 2, output: 12, cacheRead: 0.2, cacheWrite: 2 },
  "gemini-3.1-pro-preview-customtools": { input: 2, output: 12, cacheRead: 0.2, cacheWrite: 2 },
  "gemini-3-flash": { input: 0.5, output: 3, cacheRead: 0.05, cacheWrite: 0.5 },
  "gemini-3-flash-preview": { input: 0.5, output: 3, cacheRead: 0.05, cacheWrite: 0.5 },
  "gemini-2.5-pro": { input: 1.25, output: 10, cacheRead: 0.125, cacheWrite: 1.25 },
  "gemini-2.5-flash": { input: 0.3, output: 2.5, cacheRead: 0.03, cacheWrite: 0.3 },
  "gemini-2.5-flash-lite": { input: 0.1, output: 0.4, cacheRead: 0.01, cacheWrite: 0.1 },
  "gemini-robotics-er-2-preview": { input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1 },
  "gemini-robotics-er-2-streaming-preview": { input: 1, output: 5, cacheRead: 0, cacheWrite: 0 },
  "gemini-3.8-flash-tts": { input: 0.5, output: 9, cacheRead: 0, cacheWrite: 0 },
  "gemini-3.8-flash-lite-tts": { input: 0.5, output: 6, cacheRead: 0, cacheWrite: 0 },
  "gemini-3.1-flash-tts-preview": { input: 1, output: 20, cacheRead: 0, cacheWrite: 0 },
  "gemini-2.5-flash-preview-tts": { input: 0.5, output: 10, cacheRead: 0, cacheWrite: 0 },
  "gemini-2.5-pro-preview-tts": { input: 1, output: 20, cacheRead: 0, cacheWrite: 0 },
  "gemini-3.5-transcribe": { input: 2, output: 12, cacheRead: 0, cacheWrite: 0 },
  "gemini-3.5-transcribe-live": { input: 3.5, output: 21, cacheRead: 0, cacheWrite: 0 },
  "gemini-3.5-live-translate-preview": { input: 3.5, output: 21, cacheRead: 0, cacheWrite: 0 },

  // -- OpenAI: GPT-5 family (Codex CLI's default is gpt-5-codex; -codex/-mini/-nano variants priced per OpenAI tiers) --
  "gpt-5-codex":        { input: 1.25, output: 10,   cacheRead: 0.125, cacheWrite: 1.25 },
  "gpt-5-mini":         { input: 0.25, output: 2,    cacheRead: 0.025, cacheWrite: 0.25 },
  "gpt-5-nano":         { input: 0.05, output: 0.40, cacheRead: 0.005, cacheWrite: 0.05 },
  "gpt-5":              { input: 1.25, output: 10,   cacheRead: 0.125, cacheWrite: 1.25 },

  // -- OpenAI: o-series reasoning models --
  "o3-mini":            { input: 1.10, output: 4.40, cacheRead: 0.55,  cacheWrite: 1.10 },
  "o3":                 { input: 2,    output: 8,    cacheRead: 0.50,  cacheWrite: 2 },
  "o4-mini":            { input: 1.10, output: 4.40, cacheRead: 0.275, cacheWrite: 1.10 },

  // -- OpenAI: GPT-4o family (legacy, kept for runtimes still configured against it) --
  "gpt-4o-mini":        { input: 0.15, output: 0.60, cacheRead: 0.075, cacheWrite: 0.15 },
  "gpt-4o":             { input: 2.50, output: 10,   cacheRead: 1.25,  cacheWrite: 2.50 },

  // -- DeepSeek Flash: api-docs.deepseek.com/quick_start/pricing/ (2026-10-27).
  // The English sheet supplies USD; the Chinese sheet lists CNY. The official
  // Flash name and retired v4 aliases serve V4.1-Flash-1017. Preserve the
  // existing Flash routing for chat/reasoner. Only known spellings and the
  // 1017 snapshot resolve. Use peak USD rates because rollups lack each
  // request's pricing window; off-peak costs half as much and reseller prices
  // can differ. Cache writes bill as ordinary input. Mirror pricing.go.
  "deepseek-flash":              { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-v4-flash":           { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-v4-flash-vision-exp": { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-v4.1-flash":          { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-v4.1-flash-1017":     { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-chat":               { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-reasoner":           { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0.30 },
  "deepseek-v4-pro":    { input: 1.74, output: 3.48, cacheRead: 0.0145, cacheWrite: 1.74 },

  // -- Moonshot Kimi (kimi.com/resources/kimi-k2-6-pricing).
  //    Only K2.6 is on the official price sheet today; earlier K2 variants
  //    are intentionally omitted until Moonshot publishes their rates. --
  "kimi-k2.6":          { input: 0.95, output: 4.00, cacheRead: 0.16,   cacheWrite: 0.95 },
  // Kimi K3 (platform.kimi.ai/docs/pricing/chat-k3 via models.dev
  // providers/moonshotai/models/kimi-k3.toml). Moonshot bills no separate
  // cache write, so cacheWrite mirrors input (same convention as kimi-k2.6).
  "kimi-k3":            { input: 3.0,  output: 15.0,  cacheRead: 0.30,   cacheWrite: 3.0 },
  // Kimi Code CLI reports the same model as `kimi-code/k3`; provider-qualified
  // because `k3` is a generic id (see the provider-qualified keys note above).
  "kimi/k3":            { input: 3.0,  output: 15.0,  cacheRead: 0.30,   cacheWrite: 3.0 },

  // -- Zhipu z.ai (docs.z.ai/guides/overview/pricing). Free flash tiers
  //    are priced at 0 so they resolve cleanly instead of falling through
  //    to the "unmapped" diagnostic. --
  "glm-5.1":            { input: 1.4,  output: 4.4,  cacheRead: 0.26,   cacheWrite: 1.4 },
  "glm-5":              { input: 1.0,  output: 3.2,  cacheRead: 0.2,    cacheWrite: 1.0 },
  "glm-5-turbo":        { input: 1.2,  output: 4.0,  cacheRead: 0.24,   cacheWrite: 1.2 },
  "glm-4.7":            { input: 0.6,  output: 2.2,  cacheRead: 0.11,   cacheWrite: 0.6 },
  "glm-4.7-flashx":     { input: 0.07, output: 0.4,  cacheRead: 0.01,   cacheWrite: 0.07 },
  "glm-4.7-flash":      { input: 0,    output: 0,    cacheRead: 0,      cacheWrite: 0 },
  "glm-4.6":            { input: 0.6,  output: 2.2,  cacheRead: 0.11,   cacheWrite: 0.6 },
  "glm-4.5":            { input: 0.6,  output: 2.2,  cacheRead: 0.11,   cacheWrite: 0.6 },
  "glm-4.5-x":          { input: 2.2,  output: 8.9,  cacheRead: 0.45,   cacheWrite: 2.2 },
  "glm-4.5-air":        { input: 0.2,  output: 1.1,  cacheRead: 0.03,   cacheWrite: 0.2 },
  "glm-4.5-airx":       { input: 1.1,  output: 4.5,  cacheRead: 0.22,   cacheWrite: 1.1 },
  "glm-4.5-flash":      { input: 0,    output: 0,    cacheRead: 0,      cacheWrite: 0 },

  // -- Alibaba Qwen (International ≤256K tier; official sources:
  //    alibabacloud.com/help/model-studio pricing sheet and
  //    qwencloud.com/models/<model> pages, accessed 2026-08-12).
  //    qwen3.7-plus: input $0.40 / output $1.60; qwen3.6-flash: input
  //    $0.25 / output $1.50. Cache prices: Explicit Cache Creation = 1.25×
  //    input (qwen3.7-plus $0.50, qwen3.6-flash $0.3125); Explicit Cache
  //    Read = 10% of input (qwen3.7-plus $0.04, qwen3.6-flash $0.025).
  // qwen3.8-max is priced at the published pay-as-you-go rate
  // (qwencloud.com/models/qwen3.8-max: Input $2, Output $6, Implicit
  // Cache $0.25, Creation $2.5, Explicit Cache Read $0.17; also listed on
  // alibabacloud.com/help model-pricing) so the dashboard shows the
  // absolute cost even though the runtime reaches it through an Alibaba
  // Token/Coding Plan subscription. qwen3.8-max-preview stays at 0:
  //    it is only served through the subscription (token-plan.cn-beijing.
  //    maas.aliyuncs.com), which does not bill per token — 0 resolves
  //    cleanly instead of tripping the unmapped diagnostic (same convention
  //    as the free GLM flash tiers below). --
  "qwen3.7-plus":       { input: 0.40,  output: 1.60,  cacheRead: 0.04,   cacheWrite: 0.50 },
  "qwen3.6-flash":      { input: 0.25,  output: 1.50,  cacheRead: 0.025,  cacheWrite: 0.3125 },
  "qwen3.8-max":        { input: 2.00,  output: 6.00,  cacheRead: 0.17,   cacheWrite: 2.5 },
  "qwen3.8-max-preview":{ input: 0,      output: 0,     cacheRead: 0,      cacheWrite: 0 },

  // -- Volcengine Ark (ark.cn-beijing.volces.com). `ark-code-latest` is a
  //    rolling alias whose target the Volcengine console can switch between
  //    model families, so it is not a stable model identity. Daemons report
  //    the alias itself, not the resolved model, so there is no reliable
  //    rate to attach — it deliberately stays unmapped (same philosophy as
  //    xAI's `grok-composer-*`, see below), surfacing in the pricing dialog
  //    instead of inheriting a guessed rate. --

  // -- xAI Grok (docs.x.ai/developers/pricing). Rates below are the
  //    short-context tier, and are now only a FALLBACK for Grok: xAI reports
  //    its own price per turn and `estimateCost` prefers it. That matters
  //    because xAI bills a request at 2x once its prompt reaches 200K tokens,
  //    and a usage row aggregates every model call in a turn — so these rates
  //    cannot tell which tier a request hit, while xAI's own figure already
  //    has it priced in. These rows still apply to Grok usage recorded by a
  //    daemon too old to report cost (the same trade-off the Anthropic `[1m]`
  //    context tag takes, see `resolvePricing`).
  //    `cacheRead` is xAI's published "Cached" input rate; there is no
  //    separate cache-write rate on the page (writes bill as normal input),
  //    so cacheWrite mirrors input per the header note. Grok ids are
  //    vendor-prefixed, so these keys stay unqualified — which is what makes
  //    them resolve at all, since the daemon tags the rows with the runtime
  //    provider `grok`, not `xai`.
  //    `grok-composer-*` ships in the Grok Build catalog
  //    (server/pkg/agent/models.go) but is absent from the price sheet; it
  //    deliberately stays unmapped rather than inheriting a guessed rate. --
  "grok-4.6":                     { input: 2,    output: 6,    cacheRead: 0.50, cacheWrite: 2 },
  "grok-4.5":                     { input: 2,    output: 6,    cacheRead: 0.30, cacheWrite: 2 },
  "grok-4.3":                     { input: 1.25, output: 2.50, cacheRead: 0.20, cacheWrite: 1.25 },
  "grok-build-0.1":               { input: 1,    output: 2,    cacheRead: 0.20, cacheWrite: 1 },
  "grok-4.20-multi-agent-0309":   { input: 1.25, output: 2.50, cacheRead: 0.20, cacheWrite: 1.25 },
  "grok-4.20-0309-reasoning":     { input: 1.25, output: 2.50, cacheRead: 0.20, cacheWrite: 1.25 },
  "grok-4.20-0309-non-reasoning": { input: 1.25, output: 2.50, cacheRead: 0.20, cacheWrite: 1.25 },

  // -- Cursor models: Composer / Grok / Auto (cursor.com/docs/models-and-pricing,
  //    cursor.com/docs/models/cursor-composer-2,
  //    cursor.com/docs/models/cursor-composer-2-5).
  //    Cursor's model ids are all unprefixed generic names (`auto`,
  //    `composer-*`) that collide with other providers (another provider
  //    could also report `auto`), so they are provider-qualified under `cursor/`.
  //    See the `provider-qualified keys` note above. Cursor result events
  //    often omit `model`, so the daemon falls back to the configured
  //    runtime model or the legacy key `cursor`. Cursor does not publish a
  //    cache-write rate for these rows; keep it at 0 so reported
  //    cache_write_tokens don't invent spend from input pricing.
  "cursor/auto":              { input: 1.25, output: 6,    cacheRead: 0.25,   cacheWrite: 0 },
  "cursor/composer-2.5-fast": { input: 3,    output: 15,   cacheRead: 0.5,    cacheWrite: 0 },
  "cursor/composer-2.5":      { input: 0.5,  output: 2.5,  cacheRead: 0.2,    cacheWrite: 0 },
  "cursor/composer-2-fast":   { input: 1.5,  output: 7.5,  cacheRead: 0.35,   cacheWrite: 0 },
  "cursor/composer-2":        { input: 0.5,  output: 2.5,  cacheRead: 0.2,    cacheWrite: 0 },
  "cursor/composer-1.5":      { input: 3.5,  output: 17.5, cacheRead: 0.35,   cacheWrite: 0 },
  "cursor/composer-1":        { input: 1.25, output: 10,   cacheRead: 0.125,  cacheWrite: 0 },
  "cursor/grok-4.7":          { input: 2,    output: 6,    cacheRead: 0.5,    cacheWrite: 0 },
  "cursor/grok-4.7-fast":     { input: 4,    output: 12,   cacheRead: 1,      cacheWrite: 0 },
  "cursor/grok-4.6":          { input: 2,    output: 6,    cacheRead: 0.5,    cacheWrite: 0 },
  "cursor/grok-4.6-fast":     { input: 4,    output: 12,   cacheRead: 1,      cacheWrite: 0 },
  "cursor/grok-4.5":          { input: 2,    output: 6,    cacheRead: 0.5,    cacheWrite: 0 },
  "cursor/grok-4.5-fast":     { input: 4,    output: 18,   cacheRead: 1,      cacheWrite: 0 },

  // -- Third-party models through Cursor (cursor.com/docs/models-and-pricing,
  //    "Model pricing"). Cursor bills these at API list price, but its sheet
  //    is the bill, so Cursor rows use it even where it differs from the
  //    vendor rows above (GPT-5.6 Sol is on a Cursor promo through
  //    2026-11-21; update the row when it ends). Keys are the Cursor base id
  //    left by `cursorBaseModel`, which drops the effort / thinking / fast
  //    variant parts — effort and thinking don't change per-token rates.
  //    A `-fast` row exists only where Cursor publishes the Fast rate; other
  //    Fast variants stay unmapped rather than borrowing the standard rate.
  //    Where the sheet lists no cache-write rate, cacheWrite is 0 (same as the
  //    Composer rows). Long-context surcharges (GPT >272K input) can't be
  //    seen in aggregated rows, so those requests are underestimated. --
  "cursor/claude-opus-5-5":      { input: 4,    output: 20,   cacheRead: 0.2,   cacheWrite: 5 },
  "cursor/claude-opus-5":        { input: 5,    output: 25,   cacheRead: 0.5,   cacheWrite: 6.25 },
  "cursor/claude-opus-4-8":      { input: 5,    output: 25,   cacheRead: 0.5,   cacheWrite: 6.25 },
  // Published as "3x lower per-token pricing than Opus 4.7 fast mode".
  "cursor/claude-opus-4-8-fast": { input: 10,   output: 50,   cacheRead: 1,     cacheWrite: 12.5 },
  "cursor/claude-opus-4-7":      { input: 5,    output: 25,   cacheRead: 0.5,   cacheWrite: 6.25 },
  "cursor/claude-opus-4-7-fast": { input: 30,   output: 150,  cacheRead: 3,     cacheWrite: 37.5 },
  "cursor/claude-4.6-opus":      { input: 5,    output: 25,   cacheRead: 0.5,   cacheWrite: 6.25 },
  "cursor/claude-4.5-opus":      { input: 5,    output: 25,   cacheRead: 0.5,   cacheWrite: 6.25 },
  "cursor/claude-fable-5-1":     { input: 10,   output: 50,   cacheRead: 0.25,  cacheWrite: 12.5 },
  "cursor/claude-fable-5":       { input: 10,   output: 50,   cacheRead: 1,     cacheWrite: 12.5 },
  "cursor/claude-sonnet-5-5":    { input: 2,    output: 10,   cacheRead: 0.1,   cacheWrite: 2.5 },
  "cursor/claude-sonnet-5":      { input: 2,    output: 10,   cacheRead: 0.2,   cacheWrite: 2.5 },
  "cursor/claude-4.6-sonnet":    { input: 3,    output: 15,   cacheRead: 0.3,   cacheWrite: 3.75 },
  "cursor/claude-4.5-sonnet":    { input: 3,    output: 15,   cacheRead: 0.3,   cacheWrite: 3.75 },
  "cursor/claude-4-sonnet":      { input: 3,    output: 15,   cacheRead: 0.3,   cacheWrite: 3.75 },
  // <=100K prompt tier; see the claude-haiku-5-5 row above.
  "cursor/claude-haiku-5-5":     { input: 0.1,  output: 0.5,  cacheRead: 0.01,  cacheWrite: 0.125 },
  "cursor/gpt-5.6-sol":          { input: 4,    output: 20,   cacheRead: 0.4,   cacheWrite: 5 },
  "cursor/gpt-5.6-sol-fast":     { input: 8,    output: 40,   cacheRead: 0.8,   cacheWrite: 10 },
  "cursor/gpt-5.6-terra":        { input: 2,    output: 12,   cacheRead: 0.2,   cacheWrite: 2.5 },
  "cursor/gpt-5.6-terra-fast":   { input: 4,    output: 24,   cacheRead: 0.4,   cacheWrite: 5 },
  "cursor/gpt-5.6-luna":         { input: 0.2,  output: 1.2,  cacheRead: 0.02,  cacheWrite: 0.25 },
  "cursor/gpt-5.6-luna-fast":    { input: 0.4,  output: 2.4,  cacheRead: 0.04,  cacheWrite: 0.5 },
  "cursor/gpt-5.5":              { input: 5,    output: 30,   cacheRead: 0.5,   cacheWrite: 0 },
  "cursor/gpt-5.4":              { input: 2.5,  output: 15,   cacheRead: 0.25,  cacheWrite: 0 },
  "cursor/gpt-5.4-fast":         { input: 5,    output: 30,   cacheRead: 0.5,   cacheWrite: 0 },
  "cursor/gpt-5.4-mini":         { input: 0.75, output: 4.5,  cacheRead: 0.075, cacheWrite: 0 },
  "cursor/gpt-5.4-nano":         { input: 0.2,  output: 1.25, cacheRead: 0.02,  cacheWrite: 0 },
  "cursor/gpt-5.3-codex":        { input: 1.75, output: 14,   cacheRead: 0.175, cacheWrite: 0 },
  "cursor/gpt-5.2":              { input: 1.75, output: 14,   cacheRead: 0.175, cacheWrite: 0 },
  "cursor/gpt-5-mini":           { input: 0.25, output: 2,    cacheRead: 0.025, cacheWrite: 0 },
  "cursor/gemini-4-argon":       { input: 2,    output: 10,   cacheRead: 0.1,   cacheWrite: 0 },
  "cursor/gemini-3.8-flash":     { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0 },
  "cursor/gemini-3.7-flash":     { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0 },
  "cursor/gemini-3.6-flash":     { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0 },
  "cursor/gemini-3.5-flash":     { input: 0.75, output: 3.75, cacheRead: 0.075, cacheWrite: 0 },
  "cursor/gemini-3.1-pro":       { input: 2,    output: 12,   cacheRead: 0.2,   cacheWrite: 0 },
  "cursor/gemini-3-flash":       { input: 0.5,  output: 3,    cacheRead: 0.05,  cacheWrite: 0 },
  "cursor/kimi-k3":              { input: 3,    output: 15,   cacheRead: 0.3,   cacheWrite: 0 },
  "cursor/kimi-k2.7-code":       { input: 0.95, output: 4,    cacheRead: 0.19,  cacheWrite: 0 },
  "cursor/glm-5.2":              { input: 1.4,  output: 4.4,  cacheRead: 0.26,  cacheWrite: 0 },
  "cursor/muse-wonder-1.0":      { input: 4,    output: 20,   cacheRead: 0.2,   cacheWrite: 0 },
  "cursor/muse-spark-1.3":       { input: 1.25, output: 4.25, cacheRead: 0.15,  cacheWrite: 0 },

  // Legacy fallback bucket when neither the result event nor the runtime
  // model is known — the daemon emits the literal `cursor`. This key equals
  // the provider name itself, so it can't collide across providers and stays
  // unqualified. Price at the current Composer 2.5 Fast default.
  "cursor":                   { input: 3,    output: 15,   cacheRead: 0.5,    cacheWrite: 0 },
};

// Resolve a model string to its pricing tier. Exact match, with four
// tolerances applied in order (plus Gemini effort/display-name normalization):
//
//  1. Provider-prefixed IDs (`anthropic/claude-opus-4.7` from openclaw /
//     opencode) — the `<provider>/` segment is routing metadata, not part
//     of the SKU, so we strip it before lookup.
//  2. Anthropic dot↔dash normalization — Claude Code reports
//     `claude-opus-4-7`, GitHub Copilot reports `claude-opus-4.7`. Same
//     SKU, two transports. We canonicalize `claude-*` IDs to the dashed
//     form Anthropic itself publishes. Scoped to `claude-*` because for
//     OpenAI the separator IS semantic (`gpt-5.4` ≠ `gpt-5-4`).
//  3. Trailing dated snapshots (`claude-sonnet-4-5-20250929`,
//     `gpt-5-2025-08-07`) — the family is what we price, the date is
//     volatile, so we strip a trailing date / "latest" tag.
//  4. Trailing context-window tag (`claude-opus-4-7[1m]`) — Anthropic's
//     1M-context beta is the same SKU at standard rates for prompts
//     ≤200K input tokens, with a 2× surcharge above that. Aggregated
//     usage rows don't carry per-request prompt sizes, so we price the
//     bracketed variant at the standard tier. Slight under-estimate
//     beats the previous behaviour of dropping the row entirely.
//
// Anything still unmapped falls back to the user-supplied custom pricing
// store. No startsWith fallback: variants like `gpt-5.5-mini` must have
// their own row to be priced (otherwise they'd inherit `gpt-5.5`).
//
// `provider` disambiguates unprefixed generic ids (see the header note):
// every candidate is tried `${provider}/…`-qualified first, then bare, so a
// `cursor/auto` row wins for a Cursor row while an unqualified `auto` (no
// provider) stays unmapped instead of silently borrowing Cursor's price.
function resolvePricing(model: string, provider?: string) {
  if (!model) return undefined;

  const candidates = pricingCandidates(model, provider);
  for (const candidate of candidates) {
    const hit = MODEL_PRICING[candidate];
    if (hit) return hit;
  }
  for (const candidate of candidates) {
    const hit = getCustomPricing(candidate);
    if (hit) return hit;
  }
  return undefined;
}

// Canonical provider token for keying: trimmed + lowercased so lookup keys,
// storage keys, and grouping labels all tolerate case drift in the stored
// value. Returns "" when no provider is known.
function normalizeProvider(provider?: string): string {
  return provider?.trim().toLowerCase() ?? "";
}

// Provider-qualify a key, skipping the prefix when the key already carries
// this provider (an upstream-qualified `cursor/auto` must not become
// `cursor/cursor/auto`). `provider` must already be normalized.
function qualify(provider: string, key: string): string {
  return key.startsWith(`${provider}/`) ? key : `${provider}/${key}`;
}

// Lookup keys for a (model, provider) pair: every canonical candidate
// `${provider}/`-qualified first (when a provider is known), then the bare
// candidates. Qualified-first means a provider-scoped row/override always
// beats an unqualified one.
function pricingCandidates(model: string, provider?: string): string[] {
  const base = canonicalCandidates(model);
  const p = normalizeProvider(provider);
  if (!p) return base;
  if (p === "cursor") return cursorPricingCandidates(model, base);
  return [...base.map((c) => qualify(p, c)), ...base];
}

// Cursor's catalog ids are a base model plus variant parts, in no fixed
// order: `claude-opus-4-7-thinking-high-fast`, `claude-4.6-sonnet-medium-thinking`,
// `cursor-grok-4.6-high`, or the parameterized
// `claude-opus-4-8[context=1m,effort=high,fast=true]`.
const CURSOR_VARIANT_SUFFIX = /-(?:fast|thinking|none|minimal|low|medium|high|xhigh|extra-high|max)$/;

function cursorBaseModel(model: string): { base: string; fast: boolean } {
  let id = model.trim().toLowerCase();
  let fast = false;
  const params = id.match(/\[([^\]]*)\]$/);
  if (params) {
    fast = /(?:^|,)\s*fast\s*=\s*true\s*(?:,|$)/.test(params[1] ?? "");
    id = id.slice(0, params.index);
  }
  for (let m = id.match(CURSOR_VARIANT_SUFFIX); m; m = id.match(CURSOR_VARIANT_SUFFIX)) {
    if (m[0] === "-fast") fast = true;
    id = id.slice(0, m.index);
  }
  return { base: id.replace(/^cursor-/, ""), fast };
}

// Fast mode bills above the standard rate, so a Fast variant resolves only
// through its exact id or its own `cursor/<base>-fast` row — never through a
// canonical form (`[fast=true]` stripped as a context tag) or the base
// model's price. Standard variants try the exact ids, then the Cursor base
// row, then the bare vendor row for the same SKU.
function cursorPricingCandidates(model: string, canonical: string[]): string[] {
  const { base, fast } = cursorBaseModel(model);
  if (fast) return [qualify("cursor", model), `cursor/${base}-fast`];
  const qualified = canonical.map((c) => qualify("cursor", c));
  if (!base) return [...qualified, ...canonical];
  return [...new Set([...qualified, `cursor/${base}`, base, ...canonical])];
}

// The canonical storage/diagnostic key for a (model, provider) pair: the
// provider-qualified form when a provider is known, else the bare model.
// `collectUnmappedModels` returns these, and the custom-pricing dialog keys
// overrides by them, so a user-entered rate for `cursor/auto` resolves only
// for Cursor rows — not for another provider that also reports `auto`.
// Provider is lowercased so lookups tolerate case drift in the stored value.
export function pricingKey(model: string, provider?: string): string {
  const p = normalizeProvider(provider);
  return p ? qualify(p, model) : model;
}

// Display/grouping key for a usage row's model. Self-resolving ids
// (vendor-prefixed SKUs like `claude-opus-4-7`, and the legacy `cursor`
// fallback whose key equals the provider name) stay bare; a generic id that
// only prices under a provider (`auto`, `composer-*`) is provider-qualified
// so two providers reporting the same bare id don't merge into one mislabelled
// row, and the label matches what `collectUnmappedModels` / the pricing dialog
// surface.
export function modelGroupingKey(model: string, provider?: string): string {
  if (!model) return normalizeProvider(provider) || "unknown";
  return isSelfResolvingId(model) ? model : pricingKey(model, provider);
}

// Whether a model id prices on its own without a provider qualifier (a
// vendor-prefixed SKU, or the legacy `cursor` fallback). Such ids keep a bare
// grouping key; generic ids (`auto`, `composer-*`) stay provider-qualified.
//
// Probes the BARE model on purpose: forwarding a provider would let a
// qualified row report as self-resolving and collapse back to a bare key,
// re-merging the cross-provider collision this scheme prevents. Keep the
// argument list provider-free so that stays true.
function isSelfResolvingId(model: string): boolean {
  return isModelPriced(model);
}

// Generate the lookup candidates for a model string, in priority order:
// the raw string first (preserves explicit user / catalog spellings),
// then the canonicalized forms. Deduped so we don't repeat lookups.
//
// Pure in `model`, and the aggregation loops call it 3-4x per row, so the
// result is memoized — the model-string set is small and bounded. Callers
// only read the array (pricingCandidates maps/spreads into a fresh one), so
// sharing the cached reference is safe.
// Intentionally process-lifetime: never evicted (bounded key set, see above).
const canonicalCandidatesCache = new Map<string, string[]>();
function canonicalCandidates(model: string): string[] {
  const cached = canonicalCandidatesCache.get(model);
  if (cached) return cached;
  const seen = new Set<string>();
  const out: string[] = [];
  const push = (s: string) => {
    if (!s || seen.has(s)) return;
    seen.add(s);
    out.push(s);
  };
  const stripDate = (s: string) =>
    s.replace(/-(20\d{2}-\d{2}-\d{2}|20\d{6}|latest)$/, "");
  const stripProvider = (s: string) => {
    // Routing prefixes come in two flavours: `vendor/model` (opencode-style)
    // and `provider:model` (Hermes custom providers), and can nest
    // (`custom:anthropic/claude-opus-4.7` is a provider-prefixed id whose
    // model segment is itself provider-prefixed). Iteratively strip the
    // earliest `/` or `:` separator while the preceding segment looks like a
    // routing layer (`^[a-z][a-z0-9_-]*$`), until nothing valid remains to
    // strip. The raw string is always the first candidate (see `push(raw)`
    // below), so provider-qualified table keys like `cursor/composer-2.5`
    // still resolve before any stripping happens — iterative peeling only
    // ever adds previously-missed nested forms.
    let out = s;
    for (;;) {
      const i = out.indexOf("/");
      const j = out.indexOf(":");
      const sep = i === -1 ? j : j === -1 ? i : Math.min(i, j);
      if (sep <= 0 || !/^[a-z][a-z0-9_-]*$/i.test(out.slice(0, sep))) break;
      out = out.slice(sep + 1);
    }
    return out;
  };
  // Only Anthropic IDs are dot↔dash equivalent. OpenAI separators are
  // semantic, so we leave `gpt-5.4` etc. alone.
  const canonAnthropic = (s: string) =>
    s.startsWith("claude-") ? s.replace(/\./g, "-") : s;
  // Amazon Bedrock prefixes Anthropic ids with a region family (`us.`, `eu.`,
  // `apac.`, `global.`) and the vendor (`anthropic.`), and appends the
  // inference-profile version to snapshot ids (`...-20250929-v1:0`). A Bedrock
  // turn reports `us.anthropic.claude-sonnet-5`, which matched no table key at
  // all, so every such turn rendered as $0 plus an "unmapped" warning. Only the
  // vendor-prefixed forms are rewritten: `us.amazon.nova-*` and friends stay
  // unmapped rather than borrowing a Claude rate. Mirrors `normalizeBedrockID`
  // in server/internal/metrics/pricing.go.
  const stripBedrock = (s: string) => {
    const prefix = /^(?:(?:us|eu|apac|global)\.)?anthropic\.(claude-)/;
    if (!prefix.test(s)) return s;
    return s
      .replace(prefix, "$1")
      .replace(/-v\d+:\d+(\[[^\]]+\])?$/, "$1");
  };
  // Trailing context-window tag (`claude-opus-4-7[1m]`). Same family,
  // same price tier — see resolver comment above for the 1M-context
  // pricing trade-off.
  const stripContextTag = (s: string) => s.replace(/\[[^\]]+\]$/, "");

  // Antigravity's display labels and effort-suffixed IDs refer to the same
  // Gemini SKU. Match only recognized shapes; never strip arbitrary variants.
  const canonGemini = (s: string) => {
    const display = /^gemini (\d+(?:\.\d+)?) (flash(?:-lite)?|pro|argon)(?: \((?:minimal|low|medium|high)\))?$/i.exec(s);
    if (display) return `gemini-${display[1]}-${display[2]}`.toLowerCase();
    const slug = /^(gemini-\d+(?:\.\d+)?-(?:flash(?:-lite)?|pro|argon))(?:-(?:minimal|low|medium|high))?$/i.exec(s);
    return slug ? slug[1]!.toLowerCase() : s;
  };

  const raw = model;
  const noProvider = stripProvider(raw);
  const dashed = canonAnthropic(noProvider);
  const noTag = stripContextTag(dashed);
  // Bedrock ids reach us with dots (`us.anthropic.claude-…`) and a
  // `-v1:0` profile version, so they need the prefix strip BEFORE the
  // dot↔dash normalisation that `canonAnthropic` only applies to `claude-*`.
  const bedrock = canonAnthropic(stripContextTag(stripBedrock(noProvider)));

  push(raw);
  push(noProvider);
  push(dashed);
  push(noTag);
  push(bedrock);
  push(stripDate(raw));
  push(stripDate(noProvider));
  push(stripDate(dashed));
  push(stripDate(noTag));
  push(stripDate(bedrock));
  push(canonGemini(noTag));
  push(canonGemini(stripDate(noTag)));
  canonicalCandidatesCache.set(model, out);
  return out;
}

// Cheap predicate for the empty-state diagnostic: which model strings in a
// usage batch failed pricing resolution. Useful when the user is staring at
// "$0.00 / 2M tokens" and wants to know why.
export function isModelPriced(model: string, provider?: string): boolean {
  return resolvePricing(model, provider) !== undefined;
}

// Returns the unique, sorted list of pricing keys present in `rows` that
// don't resolve to a price. Keys are provider-qualified (`cursor/auto`) when
// the row carries a provider, so the same bare model id reported by two
// providers surfaces as two distinct entries the user can price separately.
// Empty when everything's priced or there are no rows.
// A row the provider priced in full needs no rate-table entry, so it must not
// raise the "we can't price this model" warning — its cost is already exact,
// and asking the user to supply a rate for it would be asking them to override
// a real bill with a guess.
export function collectUnmappedModels(rows: readonly Priceable[]): string[] {
  const set = new Set<string>();
  for (const r of rows) {
    if (!r.model || isModelPriced(r.model, r.provider)) continue;
    const uncosted = uncostedTokens(r);
    const needsEstimate =
      uncosted.input > 0 ||
      uncosted.output > 0 ||
      uncosted.cacheRead > 0 ||
      uncosted.cacheWrite > 0;
    if (!needsEstimate && (r.cost_usd_ticks ?? 0) > 0) continue;
    set.add(pricingKey(r.model, r.provider));
  }
  return Array.from(set).toSorted();
}

// Anything carrying per-model token totals can be priced — RuntimeUsage,
// RuntimeUsageByAgent, RuntimeUsageByHour all share this shape on purpose
// (the back-end keeps the model dimension specifically so the client can
// run this calculation for any aggregation axis).
// `provider` is optional so callers with provider-less rows (and existing
// test fixtures) still type-check; when present it disambiguates generic
// model ids during pricing. RuntimeUsage / RuntimeUsageByAgent /
// DashboardUsageDaily / DashboardUsageByAgent all carry it on the wire.
export type Priceable = Pick<
  RuntimeUsage,
  | "model"
  | "input_tokens"
  | "output_tokens"
  | "cache_read_tokens"
  | "cache_write_tokens"
> & {
  provider?: string;
  cost_usd_ticks?: number;
  uncosted_input_tokens?: number;
  uncosted_output_tokens?: number;
  uncosted_cache_read_tokens?: number;
  uncosted_cache_write_tokens?: number;
};

// Providers report cost in ticks of 1e-10 USD (xAI's unit), which keeps
// sub-cent turn costs exact as integers all the way from the agent to here.
export const COST_USD_TICKS_PER_USD = 10_000_000_000;

// The tokens in a row that still need pricing from the table above, i.e. the
// ones no provider priced for us.
//
// A backend older than the cost split sends no `uncosted_*` at all. Treating
// that as "0 tokens left to estimate" would report $0 for every row, so
// `undefined` falls back to the full token counts — exactly the pre-split
// behaviour. The one exception is a row that DOES carry an authoritative cost
// without the split: adding a full-token estimate on top would double-charge
// it, so the authoritative figure stands alone. A current backend always sends
// both, so this only guards against version drift.
function uncostedTokens(usage: Priceable): {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
} {
  if (usage.uncosted_input_tokens === undefined) {
    if ((usage.cost_usd_ticks ?? 0) > 0) {
      return { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
    }
    return {
      input: usage.input_tokens,
      output: usage.output_tokens,
      cacheRead: usage.cache_read_tokens,
      cacheWrite: usage.cache_write_tokens,
    };
  }
  return {
    input: usage.uncosted_input_tokens,
    output: usage.uncosted_output_tokens ?? 0,
    cacheRead: usage.uncosted_cache_read_tokens ?? 0,
    cacheWrite: usage.uncosted_cache_write_tokens ?? 0,
  };
}

// Cost of a usage row: what the provider actually charged, plus a rate-table
// estimate for whatever it didn't charge for.
//
// The rate table cannot express request-level pricing rules — xAI bills a Grok
// request at 2x once its prompt reaches 200K tokens, and these rows aggregate
// every model call in a turn, so the token counts genuinely cannot say which
// tier a given request hit. Where the provider tells us its own price, that
// number is the bill and no estimate can improve on it.
//
// Both halves are summed rather than one winning outright because a single row
// can aggregate both kinds of source row (two providers in a bucket, or Grok
// either side of a CLI upgrade). Custom pricing overrides still apply — but
// only to the estimated half, since they are a user's guess at a rate and the
// authoritative half is not a guess.
export function estimateCost(usage: Priceable): number {
  const authoritative = (usage.cost_usd_ticks ?? 0) / COST_USD_TICKS_PER_USD;
  const pricing = resolvePricing(usage.model, usage.provider);
  if (!pricing) return authoritative;
  const uncosted = uncostedTokens(usage);
  return (
    authoritative +
    (uncosted.input * pricing.input +
      uncosted.output * pricing.output +
      uncosted.cacheRead * pricing.cacheRead +
      uncosted.cacheWrite * pricing.cacheWrite) /
      1_000_000
  );
}

export interface CostBreakdown {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

// Per-token-type split of `estimateCost`. The estimated half splits naturally;
// the authoritative half arrives as one number per row, so it is distributed
// across the buckets in the same proportions the rate table would have charged.
// Only the total is authoritative — the split is presentation, and doing it
// this way keeps the stacked chart summing to the headline figure instead of
// silently under-drawing every Grok row.
export function estimateCostBreakdown(usage: Priceable): CostBreakdown {
  const pricing = resolvePricing(usage.model, usage.provider);
  if (!pricing) {
    // No rates to split by, but the provider may still have priced the turn
    // itself. Returning zeros here would make the stacked chart disagree with
    // the headline `estimateCost` on exactly the rows whose cost is EXACT, so
    // the charge lands whole in one bucket instead.
    return {
      input: (usage.cost_usd_ticks ?? 0) / COST_USD_TICKS_PER_USD,
      output: 0,
      cacheRead: 0,
      cacheWrite: 0,
    };
  }
  const uncosted = uncostedTokens(usage);
  const breakdown: CostBreakdown = {
    input: (uncosted.input * pricing.input) / 1_000_000,
    output: (uncosted.output * pricing.output) / 1_000_000,
    cacheRead: (uncosted.cacheRead * pricing.cacheRead) / 1_000_000,
    cacheWrite: (uncosted.cacheWrite * pricing.cacheWrite) / 1_000_000,
  };

  const authoritative = (usage.cost_usd_ticks ?? 0) / COST_USD_TICKS_PER_USD;
  if (authoritative <= 0) return breakdown;

  // Shape the authoritative charge like the rate table would have priced the
  // tokens it covers — the row's full tokens minus the estimated ones.
  const shape = {
    input: ((usage.input_tokens - uncosted.input) * pricing.input) / 1_000_000,
    output: ((usage.output_tokens - uncosted.output) * pricing.output) / 1_000_000,
    cacheRead:
      ((usage.cache_read_tokens - uncosted.cacheRead) * pricing.cacheRead) / 1_000_000,
    cacheWrite:
      ((usage.cache_write_tokens - uncosted.cacheWrite) * pricing.cacheWrite) / 1_000_000,
  };
  const shapeTotal = shape.input + shape.output + shape.cacheRead + shape.cacheWrite;
  if (shapeTotal <= 0) {
    // Nothing to shape it with (unpriced tokens, or a row carrying cost but no
    // tokens). Keep the money in the total rather than dropping it.
    return { ...breakdown, input: breakdown.input + authoritative };
  }
  const scale = authoritative / shapeTotal;
  return {
    input: breakdown.input + shape.input * scale,
    output: breakdown.output + shape.output * scale,
    cacheRead: breakdown.cacheRead + shape.cacheRead * scale,
    cacheWrite: breakdown.cacheWrite + shape.cacheWrite * scale,
  };
}

// Cache savings: what cache *reads* would have cost at full input pricing
// minus what they actually cost at the discounted cache-hit rate. This is a
// reconstruction of "money the cache saved you", not real-world spend.
export function estimateCacheSavings(usage: Priceable): number {
  const pricing = resolvePricing(usage.model, usage.provider);
  if (!pricing) return 0;
  const wouldHaveCost = (usage.cache_read_tokens * pricing.input) / 1_000_000;
  const actualCost = (usage.cache_read_tokens * pricing.cacheRead) / 1_000_000;
  return wouldHaveCost - actualCost;
}
