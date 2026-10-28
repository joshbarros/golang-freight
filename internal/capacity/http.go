package capacity

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-freight/internal/platform/httpx"
)

const maxCapacityTeu = 5000

// Sailing is the public view of a sailing.
type Sailing struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	Vessel       string    `json:"vessel"`
	Origin       string    `json:"origin"`
	Destination  string    `json:"destination"`
	Departure    time.Time `json:"departure"`
	CapacityTeu  int       `json:"capacityTeu"`
	RemainingTeu int       `json:"remainingTeu"`
}

// Info is what other modules may ask the sailings module (Bookings never reads this schema).
type Info struct {
	ID   uuid.UUID
	Code string
}

// Catalog answers the one cross-module read: a sailing's existence.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a sailing up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code FROM capacity.sailings WHERE code = $1`, code).Scan(&i.ID, &i.Code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the sailings HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the sailing endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/sailings", authed(a.list))
	mux.HandleFunc("GET /api/sailings/{code}", authed(a.get))
	mux.HandleFunc("POST /api/sailings", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const sailingSelect = `SELECT id, code, vessel, origin, destination, departure, capacity_teu, remaining_teu FROM capacity.sailings`

func scanSailing(row pgx.Row) (Sailing, error) {
	var d Sailing
	err := row.Scan(&d.ID, &d.Code, &d.Vessel, &d.Origin, &d.Destination, &d.Departure, &d.CapacityTeu, &d.RemainingTeu)
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), sailingSelect+` ORDER BY departure, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Sailing{}
	for rows.Next() {
		d, err := scanSailing(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanSailing(a.Pool.QueryRow(r.Context(), sailingSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code        string     `json:"code"`
	Vessel      string     `json:"vessel"`
	Origin      string     `json:"origin"`
	Destination string     `json:"destination"`
	Departure   *time.Time `json:"departure"`
	CapacityTeu int64      `json:"capacityTeu"`
}

// create opens a sailing. Staff only. This is the only place remaining_teu is initialised; after that only the reserve and release
// handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Vessel) == "" || strings.TrimSpace(req.Origin) == "" || strings.TrimSpace(req.Destination) == "" {
		return httpx.Fail(http.StatusBadRequest, "Vessel, origin and destination are required.")
	}
	if req.CapacityTeu < 1 || req.CapacityTeu > maxCapacityTeu {
		return httpx.Fail(http.StatusBadRequest, "Capacity must be between 1 and %d TEU.", maxCapacityTeu)
	}
	when := time.Now().AddDate(0, 0, 14)
	if req.Departure != nil {
		when = *req.Departure
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO capacity.sailings (id, code, vessel, origin, destination, departure, capacity_teu, remaining_teu, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,now())`,
		id, code, strings.TrimSpace(req.Vessel), strings.TrimSpace(req.Origin), strings.TrimSpace(req.Destination), when, req.CapacityTeu)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Sailing '%s' already exists.", code)
		}
		return err
	}
	d, err := scanSailing(a.Pool.QueryRow(r.Context(), sailingSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/sailings/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}
