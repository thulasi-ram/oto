package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// channelRow is the row model of `channels`.
type channelRow struct {
	id             uuid.UUID
	orgID          uuid.UUID
	kind           string
	name           string
	config         []byte
	credentialID   *uuid.UUID
	capabilities   int64
	renderer       string
	verbosity      string
	threadUpdates  bool
	showFieldEmoji bool
	enabled        bool
	healthStatus   string
	healthError    *string
	deletedAt      *time.Time
}

func (r channelRow) toDomain() domain.Channel {
	c := domain.Channel{
		ID:             r.id,
		OrgID:          r.orgID,
		Type:           domain.ChannelType(r.kind),
		Name:           r.name,
		Config:         r.config,
		CredentialID:   r.credentialID,
		Capabilities:   domain.Capability(r.capabilities),
		Renderer:       r.renderer,
		Verbosity:      domain.Verbosity(r.verbosity),
		ThreadUpdates:  r.threadUpdates,
		ShowFieldEmoji: r.showFieldEmoji,
		Enabled:        r.enabled,
		HealthStatus:   domain.HealthStatus(r.healthStatus),
		DeletedAt:      r.deletedAt,
	}
	if r.healthError != nil {
		c.HealthError = *r.healthError
	}
	return c
}

// SealedCredential is a credential as stored: still sealed.
//
// This module NEVER unseals one. It carries the ciphertext to a port and gets
// values back, so a stack trace or a log line out of the notification module can
// never contain a workspace token.
type SealedCredential struct {
	ID         uuid.UUID
	Kind       string
	Sealed     []byte
	KeyVersion int
}

// ChannelRepository is the SQL over `channels` and `channel_credentials`.
//
// Channels are configuration this module READS and whose health it WRITES. The
// health write is the exception on purpose: only the code that actually
// attempted a delivery knows whether a token is dead, and `auth_failed` three
// days before anybody notices is precisely the failure §G.6 exists to surface.
type ChannelRepository struct {
	q db.Querier
}

// NewChannelRepository builds the repository over a fallback querier.
func NewChannelRepository(q db.Querier) *ChannelRepository { return &ChannelRepository{q: q} }

func (r *ChannelRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const channelColumns = `
  id, org_id, type, name, config, credential_id, capabilities, renderer,
  verbosity, thread_updates, show_field_emoji, enabled, health_status,
  health_error, deleted_at`

const listChannelsByIDsSQL = `
SELECT` + channelColumns + `
  FROM channels
 WHERE org_id = $1 AND id = ANY($2::uuid[])
 ORDER BY name ASC`

// ListByIDs resolves a policy's `channel_ids` fan-out.
//
// It returns DELETED AND DISABLED channels too. That is deliberate: the caller
// has to be able to tell "the policy names three channels and all three are
// disabled" (a recorded `channel_disabled` suppression) apart from "the policy
// names three channels that do not exist", and a query that filters them out
// makes those two indistinguishable.
func (r *ChannelRepository) ListByIDs(
	ctx context.Context, s db.TenantScope, ids []uuid.UUID,
) ([]domain.Channel, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db(ctx).Query(ctx, listChannelsByIDsSQL, s.OrgID(), ids)
	if err != nil {
		return nil, mapErr(err, "channel_not_found", "list channels")
	}
	defer rows.Close()

	out := make([]domain.Channel, 0, len(ids))
	for rows.Next() {
		var row channelRow
		if err := rows.Scan(
			&row.id, &row.orgID, &row.kind, &row.name, &row.config, &row.credentialID,
			&row.capabilities, &row.renderer, &row.verbosity, &row.threadUpdates,
			&row.showFieldEmoji, &row.enabled, &row.healthStatus, &row.healthError,
			&row.deletedAt,
		); err != nil {
			return nil, mapErr(err, "channel_not_found", "scan channel")
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "channel_not_found", "read channels")
	}
	return out, nil
}

const getChannelSQL = `
SELECT` + channelColumns + `
  FROM channels
 WHERE org_id = $1 AND id = $2`

// Get reads one channel.
func (r *ChannelRepository) Get(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Channel, error) {
	var row channelRow
	err := r.db(ctx).QueryRow(ctx, getChannelSQL, s.OrgID(), id).Scan(
		&row.id, &row.orgID, &row.kind, &row.name, &row.config, &row.credentialID,
		&row.capabilities, &row.renderer, &row.verbosity, &row.threadUpdates,
		&row.showFieldEmoji, &row.enabled, &row.healthStatus, &row.healthError,
		&row.deletedAt,
	)
	if err != nil {
		return domain.Channel{}, mapErr(err, "channel_not_found", "channel")
	}
	return row.toDomain(), nil
}

// `health_checked_at` takes the caller's instant verbatim; `updated_at` is
// MONOTONIC.
//
// ⭐ THE GREATEST IS THE FIX FOR `internal_error/channels_time_ck`. Every
// timestamp on `channels` comes from the application (00032 removed the DB
// default that used to stamp `created_at`), but "the application" is N pods and N
// clocks, and this is the first write a brand-new channel receives: a dispatcher
// a few milliseconds behind the pod that created the destination would write an
// `updated_at` below `created_at` and fail channels_time_ck with a 23514 — a 500
// on the first delivery, with nothing actually wrong. Same idiom, same reason, as
// OrderingStore.Advance in threads.go.
const setChannelHealthSQL = `
UPDATE channels
   SET health_status = $3,
       health_error  = $4,
       health_checked_at = $5,
       updated_at    = GREATEST(updated_at, $5)
 WHERE org_id = $1 AND id = $2
   AND (health_status IS DISTINCT FROM $3 OR health_error IS DISTINCT FROM $4)`

// SetHealth records what the last delivery attempt learned about a destination.
//
// The `IS DISTINCT FROM` guard makes a repeated failure a no-op write rather
// than a row-version churn on every retry — a busy dead channel would otherwise
// generate one UPDATE per attempt per delivery.
//
// `health_error` is mandatory whenever the status is not healthy or unknown
// (channels_health_ck), so a caller that clears the status must clear the text
// with it. Passing an empty string for a failing status is a bug the database
// will catch, which is where it belongs.
func (r *ChannelRepository) SetHealth(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
	status domain.HealthStatus, detail string, now time.Time,
) error {
	var errText *string
	if detail != "" {
		errText = &detail
	}
	_, err := r.db(ctx).Exec(ctx, setChannelHealthSQL, s.OrgID(), id, string(status), errText, now)
	return mapErr(err, "channel_not_found", "update channel health")
}

const claimStormNoticeSQL = `
UPDATE channels
   SET storm_notice_at = $3
 WHERE org_id = $1 AND id = $2
   AND (storm_notice_at IS NULL OR storm_notice_at <= $4)
RETURNING id`

// ClaimStormNotice takes the channel-level storm-notice latch (ADR 0020).
//
// ⭐ THE RETURN VALUE IS THE WHOLE MECHANISM. It reports whether THIS caller is
// the one that gets to tell the channel that oto has started withholding
// individual notifications. Storm mode is per GROUP and a channel carries many
// groups, so without this every group collapsing in a burst would post its own
// "going quiet" message into the same channel — the flood the damper exists to
// prevent, produced by the damper's own announcement.
//
// The claim is ONE conditional UPDATE, so concurrent dispatchers cannot both win:
// the row lock serialises them and the loser's predicate no longer holds. Zero
// rows IS the answer, in the same idiom as the delivery claim in §G.5 — it is not
// an error and the caller must not retry it.
//
// `notBefore` is `now - window`; the caller passes the org's storm cooldown,
// because that is the setting that already defines the minimum distance between a
// storm starting and the same storm ending.
func (r *ChannelRepository) ClaimStormNotice(
	ctx context.Context, s db.TenantScope, id uuid.UUID, now, notBefore time.Time,
) (bool, error) {
	var claimed uuid.UUID
	err := r.db(ctx).QueryRow(ctx, claimStormNoticeSQL, s.OrgID(), id, now, notBefore).Scan(&claimed)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Another group's storm already told this channel, inside the window.
		return false, nil
	default:
		return false, mapErr(err, "channel_not_found", "claim storm notice")
	}
}

const getCredentialSQL = `
SELECT id, kind, sealed, key_version
  FROM channel_credentials
 WHERE org_id = $1 AND id = $2`

// Credential reads a channel's sealed secret. It comes back SEALED.
func (r *ChannelRepository) Credential(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (SealedCredential, error) {
	var c SealedCredential
	err := r.db(ctx).QueryRow(ctx, getCredentialSQL, s.OrgID(), id).
		Scan(&c.ID, &c.Kind, &c.Sealed, &c.KeyVersion)
	if err != nil {
		return SealedCredential{}, mapErr(err, "credential_not_found", "channel credential")
	}
	return c, nil
}
