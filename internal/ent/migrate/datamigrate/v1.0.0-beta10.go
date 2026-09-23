package datamigrate

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xtime"
)

// V1_0_0_Beta10 implements DataMigrator for version 1.0.0-beta10.
type V1_0_0_Beta10 struct{}

// NewV1_0_0_Beta10 creates the v1.0.0-beta10 data migrator.
func NewV1_0_0_Beta10() *V1_0_0_Beta10 {
	return &V1_0_0_Beta10{}
}

// Version returns the migration version.
func (v *V1_0_0_Beta10) Version() string {
	return "v1.0.0-beta10"
}

// Migrate lifts every subscription channel's inline OAuth credential into a
// ChannelAccount row.
//
// OAuth channels used to keep a single credential inlined on the channel row, so
// a channel could serve exactly one account. Accounts are now their own entity,
// and this backfills one account per channel that already holds a grant.
//
// It is a pure copy: the channel's own credential is deliberately left in place
// and remains authoritative, so rolling back to the previous version still finds
// every credential it needs. Nothing reads the new rows yet.
//
// The account stores the channel's whole credential object rather than a single
// token field. That keeps the backfill lossless for every provider shape,
// including Antigravity, whose grant lives in the legacy APIKey field as
// "<refreshToken>|<projectID>" rather than in the OAuth object.
//
// Idempotent: a channel that already has an account for the same credential is
// skipped, so a re-run after a partial failure converges.
func (v *V1_0_0_Beta10) Migrate(ctx context.Context, client *ent.Client) error {
	ctx = authz.WithSystemBypass(ctx, "database-migrate")

	channels, err := client.Channel.Query().
		Where(channel.TypeIn(subscriptionChannelTypes()...)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("query subscription channels: %w", err)
	}

	var created int

	for _, ch := range channels {
		migrated, err := migrateChannel(ctx, client, ch)
		if err != nil {
			return fmt.Errorf("migrate channel %d (%s): %w", ch.ID, ch.Name, err)
		}

		if migrated {
			created++
		}
	}

	if created > 0 {
		log.Info(ctx, "backfilled channel accounts from inline oauth credentials",
			log.Int("channels_scanned", len(channels)),
			log.Int("accounts_created", created),
		)
	}

	return nil
}

// migrateChannel backfills one channel and reports whether it created a row.
func migrateChannel(ctx context.Context, client *ent.Client, ch *ent.Channel) (bool, error) {
	if !holdsSubscriptionGrant(ch) {
		return false, nil
	}

	raw, err := objects.AccountCredentialFromChannelCredentials(ch.Credentials)
	if err != nil {
		return false, err
	}

	fingerprint, err := objects.AccountCredentialFingerprint(raw)
	if err != nil {
		return false, err
	}

	exists, err := client.ChannelAccount.Query().
		Where(
			channelaccount.ChannelIDEQ(ch.ID),
			channelaccount.CredentialFingerprintEQ(fingerprint),
		).
		Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("check existing account: %w", err)
	}

	if exists {
		return false, nil
	}

	builder := client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetCredentials(raw).
		SetCredentialFingerprint(fingerprint).
		SetEnabled(true)

	// Only a real OAuth object carries an expiry we can trust; Antigravity's
	// legacy string has none.
	if creds, resolveErr := ch.Credentials.ResolveOAuthCredentials(); resolveErr == nil && creds != nil && !creds.ExpiresAt.IsZero() {
		builder = builder.SetExpiresAt(creds.ExpiresAt)
	}

	if _, err := builder.Save(ctx); err != nil {
		return false, fmt.Errorf("create account: %w", err)
	}

	// Bump the channel so its cached snapshot is rebuilt: the channel cache only
	// reloads when a channel row moves, and the account lives in its own table.
	if _, err := client.Channel.UpdateOneID(ch.ID).
		SetUpdatedAt(xtime.UTCNow()).
		Save(ctx); err != nil {
		return false, fmt.Errorf("touch channel: %w", err)
	}

	return true, nil
}

// holdsSubscriptionGrant reports whether a channel actually carries a grant.
//
// An OAuth object is the modern shape. Antigravity predates it and keeps a
// "<refreshToken>|<projectID>" string in the legacy APIKey field, which never
// satisfies IsOAuth, so it is matched explicitly.
func holdsSubscriptionGrant(ch *ent.Channel) bool {
	if ch == nil {
		return false
	}

	if ch.Credentials.IsOAuth() {
		return true
	}

	return ch.Type == channel.TypeAntigravity && ch.Credentials.APIKey != ""
}

// subscriptionChannelTypes lists the channel types that authorize through a
// subscription grant rather than a plain API key.
func subscriptionChannelTypes() []channel.Type {
	return []channel.Type{
		channel.TypeCodex,
		channel.TypeFenno,
		channel.TypeClaudecode,
		channel.TypeGithubCopilot,
		channel.TypeXaiSubscription,
		channel.TypeAntigravity,
	}
}
