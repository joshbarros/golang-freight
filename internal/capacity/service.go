package capacity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-freight/internal/platform/messaging"
	"github.com/joshbarros/golang-freight/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "capacity"
	ConsumerName = "capacity"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	Duplicate
)

// Service is the sailings module's logic. It is the only writer of remaining_teu.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "capacity"}, Log: log}
}

// Handle implements messaging.Handler for the sailings queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReserveCapacity](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleaseCapacity](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("sailings does not handle %q", d.RoutingKey)
}

// Reserve holds teu with one conditional UPDATE that matches only while the remaining teu still cover the request: when
// two bookings fight for the last teu, one commits a hold and the other is short and writes none.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReserveCapacity) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE capacity.sailings SET remaining_teu = remaining_teu - $2 WHERE id = $1 AND remaining_teu >= $2`, cmd.SailingID, cmd.Teu)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `INSERT INTO capacity.teu_holds (booking_id, owner_id, sailing_id, teu, status, created_at) VALUES ($1,$2,$3,$4,'Held',now())`,
				cmd.BookingID, cmd.OwnerID, cmd.SailingID, cmd.Teu); err != nil {
				return err
			}
			return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.BookingID,
				messaging.CapacityHeld{BookingID: cmd.BookingID, OwnerID: cmd.OwnerID, SailingID: cmd.SailingID, Teu: cmd.Teu, Reference: cmd.Reference})
		}
		outcome = Short
		var left int
		_ = tx.QueryRow(ctx, `SELECT remaining_teu FROM capacity.sailings WHERE id = $1`, cmd.SailingID).Scan(&left)
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.BookingID,
			messaging.CapacityShort{BookingID: cmd.BookingID, Reference: cmd.Reference, RequestedTeu: cmd.Teu, RemainingTeu: left})
	})
	return outcome, err
}

// Release gives held teu back exactly once. A second release for the same booking changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleaseCapacity) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var sailingID uuid.UUID
		var teu int
		err = tx.QueryRow(ctx, `
			UPDATE capacity.teu_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE booking_id = $1 AND status = 'Held' RETURNING sailing_id, teu`, cmd.BookingID, cmd.Reason).Scan(&sailingID, &teu)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE capacity.sailings SET remaining_teu = remaining_teu + $2 WHERE id = $1 AND remaining_teu + $2 <= capacity_teu`, sailingID, teu); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.BookingID, messaging.CapacityReleased{BookingID: cmd.BookingID, Teu: teu, Reason: cmd.Reason})
	})
	return released, err
}
