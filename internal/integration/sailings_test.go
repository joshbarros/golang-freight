package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-freight/internal/capacity"
	"github.com/joshbarros/golang-freight/internal/platform/messaging"
)

func (e *env) rawSailing(teu int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO capacity.sailings (id, code, vessel, origin, destination, departure, capacity_teu, remaining_teu, created_at)
		VALUES ($1,$2,'a','b','c',now(),$3,$3,now())`, id, code(), teu)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, sailing uuid.UUID, teu int) capacity.ReserveOutcome {
	out, err := e.app.Sailings.Reserve(context.Background(), uuid.New(), messaging.ReserveCapacity{BookingID: uuid.New(), OwnerID: uuid.New(), SailingID: sailing, Teu: teu, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	sailing := e.rawSailing(30)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, sailing, 1) == capacity.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT remaining_teu FROM capacity.sailings WHERE id = $1`, sailing).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 bookings, 30 teu: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM capacity.teu_holds WHERE sailing_id = $1`, sailing) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestAShortReserveWritesNoHoldAndTakesNothing(t *testing.T) {
	e := newEnv(t)
	sailing := e.rawSailing(3)
	if reserve(e, sailing, 2) != capacity.Held || reserve(e, sailing, 2) != capacity.Short {
		t.Fatal("2 of 3 held, then 2 wanted and 1 left is short")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT remaining_teu FROM capacity.sailings WHERE id = $1`, sailing).Scan(&left)
	if left != 1 || e.count(`SELECT count(*) FROM capacity.teu_holds WHERE sailing_id = $1`, sailing) != 1 {
		t.Fatalf("the short try left no trace: %d left", left)
	}
	if reserve(e, sailing, 1) != capacity.Held {
		t.Fatal("the last teu is still there")
	}
}

func TestReleaseGivesBackExactlyOnce(t *testing.T) {
	e := newEnv(t)
	sailing := e.rawSailing(5)
	booking := uuid.New()
	if out, _ := e.app.Sailings.Reserve(context.Background(), uuid.New(), messaging.ReserveCapacity{BookingID: booking, OwnerID: uuid.New(), SailingID: sailing, Teu: 2, Reference: "R"}); out != capacity.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.app.Sailings.Release(context.Background(), uuid.New(), messaging.ReleaseCapacity{BookingID: booking, Reason: "refund"}); err != nil {
			t.Fatal(err)
		}
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT remaining_teu FROM capacity.sailings WHERE id = $1`, sailing).Scan(&left)
	if left != 5 || e.outbox("capacity", messaging.ReleasedKey) != 1 {
		t.Fatalf("given back once, not three times: %d left", left)
	}
}

func TestOpeningASailingValidatesIt(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := func() map[string]any {
		return map[string]any{"code": code(), "vessel": "a", "origin": "b", "destination": "c", "capacityTeu": 10}
	}
	if r := e.post("/api/sailings", good(), adm.Token); r.Status != 201 || r.Body["remainingTeu"] != float64(10) {
		t.Fatalf("a sailing opens full: %d %s", r.Status, r.Raw)
	}
	for name, mut := range map[string]func(map[string]any){
		"no code":   func(m map[string]any) { m["code"] = "" },
		"long code": func(m map[string]any) { m["code"] = "ABCDEFGHIJKLMNOPQRSTU" },
		"no text":   func(m map[string]any) { m["origin"] = " " },
		"zero":      func(m map[string]any) { m["capacityTeu"] = 0 },
		"too many":  func(m map[string]any) { m["capacityTeu"] = 100_000_000 },
	} {
		bad := good()
		mut(bad)
		if r := e.post("/api/sailings", bad, adm.Token); r.Status != 400 {
			t.Errorf("%s: %d", name, r.Status)
		}
	}
	dup := good()
	e.post("/api/sailings", dup, adm.Token)
	if r := e.post("/api/sailings", dup, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/sailings", good(), e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a sailing: %d", r.Status)
	}
}
