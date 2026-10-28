// Package messaging is the transactional outbox, the inbox, and the RabbitMQ and Kafka plumbing around them.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Names of the exchanges, the Kafka topic and the routing keys. They are the contract between the modules.
const (
	Exchange        = "freight.work"
	DeadLetterExch  = "freight.dlx"
	RequeueExchange = "freight.requeue"
	KafkaTopic      = "freight.events"
	RetryDelayMs    = 2000
	MaxAttempts     = 3
	ReserveKey      = "capacity.reserve"
	ReleaseKey      = "capacity.release"
	HeldKey         = "capacity.held"
	ShortKey        = "capacity.short"
	ReleasedKey     = "capacity.released"
	IssuedKey       = "document.issued"
	RejectedKey     = "document.rejected"
	SailingsQueue   = "capacity.commands"
	DocumentsQueue  = "documents.work"
	BookingsQueue   = "bookings.events"
	PoisonToken     = "POISON"
	attemptHeader   = "x-attempt"
	outboxBatchSize = 50
	relayIdleDelay  = 200 * time.Millisecond
	relayErrorDelay = time.Second
	trimEvery       = 10 * time.Minute
)

// Delivery is one message handed to a Handler.
type Delivery struct {
	MessageID  uuid.UUID
	RoutingKey string
	Body       []byte
}

// Handler consumes one message. Returning ErrPoison dead-letters it at once; any other error retries it.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }

// ErrPoison marks a message whose rejection is already committed: it must be dead-lettered, not retried.
var ErrPoison = errors.New("poison message")

// OutOfBookingError means the message arrived before the state it depends on; it is retried later.
type OutOfBookingError struct{ Reason string }

func (e *OutOfBookingError) Error() string { return e.Reason }

// IsPoison reports whether the reference carries the poison token (a case-sensitive substring).
func IsPoison(reference string) bool { return strings.Contains(reference, PoisonToken) }

// FailureAction is what a consumer does with a message whose handler failed.
type FailureAction int

// The two things a failed message can meet.
const (
	ActionRetry FailureAction = iota
	ActionDeadLetter
)

// Decide: a poison message is dead-lettered at once; anything else is retried until it has been tried MaxAttempts times.
func Decide(priorAttempts int, err error) FailureAction {
	if errors.Is(err, ErrPoison) {
		return ActionDeadLetter
	}
	if priorAttempts+1 >= MaxAttempts {
		return ActionDeadLetter
	}
	return ActionRetry
}

// ---- the messages ----

// ReserveCapacity asks the sailing to hold teu.
type ReserveCapacity struct {
	BookingID uuid.UUID `json:"bookingId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	SailingID uuid.UUID `json:"sailingId"`
	Teu       int       `json:"teu"`
	Reference string    `json:"reference"`
}

// CapacityHeld says the teu are held.
type CapacityHeld struct {
	BookingID uuid.UUID `json:"bookingId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	SailingID uuid.UUID `json:"sailingId"`
	Teu       int       `json:"teu"`
	Reference string    `json:"reference"`
}

// CapacityShort says the reserve was refused: the remaining teu did not cover it.
type CapacityShort struct {
	BookingID    uuid.UUID `json:"bookingId"`
	Reference    string    `json:"reference"`
	RequestedTeu int       `json:"requestedTeu"`
	RemainingTeu int       `json:"remainingTeu"`
}

// ReleaseCapacity asks the sailing to give held teu back.
type ReleaseCapacity struct {
	BookingID uuid.UUID `json:"bookingId"`
	Reason    string    `json:"reason"`
}

// CapacityReleased says teu (possibly none) were given back.
type CapacityReleased struct {
	BookingID uuid.UUID `json:"bookingId"`
	Teu       int       `json:"teu"`
	Reason    string    `json:"reason"`
}

// BillOfLadingIssued says the document exists.
type BillOfLadingIssued struct {
	BookingID uuid.UUID `json:"bookingId"`
	Number    string    `json:"number"`
}

// DocumentRejected says the document was refused (the poison rule).
type DocumentRejected struct {
	BookingID uuid.UUID `json:"bookingId"`
	Reason    string    `json:"reason"`
}

// Parse decodes a message body.
func Parse[T any](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	return v, nil
}
