package bookings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joshbarros/golang-freight/internal/platform/messaging"
	"github.com/joshbarros/golang-freight/internal/platform/pgdb"
)

// MarkOutcome is what a mark did.
type MarkOutcome int

// The ways a mark can end.
const (
	Marked MarkOutcome = iota
	Skipped
	Duplicate
)

// Handle consumes the events that move an booking forward. Each step runs only from the status it expects.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	poison := func(err error) error { return fmt.Errorf("%w: %v", messaging.ErrPoison, err) }
	switch d.RoutingKey {
	case messaging.HeldKey:
		m, err := messaging.Parse[messaging.CapacityHeld](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkHeld(ctx, d.MessageID, m.BookingID)
		return err
	case messaging.ShortKey:
		m, err := messaging.Parse[messaging.CapacityShort](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkShort(ctx, d.MessageID, m)
		return err
	case messaging.IssuedKey:
		m, err := messaging.Parse[messaging.BillOfLadingIssued](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkIssued(ctx, d.MessageID, m, s.FinalMarkDelay)
		return err
	case messaging.RejectedKey:
		m, err := messaging.Parse[messaging.DocumentRejected](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkRejected(ctx, d.MessageID, m)
		return err
	case messaging.ReleasedKey:
		m, err := messaging.Parse[messaging.CapacityReleased](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkReleased(ctx, d.MessageID, m)
		return err
	}
	return fmt.Errorf("bookings does not handle %q", d.RoutingKey)
}

// step runs update inside one transaction with the inbox document. When update changes nothing and the booking is still waiting
// for an earlier mark (Requested), the message arrived too early: it is retried, not dropped.
func (s *Service) step(ctx context.Context, messageID uuid.UUID, bookingForBookingCheck *uuid.UUID, update func(pgx.Tx) (int64, error)) (MarkOutcome, error) {
	outcome := Marked
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		changed, err := update(tx)
		if err != nil {
			return err
		}
		if changed == 0 && bookingForBookingCheck != nil {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM bookings.bookings WHERE id = $1`, *bookingForBookingCheck).Scan(&status)
			if err == nil && status == Requested {
				return &messaging.OutOfBookingError{Reason: "The final mark arrived before the Held mark."}
			}
		}
		if changed == 0 {
			outcome = Skipped
		}
		return nil
	})
	return outcome, err
}

// MarkHeld: the teu are held.
func (s *Service) MarkHeld(ctx context.Context, messageID, bookingID uuid.UUID) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE bookings.bookings SET status = 'Held', reason = NULL, updated_at = now() WHERE id = $1 AND status = 'Requested'`, bookingID)
		return tag.RowsAffected(), err
	})
}

// ShortReason words why the reserve was refused.
func ShortReason(m messaging.CapacityShort) string {
	return fmt.Sprintf("Short: %d TEU requested, %d remaining.", m.RequestedTeu, m.RemainingTeu)
}

// MarkShort: the stock said no.
func (s *Service) MarkShort(ctx context.Context, messageID uuid.UUID, m messaging.CapacityShort) (MarkOutcome, error) {
	reason := ShortReason(m)
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE bookings.bookings SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status = 'Requested'`, m.BookingID, reason)
		return tag.RowsAffected(), err
	})
}

// MarkIssued is the final mark. If a refund got there first the row is no longer Held and nothing changes.
func (s *Service) MarkIssued(ctx context.Context, messageID uuid.UUID, m messaging.BillOfLadingIssued, delay time.Duration) (MarkOutcome, error) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Skipped, ctx.Err()
		}
	}
	return s.step(ctx, messageID, &m.BookingID, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE bookings.bookings SET status = 'Issued', bill_of_lading_number = $2, updated_at = now() WHERE id = $1 AND status = 'Held'`, m.BookingID, m.Number)
		return tag.RowsAffected(), err
	})
}

// MarkRejected: the document was refused (poison).
func (s *Service) MarkRejected(ctx context.Context, messageID uuid.UUID, m messaging.DocumentRejected) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE bookings.bookings SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status IN ('Held','Requested')`, m.BookingID, m.Reason)
		return tag.RowsAffected(), err
	})
}

// MarkReleased records that the teu came back.
func (s *Service) MarkReleased(ctx context.Context, messageID uuid.UUID, m messaging.CapacityReleased) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE bookings.bookings SET capacity_released_at = now() WHERE id = $1 AND capacity_released_at IS NULL`, m.BookingID)
		return tag.RowsAffected(), err
	})
}

var _ = errors.New
