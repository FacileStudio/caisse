package pg_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/FacileStudio/caisse"
	"github.com/FacileStudio/caisse/pg"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var _ caisse.EventStore = (*pg.Store)(nil)

// database connects to CAISSE_TEST_DATABASE_URL, and skips when it is unset so
// a clone with no PostgreSQL still passes the gate. CI always sets it.
func database(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CAISSE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CAISSE_TEST_DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	if err := pg.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE caisse_events"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func TestStoreClaimsAnEventOnce(t *testing.T) {
	db := database(t)
	store := pg.New(db, 0)
	ctx := context.Background()

	first, err := store.Begin(ctx, "evt_1")
	if err != nil || !first {
		t.Fatalf("Begin = %v, %v; want true, nil", first, err)
	}
	second, err := store.Begin(ctx, "evt_1")
	if err != nil || second {
		t.Fatalf("second Begin = %v, %v; want false, nil", second, err)
	}

	if err := store.Done(ctx, "evt_1"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	afterDone, err := store.Begin(ctx, "evt_1")
	if err != nil || afterDone {
		t.Fatalf("Begin after Done = %v, %v; want false, nil", afterDone, err)
	}
}

func TestStoreReleasesAFailedClaim(t *testing.T) {
	db := database(t)
	store := pg.New(db, 0)
	ctx := context.Background()

	if claimed, err := store.Begin(ctx, "evt_fail"); err != nil || !claimed {
		t.Fatalf("Begin = %v, %v", claimed, err)
	}
	if err := store.Fail(ctx, "evt_fail"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	reclaimed, err := store.Begin(ctx, "evt_fail")
	if err != nil || !reclaimed {
		t.Fatalf("Begin after Fail = %v, %v; want true, nil", reclaimed, err)
	}
}

// Fail after Done would resurrect a handled event and let it run a second time.
func TestStoreWillNotReleaseAHandledEvent(t *testing.T) {
	db := database(t)
	store := pg.New(db, 0)
	ctx := context.Background()

	if _, err := store.Begin(ctx, "evt_done"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.Done(ctx, "evt_done"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if err := store.Fail(ctx, "evt_done"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if claimed, err := store.Begin(ctx, "evt_done"); err != nil || claimed {
		t.Fatalf("Begin after Done then Fail = %v, %v; want false, nil", claimed, err)
	}
}

// A process killed mid-handler leaves a claim behind. Without reclamation that
// event is unprocessable forever and Stripe's retries all bounce off it.
func TestStoreReclaimsAStaleClaim(t *testing.T) {
	db := database(t)
	ctx := context.Background()

	patient := pg.New(db, time.Hour)
	if claimed, err := patient.Begin(ctx, "evt_stale"); err != nil || !claimed {
		t.Fatalf("Begin = %v, %v", claimed, err)
	}
	if claimed, err := patient.Begin(ctx, "evt_stale"); err != nil || claimed {
		t.Fatalf("a fresh claim was reclaimed: %v, %v", claimed, err)
	}

	if _, err := db.ExecContext(ctx,
		"UPDATE caisse_events SET claimed_at = now() - interval '2 hours' WHERE id = $1", "evt_stale"); err != nil {
		t.Fatalf("age the claim: %v", err)
	}

	reclaimed, err := patient.Begin(ctx, "evt_stale")
	if err != nil || !reclaimed {
		t.Fatalf("Begin on a stale claim = %v, %v; want true, nil", reclaimed, err)
	}
}

// Stripe can deliver the same event to several replicas at once. Exactly one
// must win, or the order ships twice.
// claimRace runs racers concurrent Begin calls and returns each racer's result
// and error, so the test can assert exactly one won.
func claimRace(ctx context.Context, store *pg.Store, id string, racers int) ([]bool, []error) {
	results := make([]bool, racers)
	errs := make([]error, racers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for index := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			results[index], errs[index] = store.Begin(ctx, id)
		}()
	}
	start.Done()
	done.Wait()
	return results, errs
}

func TestStoreLetsOnlyOneConcurrentClaimWin(t *testing.T) {
	db := database(t)
	store := pg.New(db, 0)
	results, errs := claimRace(context.Background(), store, "evt_race", 12)

	winners := 0
	for index := range results {
		if errs[index] != nil {
			t.Fatalf("racer %d: %v", index, errs[index])
		}
		if results[index] {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d racers claimed the same event, want exactly 1", winners)
	}
}

func handleEvent(t *testing.T, store *pg.Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := store.Begin(ctx, id); err != nil {
		t.Fatalf("Begin %s: %v", id, err)
	}
	if err := store.Done(ctx, id); err != nil {
		t.Fatalf("Done %s: %v", id, err)
	}
}

func TestPurgeDropsHandledEventsOnly(t *testing.T) {
	db := database(t)
	store := pg.New(db, 0)
	ctx := context.Background()

	for _, id := range []string{"evt_old", "evt_recent"} {
		handleEvent(t, store, ctx, id)
	}
	if _, err := store.Begin(ctx, "evt_pending"); err != nil {
		t.Fatalf("Begin pending: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE caisse_events SET done_at = now() - interval '30 days' WHERE id = $1", "evt_old"); err != nil {
		t.Fatalf("age the event: %v", err)
	}

	deleted, err := store.Purge(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if deleted != 1 {
		t.Errorf("purged %d rows, want 1", deleted)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM caisse_events").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Errorf("%d rows left, want 2", remaining)
	}
}
