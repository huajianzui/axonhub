# Subscription Accounts

Subscription channels (Codex, Claude, Antigravity, xAI) authenticate with OAuth rather than an API key. This guide explains how to hold several subscription logins on one channel.

## What a subscription account is

**One subscription account is one OAuth authorization.** A channel may hold several, and requests are spread across them.

What that buys you:

| Without accounts | With accounts |
|------------------|---------------|
| One subscription runs out and you swap it manually | Several subscriptions sit in one channel and are used automatically |
| One account gets rate limited and the channel goes dark | Only that account steps aside; the others keep serving |
| Adding a subscription means a new channel and a full model setup | Adding a subscription means adding an account |

## Where accounts come from

**Existing channels migrate automatically.** On upgrade, every subscription channel that already held an OAuth credential gets one account, and the channel keeps its own credential untouched — so the upgrade does not disturb a serving channel, and rolling back is safe.

New accounts come from an authorization flow you complete.

## Adding an account

1. Open **Channels** and open the row menu (⋮) for the channel
2. Choose **Accounts**
3. Click **Add account**
4. Paste the result of the authorization flow into **Credential**
5. Optionally set **Label** (usually the account email, to tell accounts apart)
6. Click **Add**

### Where the credential comes from

First run an authorization in the channel dialog (`Start OAuth`). That produces a credential:

- **Codex / Claude Code / xAI**: a JSON blob such as `{"access_token":"...","refresh_token":"..."}`
- **Antigravity**: `<refreshToken>|<projectID>`

Paste that result into the account dialog. The channel's type decides how the server reads it, so you never have to state the format.

> Adding the same credential twice does not create a duplicate — the existing account is reused.

## Account state

| State | Meaning |
|-------|---------|
| **Ready** | Can serve requests |
| **Refreshing** | Exchanging for a new access token |
| **Reauthorization required** | Upstream rejected the grant; authorize again |
| **Unknown** | The refresh outcome is uncertain, and the system will not guess |

There is also a separate **Paused** flag, controlled by you and independent of the authorization state. A paused account keeps its credential and its history, and can be resumed at any time.

**Only accounts that are Ready and not paused receive requests.**

## Routing behaviour

- Requests are spread by account **weight**, heaviest first
- **A conversation stays on the account it started with**, so editing weights does not disturb work in progress — which matters for upstream prompt caching
- When an account fails authentication, **only that account is disabled**; the channel is disabled only once it has no usable account left
- Removing an account cannot take a channel out of service: after the last account is removed the channel falls back to its own credential

## Common questions

**Why did a new account not take effect immediately?**

A channel's account set is loaded when the channel cache is rebuilt. The console triggers that rebuild on any account change, so this is normally automatic; if it has not taken effect, edit and save the channel to force it.

**How do I recover an account that was disabled?**

Re-enable it in the account list. If it reads "Reauthorization required", run the authorization flow again in the channel dialog and add the resulting account.

**Why can I not see the credential?**

Credentials are never returned by the API. The console reads identity, state, weight and expiry only.

**What are the limits for Antigravity?**

Account selection works for Antigravity, but **token refresh remains channel-level** rather than per account. Making it per account requires changing its transformer, which currently takes the credential as a string and builds its token provider at construction time. The other three subscription types refresh per account.

---

# For maintainers

The constraints below are not style preferences. Breaking any of them produces a failure that is hard to trace back.

## 1. The credential fingerprint must come from the stable identity

An account's per-channel uniqueness is built on `credential_fingerprint`, which **must** be computed from the stable part of the authorization — the refresh token, or the legacy credential string when there is no refresh token.

Two reasons:

1. **Parsing rewrites the credential.** When upstream sends no expiry, parsing injects the current time, so two parses of one authorization serialize **differently**. Fingerprinting the serialized form makes a retried import look like a new account.
2. **The access token is replaced on every refresh.** Fingerprinting it makes a refreshed account look like a different one.

See `internal/objects/account_fingerprint.go`.

## 2. The channel cache only reloads when a channel row moves

`reloadEnabledChannels` returns early when the channel row's `updated_at` has not advanced:

```go
} else if !latestUpdatedChannel.UpdatedAt.After(lastUpdate) {
    return current, lastUpdate, false, nil
}
```

Accounts live in their **own table**, so adding, changing or removing one does **not** touch the channel row. Every account write must therefore **explicitly touch the channel row** (`ChannelAccountService.TouchChannel`), or the runtime will not see the change.

## 3. The request context is a shared mutable container

`contexts.WithChannelAPIKey` writes into a **shared container** guarded by a mutex, not an ordinary context value. When the request already carries a container the write is visible to every sharer, and the production path installs one in `orchestrator` and in the trace middleware.

But `oauth.TokenGetter`'s context argument is passed **by value**, and the interface cannot hand a derived context back to its caller. The getter must therefore call `contexts.EnsureContainer` before recording its choice, or the record is lost and the failure is attributed to the whole channel — **which disables every account on it**.

## 4. A new ent entity needs three things filled in by hand

After the entity is generated, these three are **not** automatic, and the symptom is misleading:

1. **The `channelAccounts` query resolver** is a `panic("not implemented")` stub that must be implemented (`internal/server/gql/ent.resolvers.go`).
2. **The `id` and `channelID` field resolvers** are stubs too, and must return an `objects.GUID`.
3. **`guidTypeToNodeType`** (`internal/server/gql/graphql.go`) is a **hand-written allowlist**. Omitting an entry is easy to misread: **every other field of the object resolves fine and only `id` fails** with `unknown node type`.

`internal/server/gql/graphql_node_types_test.go` pins the third.

## 5. Security boundaries

- `ChannelAccount.credentials` is marked `Sensitive()`, so it does **not** appear in the GraphQL schema. That is what keeps credentials from leaking; do not add a GraphQL field for it for convenience.
- Permissions reuse the channel scopes (`read_channels` / `write_channels`).
- Account writes live in REST (`/admin/channels/:id/accounts`) because adding an account takes the opaque string an authorization flow returns, which is a REST shape rather than a GraphQL input.

## 6. Known gaps

| Gap | Impact | What it needs |
|-----|--------|---------------|
| Antigravity refresh is channel-level | Those channels cannot refresh per account | Change `llm/transformer/antigravity` to resolve a credential per request instead of building a provider at construction |
| Account weights are ordered at load time | Unchanged for one-account channels; heaviest account first when there are several | No change needed, but worth knowing |

## Key files

| File | Responsibility |
|------|----------------|
| `internal/ent/schema/channel_account.go` | Entity and indexes |
| `internal/ent/migrate/datamigrate/v1.0.0-beta11.go` | Backfill from inline credentials |
| `internal/objects/account_credential.go` | Round trip between credential shapes |
| `internal/objects/account_fingerprint.go` | Stable fingerprint |
| `internal/server/biz/channel_account.go` | Account service, routable accounts, projection |
| `internal/server/biz/channel_account_token.go` | Per-request account selection and per-account refresh |
| `internal/server/biz/channel_account_create.go` | Add an account from a credential |
| `internal/server/api/channel_account.go` | REST write endpoints |
