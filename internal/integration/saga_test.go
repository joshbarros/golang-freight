package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-freight/internal/bookings"
	"github.com/joshbarros/golang-freight/internal/documents"
	"github.com/joshbarros/golang-freight/internal/platform/messaging"
)

func (e *env) outbox(schema, key string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+schema+`.outbox_messages WHERE routing_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) count(sql string, args ...any) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) remainingTeu(sailingCode string) float64 {
	e.t.Helper()
	v, _ := e.get("/api/sailings/"+sailingCode, e.admin().Token).Body["remainingTeu"].(float64)
	return v
}

func status(e *env, s session, id string) string { return e.booking(s, id).str("status") }

func feeOf(t *testing.T, teu int) float64 {
	t.Helper()
	fee, err := bookings.FeeCents(teu)
	if err != nil {
		t.Fatal(err)
	}
	return float64(fee)
}

func TestTheWholeSagaFromBookingToIssuedAndTheDocument(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(10)
	ana := e.register()
	created := e.enter(ana, sailing, "COL-1", 2)
	id := created.str("id")
	if created.Status != 202 || created.str("status") != "Requested" || created.Body["feeCents"] != feeOf(t, 2) {
		t.Fatalf("accepted as Requested with the fee: %s", created.Raw)
	}
	if dead := e.drain(); dead != 0 {
		t.Fatalf("nothing should dead-letter: %d", dead)
	}
	o := e.booking(ana, id)
	if o.str("status") != "Issued" || !strings.HasPrefix(o.str("billOfLadingNumber"), "BL-") || o.Body["reason"] != nil {
		t.Fatalf("sold with a document number and no leftover reason: %s", o.Raw)
	}
	if e.remainingTeu(sailing) != 8 {
		t.Fatal("2 of 10 teu are held")
	}
	c := e.get("/api/documents/"+id, ana.Token)
	if c.Status != 200 || c.str("status") != "Issued" || c.str("number") != o.str("billOfLadingNumber") {
		t.Fatalf("the document: %d %s", c.Status, c.Raw)
	}
}

func TestWhenTwoBookingsFightForTheLastTeuOneCommitsAndTheOtherIsShortAndWritesNoHold(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(3)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, sailing, "COL-A", 2).str("id")
	e.drain()
	b := e.enter(bob, sailing, "COL-B", 2).str("id")
	e.drain()
	if status(e, ana, a) != "Issued" {
		t.Fatal("the first booking is sold")
	}
	o := e.booking(bob, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Short: 2 TEU requested, 1 remaining." {
		t.Fatalf("the second is short: %s", o.Raw)
	}
	if e.get("/api/documents/"+b, bob.Token).Status != 404 || e.count(`SELECT count(*) FROM capacity.teu_holds`) != 1 {
		t.Fatal("no document and no hold for a refused booking")
	}
	if e.remainingTeu(sailing) != 1 {
		t.Fatal("the refused booking took nothing")
	}
}

func TestPoisonIsRejectedAndTheTeuComeBack(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(5)
	ana := e.register()
	id := e.enter(ana, sailing, "COL-POISON-1", 2).str("id")
	if dead := e.drain(); dead != 1 {
		t.Fatalf("the poison document is dead-lettered once: %d", dead)
	}
	o := e.booking(ana, id)
	if o.str("status") != "Rejected" || !strings.Contains(o.str("reason"), "poison") || o.Body["capacityReleasedAt"] == nil {
		t.Fatalf("rejected, with the reason, and the release recorded: %s", o.Raw)
	}
	if e.remainingTeu(sailing) != 5 {
		t.Fatal("the teu are back")
	}
	if c := e.get("/api/documents/"+id, ana.Token); c.str("status") != "Rejected" {
		t.Fatalf("the rejection is committed on the document: %s", c.Raw)
	}
}

func TestRefundAfterSaleReturnsTheTeuButKeepsTheDocument(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(5)
	ana := e.register()
	id := e.enter(ana, sailing, "COL-1", 2).str("id")
	e.drain()
	if r := e.post("/api/bookings/"+id+"/reversal", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Reversed" || e.remainingTeu(sailing) != 5 {
		t.Fatal("refunded and the teu are back on the shelf")
	}
	if c := e.get("/api/documents/"+id, ana.Token); c.str("status") != "Issued" {
		t.Fatal("a refund does not roll back the document that was issued")
	}
	if r := e.post("/api/bookings/"+id+"/reversal", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a second refund is a conflict: %d", r.Status)
	}
}

func TestRefundBeatsAFinalMarkThatArrivesLate(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, sailing, "COL-1", 1).str("id"))
	for pass := 0; pass < 4; pass++ {
		for _, row := range e.unsent() {
			if row.d.RoutingKey == messaging.IssuedKey {
				continue
			}
			e.markSent(row)
			_, _ = e.deliver(row.d.RoutingKey, row.d)
		}
	}
	if got := status(e, ana, id.String()); got != "Held" {
		t.Fatalf("held, document issued, final mark not yet landed: %s", got)
	}
	if r := e.post("/api/bookings/"+id.String()+"/reversal", nil, ana.Token); r.Status != 202 {
		t.Fatal(r.Status)
	}
	e.drain()
	o := e.booking(ana, id.String())
	if o.str("status") != "Reversed" || o.Body["billOfLadingNumber"] != nil {
		t.Fatalf("the late final mark must not undo the refund: %s", o.Raw)
	}
	if c := e.get("/api/documents/"+id.String(), ana.Token); c.str("status") != "Issued" {
		t.Fatal("the issued document stays")
	}
}

func TestADeliveryRepeatedByTheBrokerHasNoEffectTheSecondTime(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(5)
	ana := e.register()
	e.enter(ana, sailing, "COL-1", 1)
	rows := e.unsent()
	if len(rows) != 1 {
		t.Fatal("one reserve command")
	}
	d := rows[0].d
	for i := 0; i < 3; i++ {
		if _, err := e.deliver(d.RoutingKey, d); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM capacity.teu_holds`) != 1 || e.outbox("capacity", messaging.HeldKey) != 1 || e.remainingTeu(sailing) != 4 {
		t.Fatal("at-least-once delivery, exactly-once effect: the inbox absorbs the repeats")
	}
}

func TestAFinalMarkBeforeTheHeldMarkIsRetriedNotSailingped(t *testing.T) {
	e := newEnv(t)
	sailing := e.openSailing(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, sailing, "COL-1", 1).str("id"))
	_, err := e.app.Bookings.MarkIssued(context.Background(), uuid.New(), messaging.BillOfLadingIssued{BookingID: id, Number: "BL-TEST"}, 0)
	var ooo *messaging.OutOfBookingError
	if err == nil || !errorsAs(err, &ooo) {
		t.Fatalf("the final mark must ask to be retried, got %v", err)
	}
	if status(e, ana, id.String()) != "Requested" {
		t.Fatal("and change nothing")
	}
	e.drain()
	if status(e, ana, id.String()) != "Issued" {
		t.Fatal("the normal flow still completes")
	}
}

func TestIssueIsIdempotentAndPoisonIsAFlagNotAnException(t *testing.T) {
	e := newEnv(t)
	id, owner := uuid.New(), uuid.New()
	msg := uuid.New()
	held := messaging.CapacityHeld{BookingID: id, OwnerID: owner, SailingID: uuid.New(), Teu: 1, Reference: "COL-1"}
	first, err := e.app.Documents.Issue(context.Background(), msg, held)
	again, err2 := e.app.Documents.Issue(context.Background(), msg, held)
	if err != nil || err2 != nil || first != documents.Issued || again != documents.Duplicate {
		t.Fatalf("issue then duplicate: %v %v %v %v", first, err, again, err2)
	}
	poison := messaging.CapacityHeld{BookingID: uuid.New(), OwnerID: owner, SailingID: uuid.New(), Teu: 1, Reference: "xPOISONx"}
	if out, _ := e.app.Documents.Issue(context.Background(), uuid.New(), poison); out != documents.Rejected {
		t.Fatal("a poison reference is rejected")
	}
	if e.outbox("documents", messaging.ReleaseKey) != 1 || e.outbox("documents", messaging.RejectedKey) != 1 || e.outbox("documents", messaging.IssuedKey) != 1 {
		t.Fatal("one issued, one release command and one rejection")
	}
}
