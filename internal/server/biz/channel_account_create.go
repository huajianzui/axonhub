package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

// CreateAccountFromCredentials adds one authorization grant to a channel.
//
// The console obtains a grant from a provider's OAuth exchange, which returns
// it as an opaque string, and hands it over here. The grant is stored as an
// account rather than written into the channel, which is what lets a channel
// hold several accounts.
//
// It is idempotent on the grant itself: re-adding the same authorization
// returns the existing account instead of creating a duplicate, because the
// per-channel uniqueness index is keyed on the credential fingerprint. That
// also makes a retried request safe.
func (svc *ChannelAccountService) CreateAccountFromCredentials(ctx context.Context, channelID int, rawCredentials string) (*ent.ChannelAccount, error) {
	trimmed := strings.TrimSpace(rawCredentials)
	if trimmed == "" {
		return nil, errors.New("credentials are required")
	}

	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("channel %d not found: %w", channelID, err)
	}

	credential, err := decodeAccountCredentials(ch, trimmed)
	if err != nil {
		return nil, err
	}

	fingerprint, err := objects.AccountCredentialFingerprint(credential)
	if err != nil {
		return nil, err
	}

	// Re-adding the same grant must not duplicate it.
	existing, err := svc.entFromContext(ctx).ChannelAccount.Query().
		Where(
			channelaccount.ChannelIDEQ(channelID),
			channelaccount.CredentialFingerprintEQ(fingerprint),
		).
		Only(ctx)
	if err == nil {
		return existing, nil
	}

	if !ent.IsNotFound(err) {
		return nil, fmt.Errorf("check for an existing account on channel %d: %w", channelID, err)
	}

	builder := svc.entFromContext(ctx).ChannelAccount.Create().
		SetChannelID(channelID).
		SetCredentials(credential).
		SetCredentialFingerprint(fingerprint).
		SetEnabled(true)

	if expiresAt, ok := objects.AccountCredentialExpiry(credential); ok {
		builder = builder.SetExpiresAt(expiresAt)
	}

	account, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create account on channel %d: %w", channelID, err)
	}

	// The channel cache only reloads when a channel row moves, and the account
	// lives in its own table, so without this the new account would not be
	// visible to the runtime until some unrelated change touched the channel.
	if err := svc.TouchChannel(ctx, channelID); err != nil {
		return nil, err
	}

	log.Info(ctx, "added channel account",
		log.Int("channel_id", channelID),
		log.String("channel", ch.Name),
		log.Int("account_id", account.ID),
	)

	return account, nil
}

// decodeAccountCredentials normalizes a provider's grant into the shape an
// account row stores.
//
// Two shapes exist, and which one applies is decided by the channel's type
// rather than by guessing:
//   - the modern OAuth object, as returned by the Codex, Claude Code and xAI
//     exchanges;
//   - Antigravity's legacy "<refreshToken>|<projectID>" string, which never had
//     an object form.
func decodeAccountCredentials(ch *ent.Channel, raw string) (map[string]any, error) {
	if ch.Type == channel.TypeAntigravity {
		if !strings.Contains(raw, "|") {
			return nil, errors.New("antigravity credentials must be \"<refreshToken>|<projectID>\"")
		}

		return map[string]any{"apiKey": raw}, nil
	}

	oauthCreds, err := oauth.ParseCredentialsJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid oauth credentials: %w", err)
	}

	return objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{OAuth: oauthCreds})
}
