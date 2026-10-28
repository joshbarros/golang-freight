package bookings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-freight/internal/capacity"
	"github.com/joshbarros/golang-freight/internal/platform/httpx"
	"github.com/joshbarros/golang-freight/internal/platform/messaging"
	"github.com/joshbarros/golang-freight/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "bookings"
	ConsumerName = "bookings"
)

// Booking statuses.
const (
	Requested = "Requested"
	Held      = "Held"
	Issued    = "Issued"
	Reversed  = "Reversed"
	Rejected  = "Rejected"
)

// Booking is the public view of an booking.
type Booking struct {
	ID                 uuid.UUID  `json:"id"`
	OwnerID            uuid.UUID  `json:"ownerId"`
	SailingCode        string     `json:"sailingCode"`
	Reference          string     `json:"reference"`
	Teu                int        `json:"teu"`
	FeeCents           int64      `json:"feeCents"`
	Status             string     `json:"status"`
	Reason             *string    `json:"reason"`
	BillOfLadingNumber *string    `json:"billOfLadingNumber"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	CapacityReleasedAt *time.Time `json:"capacityReleasedAt"`
}

// SailingCatalog is the one thing Bookings may ask the sailings module.
type SailingCatalog interface {
	Find(ctx context.Context, code string) (*capacity.Info, error)
}

// Service is the HTTP side of the saga: it writes the booking and its first outbox row in one local transaction.
type Service struct {
	Pool     *pgxpool.Pool
	Outbox   messaging.Outbox
	Sailings SailingCatalog
	Log      *slog.Logger

	// FinalMarkDelay is a latency knob for the final mark: Kubernetes sets it so the refund race window is wide enough to hit on purpose.
	FinalMarkDelay time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, catalog SailingCatalog, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "bookings"}, Sailings: catalog, Log: log}
}

const bookingColumns = `id, owner_id, sailing_code, reference, teu, fee_cents, status, reason, bill_of_lading_number, created_at, updated_at, capacity_released_at`

func scanBooking(row pgx.Row) (Booking, error) {
	var o Booking
	err := row.Scan(&o.ID, &o.OwnerID, &o.SailingCode, &o.Reference, &o.Teu, &o.FeeCents, &o.Status, &o.Reason, &o.BillOfLadingNumber, &o.CreatedAt, &o.UpdatedAt, &o.CapacityReleasedAt)
	return o, err
}

// CreateRequest is the body of POST /api/bookings.
type CreateRequest struct {
	SailingCode string `json:"sailingCode"`
	Reference   string `json:"reference"`
	Teu         int64  `json:"teu"`
}

// Create reserves teu in the sailing for the caller: validates, writes the booking and its reserve command in one transaction.
func (s *Service) Create(ctx context.Context, caller *httpx.Principal, req CreateRequest) (Booking, error) {
	if caller == nil {
		return Booking{}, httpx.Fail(http.StatusUnauthorized, "Sign in to book.")
	}
	if strings.TrimSpace(req.SailingCode) == "" {
		return Booking{}, httpx.Fail(http.StatusBadRequest, "Sailing code is required.")
	}
	if msg := Validate(req.Reference, req.Teu); msg != "" {
		return Booking{}, httpx.Fail(http.StatusBadRequest, "%s", msg)
	}
	sailing, err := s.Sailings.Find(ctx, strings.TrimSpace(req.SailingCode))
	if err != nil {
		return Booking{}, err
	}
	if sailing == nil {
		return Booking{}, httpx.Fail(http.StatusNotFound, "Sailing '%s' does not exist.", req.SailingCode)
	}
	now := time.Now().UTC()
	teu := int(req.Teu)
	reference := strings.TrimSpace(req.Reference)
	fee, err := FeeCents(teu)
	if err != nil {
		return Booking{}, httpx.Fail(http.StatusBadRequest, "The fee is too large.")
	}
	id, _ := uuid.NewV7()
	o := Booking{ID: id, OwnerID: caller.ID, SailingCode: sailing.Code, Reference: reference, Teu: teu, FeeCents: fee, Status: Requested, CreatedAt: now, UpdatedAt: now}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO bookings.bookings (id, owner_id, sailing_id, sailing_code, reference, teu, fee_cents, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'Requested',$8,$8)`, o.ID, o.OwnerID, sailing.ID, o.SailingCode, o.Reference, o.Teu, o.FeeCents, now); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReserveKey, o.ID,
			messaging.ReserveCapacity{BookingID: o.ID, OwnerID: caller.ID, SailingID: sailing.ID, Teu: teu, Reference: reference})
	})
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return Booking{}, httpx.Fail(http.StatusConflict, "Reference '%s' is already booked on %s.", reference, sailing.Code)
		}
		return Booking{}, err
	}
	return o, nil
}

// Get returns an booking to its owner or to staff. Anyone else gets a 404, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Booking, error) {
	o, err := scanBooking(s.Pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings.bookings WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !caller.CanAccess(o.OwnerID)) {
		return Booking{}, httpx.Fail(http.StatusNotFound, "Booking not found.")
	}
	return o, err
}

// List returns the caller's bookings (staff: everyone's), newest first, a page at a time, with the total.
func (s *Service) List(ctx context.Context, caller *httpx.Principal, sailingCode string, page, pageSize int) ([]Booking, int, error) {
	if caller == nil {
		return nil, 0, httpx.Fail(http.StatusUnauthorized, "Sign in to see bookings.")
	}
	where := []string{"true"}
	args := []any{}
	if !caller.IsStaff() {
		args = append(args, caller.ID)
		where = append(where, "owner_id = $"+strconv.Itoa(len(args)))
	}
	if strings.TrimSpace(sailingCode) != "" {
		args = append(args, sailingCode)
		where = append(where, "sailing_code = $"+strconv.Itoa(len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM bookings.bookings WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM bookings.bookings WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		bookingColumns, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Booking{}
	for rows.Next() {
		o, err := scanBooking(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
	}
	return items, total, rows.Err()
}

// Refund moves Held or Issued to Reversed and asks Sailings to give the teu back. The document is not touched.
func (s *Service) Refund(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Booking, error) {
	if _, err := s.Get(ctx, caller, id); err != nil { // 404 unless the caller may see it
		return Booking{}, err
	}
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE bookings.bookings SET status = 'Reversed', reason = 'Reversed by request.', updated_at = now()
			WHERE id = $1 AND status IN ('Held','Issued')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var current string
			if err := tx.QueryRow(ctx, `SELECT status FROM bookings.bookings WHERE id = $1`, id).Scan(&current); err != nil {
				return err
			}
			return httpx.Fail(http.StatusConflict, "A booking in status %s cannot be reversed.", current)
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, id, messaging.ReleaseCapacity{BookingID: id, Reason: "refund"})
	})
	if err != nil {
		return Booking{}, err
	}
	return scanBooking(s.Pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings.bookings WHERE id = $1`, id))
}
