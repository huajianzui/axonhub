package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/scopes"
)

// ChannelAccount is one authorization grant on a channel.
//
// OAuth channels (Codex, Claude Code, Antigravity, xAI) historically held a
// single credential inlined on the channel row, so a channel could serve
// exactly one subscription account. A ChannelAccount is that credential lifted
// out into its own row, which lets one channel hold several accounts and lets
// the failover and quota machinery address them independently.
//
// Accounts are exposed read-only over GraphQL. Creating, re-authorizing and
// deleting a grant goes through the provider OAuth endpoints, which own the
// credential handling; there is no mutation input that accepts raw credentials.
type ChannelAccount struct {
	ent.Schema
}

func (ChannelAccount) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
		schematype.SoftDeleteMixin{},
	}
}

func (ChannelAccount) Indexes() []ent.Index {
	return []ent.Index{
		// A grant is unique per channel. The credential fingerprint is always
		// computable, unlike the account identity, which providers only reveal
		// once a grant has been parsed. Re-importing the same credential
		// therefore collides, which is what lets the account service update the
		// grant in place instead of duplicating it.
		index.Fields("channel_id", "credential_fingerprint", "deleted_at").
			StorageKey("channel_accounts_by_channel_credential").
			Unique(),
		// Locates an existing account by upstream identity when re-authorizing.
		// Not unique: the fingerprint above already enforces one row per grant,
		// and identity is optional, so several rows may legitimately lack one.
		index.Fields("channel_id", "identity", "deleted_at").
			StorageKey("channel_accounts_by_channel_identity"),
		index.Fields("channel_id", "deleted_at").
			StorageKey("channel_accounts_by_channel"),
		// Account selection scans the enabled accounts of a channel.
		index.Fields("channel_id", "enabled", "deleted_at").
			StorageKey("channel_accounts_by_channel_enabled"),
	}
}

func (ChannelAccount) Fields() []ent.Field {
	return []ent.Field{
		field.Int("channel_id").Immutable(),

		field.String("name").
			Optional().
			Default("").
			Comment("Operator-facing label. Often the account email, but never required to be."),

		field.String("identity").
			Optional().
			Default("").
			Comment("Upstream account identity, e.g. the Google account id or the Anthropic account/organization uuid. Empty when the provider does not reveal one; the credential fingerprint still identifies the grant."),

		field.String("identity_fingerprint").
			Optional().
			Default("").
			Comment("Stable digest of the identity, used to detect re-authorization of the same account. Empty when the identity is unknown."),

		field.String("credential_fingerprint").
			Comment("Stable digest of the credential itself. Always computable, so it is what makes a grant unique per channel."),

		field.JSON("credentials", map[string]any{}).
			Sensitive().
			Comment("The granted credential. Its shape is owned by the provider transformer, not by this schema."),

		field.Enum("auth_state").
			Values("ready", "refreshing", "reauthorization_required", "outcome_unknown").
			Default("ready").
			Comment("Lifecycle of the grant. Only a ready account serves traffic."),

		field.String("auth_error_code").
			Optional().
			Default("").
			Comment("Machine-readable reason the account is not ready. Safe to log: never carries credential material."),

		field.Bool("enabled").
			Default(true).
			Comment("Operator-controlled switch, independent of auth_state."),

		field.Int("weight").
			Default(50).
			Comment("Relative share of traffic among a channel's accounts. Zero pauses the account while keeping its history."),

		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("When the access token expires. Nil when the grant does not expire."),

		field.Time("last_refresh_at").
			Optional().
			Nillable().
			Comment("Last successful token refresh, for observability."),
	}
}

func (ChannelAccount) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("channel", Channel.Type).
			Ref("accounts").
			Field("channel_id").
			Required().
			Immutable().
			Unique(),
	}
}

func (ChannelAccount) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.RelayConnection(),
		entgql.QueryField("channelAccounts"),
	}
}

// Policy mirrors Channel: accounts are a sub-resource of a channel, so they
// require the channel read scope.
func (ChannelAccount) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.APIKeyScopeQueryRule(scopes.ScopeReadChannels),
			scopes.OwnerRule(),
			scopes.UserReadScopeRule(scopes.ScopeReadChannels),
		},
		Mutation: scopes.MutationPolicy{
			scopes.OwnerRule(),
			scopes.UserWriteScopeRule(scopes.ScopeWriteChannels),
		},
	}
}
