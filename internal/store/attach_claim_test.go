package store_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/store"
)

// ADR-065 — an attach token is spent by the session it opens, not by the request
// that tries.
//
// The decision is implemented in exactly two places: the WHERE clause of
// ClaimPortForwardSession and the WHERE clause of ClaimTerminalSession. Every
// assertion here is therefore made against the ROW after the statement ran —
// never against the statement's text or its arguments, which is all a fake
// database can see and which would pass unchanged against a claim missing the
// clause that makes it safe.
//
// Every assertion is owed twice, once per family: the two claims are two
// statements over two tables, and a test that only covers one proves nothing
// about the other (ADR-065 §Verification, §2).

// sessionRow is the observable state of a session row — the subject of every
// assertion below.
type sessionRow struct {
	id            int64
	claimedAt     time.Time // zero when NULL
	startedAt     time.Time
	endedAt       time.Time // zero when NULL
	endReason     string    // empty when NULL
	attachKeyHash []byte
	attachSeq     int64
}

// attachFamily is one of the two SQL-claimed attach paths. It exists so an
// assertion is written once and *executed* twice — what ADR-065 forbids is
// covering one family and inferring the other, not sharing the assertion's
// text.
type attachFamily struct {
	name  string
	table string
	// mint inserts a claimable session whose token lives for ttl.
	mint func(t *testing.T, q *store.Queries, teamID int64, ttl time.Duration) (id int64, tokenHash string)
	// claim runs the family's claim statement and reports the row it returned.
	claim func(ctx context.Context, q *store.Queries, tokenHash string, keyHash []byte) (sessionRow, error)
	// expireGrant expires the ADR-045 authorization window on the row WITHOUT
	// ending it, so the `authorized_until` clause can be tested independently of
	// `ended_at`. nil on the terminal, which has no such column (ADR-065 §4).
	expireGrant func(t *testing.T, pool *pgxpool.Pool, id int64)
}

func portForwardFamily() attachFamily {
	const table = "port_forward_sessions"
	return attachFamily{
		name:  "port_forward",
		table: table,
		mint: func(t *testing.T, q *store.Queries, teamID int64, ttl time.Duration) (int64, string) {
			t.Helper()
			hash := randomHex(32)
			row, err := q.CreatePortForwardSession(context.Background(), store.CreatePortForwardSessionParams{
				TeamID: teamID, TargetName: "adr065", TargetPort: 5432,
				TokenHash: hash, TokenExpiresAt: stamp(time.Now().Add(ttl)),
			})
			if err != nil {
				t.Fatalf("minting a port-forward session: %v", err)
			}
			return row.ID, hash
		},
		claim: func(ctx context.Context, q *store.Queries, tokenHash string, keyHash []byte) (sessionRow, error) {
			row, err := q.ClaimPortForwardSession(ctx, store.ClaimPortForwardSessionParams{
				TokenHash: tokenHash, AttachKeyHash: keyHash,
			})
			if err != nil {
				return sessionRow{}, err
			}
			return sessionRow{
				id: row.ID, claimedAt: row.ClaimedAt.Time, startedAt: row.StartedAt.Time,
				endedAt: row.EndedAt.Time, attachKeyHash: row.AttachKeyHash, attachSeq: row.AttachSeq,
			}, nil
		},
		expireGrant: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			t.Helper()
			execOnRow(t, pool, "UPDATE "+table+" SET authorized_until = now() - interval '1 millisecond' WHERE id = $1", id)
		},
	}
}

func terminalFamily() attachFamily {
	const table = "terminal_sessions"
	return attachFamily{
		name:  "terminal",
		table: table,
		mint: func(t *testing.T, q *store.Queries, teamID int64, ttl time.Duration) (int64, string) {
			t.Helper()
			hash := randomHex(32)
			row, err := q.CreateTerminalSession(context.Background(), store.CreateTerminalSessionParams{
				TeamID: teamID, TargetKind: store.TerminalTargetServer, TargetName: "adr065",
				TokenHash: hash, TokenExpiresAt: stamp(time.Now().Add(ttl)),
			})
			if err != nil {
				t.Fatalf("minting a terminal session: %v", err)
			}
			return row.ID, hash
		},
		claim: func(ctx context.Context, q *store.Queries, tokenHash string, keyHash []byte) (sessionRow, error) {
			row, err := q.ClaimTerminalSession(ctx, store.ClaimTerminalSessionParams{
				TokenHash: tokenHash, AttachKeyHash: keyHash,
			})
			if err != nil {
				return sessionRow{}, err
			}
			return sessionRow{
				id: row.ID, claimedAt: row.ClaimedAt.Time, startedAt: row.StartedAt.Time,
				endedAt: row.EndedAt.Time, attachKeyHash: row.AttachKeyHash, attachSeq: row.AttachSeq,
			}, nil
		},
		// No authorized_until on terminal_sessions: the terminal is not
		// grant-bound, and ADR-065 §4 drops the clause for exactly that reason.
		expireGrant: nil,
	}
}

// forEachAttachFamily runs one body against both claim statements.
func forEachAttachFamily(t *testing.T, run func(*testing.T, *pgxpool.Pool, *store.Queries, attachFamily)) {
	t.Helper()
	pool := testDB(t)
	q := store.New(pool)
	for _, family := range []attachFamily{portForwardFamily(), terminalFamily()} {
		t.Run(family.name, func(t *testing.T) {
			run(t, pool, q, family)
		})
	}
}

// The table name always comes from an attachFamily constant in this file, never
// from data — the two statements below are the only place a table is
// interpolated, and this is why it is safe.
func readRow(t *testing.T, pool *pgxpool.Pool, table string, id int64) sessionRow {
	t.Helper()
	var (
		row                           sessionRow
		claimedAt, startedAt, endedAt pgtype.Timestamptz
		endReason                     *string
	)
	err := pool.QueryRow(context.Background(),
		"SELECT id, claimed_at, started_at, ended_at, end_reason, attach_key_hash, attach_seq FROM "+table+" WHERE id = $1", id).
		Scan(&row.id, &claimedAt, &startedAt, &endedAt, &endReason, &row.attachKeyHash, &row.attachSeq)
	if err != nil {
		t.Fatalf("reading %s row %d: %v", table, id, err)
	}
	row.claimedAt, row.startedAt, row.endedAt = claimedAt.Time, startedAt.Time, endedAt.Time
	if endReason != nil {
		row.endReason = *endReason
	}
	return row
}

func execOnRow(t *testing.T, pool *pgxpool.Pool, sql string, id int64) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), sql, id)
	if err != nil {
		t.Fatalf("fixture statement %q: %v", sql, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("fixture statement %q touched %d rows, want 1", sql, tag.RowsAffected())
	}
}

// expireToken pushes the token one millisecond into the past, using the
// DATABASE's clock. The claim's own `now()` is the database's too, so the
// boundary is exact and immune to any skew between the test process and the
// server.
func expireToken(t *testing.T, pool *pgxpool.Pool, table string, id int64) {
	t.Helper()
	execOnRow(t, pool, "UPDATE "+table+" SET token_expires_at = now() - interval '1 millisecond' WHERE id = $1", id)
}

// ADR-065 §4: a second request from the same attacher inside the TTL is the same
// attempt retried, not a replay. It is accepted, the generation counts it, and
// the two stamps that bound the session's lifetime stay pinned to the FIRST
// claim — a retry loop must not be able to buy itself extra duration by
// restarting the max-duration ceiling.
func TestSameKeyReclaimInsideTheTTLIsAcceptedAndPinsTheFirstClaimsStamps(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		ctx := context.Background()
		team := testTeam(t, pool)
		id, token := family.mint(t, q, team, time.Minute)
		key := randomKeyHash(t)

		first, err := family.claim(ctx, q, token, key)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if first.attachSeq != 1 {
			t.Errorf("attach_seq after the first claim = %d, want 1", first.attachSeq)
		}
		if first.claimedAt.IsZero() {
			t.Error("claimed_at is NULL after a successful claim")
		}
		if !bytes.Equal(first.attachKeyHash, key) {
			t.Error("the first claim did not stamp the attacher's key hash")
		}

		// Sleep so that "unmoved" means something: two statements in the same
		// microsecond would satisfy the assertion for the wrong reason.
		time.Sleep(5 * time.Millisecond)

		second, err := family.claim(ctx, q, token, key)
		if err != nil {
			t.Fatalf("same-key re-claim inside the TTL was refused: %v", err)
		}
		if second.id != first.id {
			t.Errorf("the re-claim returned row %d, want the same row %d", second.id, first.id)
		}
		if second.attachSeq != 2 {
			t.Errorf("attach_seq after the re-claim = %d, want 2", second.attachSeq)
		}
		if !second.claimedAt.Equal(first.claimedAt) {
			t.Errorf("claimed_at moved on re-claim: %s -> %s", first.claimedAt, second.claimedAt)
		}
		if !second.startedAt.Equal(first.startedAt) {
			t.Errorf("started_at moved on re-claim: %s -> %s — a retry loop would buy duration",
				first.startedAt, second.startedAt)
		}

		// A third proves the generation counts attaches rather than toggling,
		// and that the pinning holds beyond the first re-claim.
		time.Sleep(5 * time.Millisecond)
		third, err := family.claim(ctx, q, token, key)
		if err != nil {
			t.Fatalf("third same-key claim: %v", err)
		}
		if third.attachSeq != 3 {
			t.Errorf("attach_seq after the third claim = %d, want 3", third.attachSeq)
		}
		if !third.claimedAt.Equal(first.claimedAt) || !third.startedAt.Equal(first.startedAt) {
			t.Error("the stamps moved on the third claim")
		}

		// The row agrees with what the statements returned.
		row := readRow(t, pool, family.table, id)
		if row.attachSeq != 3 {
			t.Errorf("stored attach_seq = %d, want 3", row.attachSeq)
		}
		if !row.claimedAt.Equal(first.claimedAt) || !row.startedAt.Equal(first.startedAt) {
			t.Error("the stored stamps do not match the first claim's")
		}
		if !row.endedAt.IsZero() {
			t.Error("the session was finalized by a re-claim")
		}
	})
}

// ADR-065 §4: a different attacher presenting the same token is the replay the
// rule exists to stop. It matches zero rows, and — just as important — it leaves
// the incumbent's row exactly as it found it.
func TestADifferentKeyMatchesZeroRowsAndLeavesTheRowUntouched(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		ctx := context.Background()
		team := testTeam(t, pool)
		id, token := family.mint(t, q, team, time.Minute)
		key := randomKeyHash(t)

		first, err := family.claim(ctx, q, token, key)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}

		if _, err := family.claim(ctx, q, token, randomKeyHash(t)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a different-key claim returned %v, want pgx.ErrNoRows — that is the replay", err)
		}

		row := readRow(t, pool, family.table, id)
		if row.attachSeq != first.attachSeq {
			t.Errorf("the refused claim moved attach_seq to %d, want %d", row.attachSeq, first.attachSeq)
		}
		if !bytes.Equal(row.attachKeyHash, key) {
			t.Error("the refused claim overwrote the stored attach key hash")
		}
		if !row.claimedAt.Equal(first.claimedAt) || !row.startedAt.Equal(first.startedAt) {
			t.Error("the refused claim moved the session's stamps")
		}

		// The incumbent is unharmed by the attempt.
		again, err := family.claim(ctx, q, token, key)
		if err != nil {
			t.Fatalf("the legitimate attacher was locked out by a refused replay: %v", err)
		}
		if again.attachSeq != first.attachSeq+1 {
			t.Errorf("attach_seq = %d after the incumbent re-claimed, want %d", again.attachSeq, first.attachSeq+1)
		}
	})
}

// ADR-065 §7: an attach that presents no key is still accepted, and the claim
// stores server-generated random bytes rather than a NULL or a sentinel. No
// presentable key hashes to those, and neither does the next keyless attach's
// own random bytes — so such a session stays strictly single-use, exactly as
// before this decision.
func TestAKeylessClaimStoresUnmatchableBytesAndStaysStrictlySingleUse(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		ctx := context.Background()
		team := testTeam(t, pool)
		id, token := family.mint(t, q, team, time.Minute)

		// What handlers.attachClaimKey stamps when the attach key header is
		// absent: 32 random bytes that are nobody's key.
		unmatchable := randomKeyHash(t)
		first, err := family.claim(ctx, q, token, unmatchable)
		if err != nil {
			t.Fatalf("the keyless claim was refused: %v", err)
		}
		if first.attachSeq != 1 {
			t.Errorf("attach_seq = %d, want 1", first.attachSeq)
		}

		// A second keyless attach generates its own random bytes.
		if _, err := family.claim(ctx, q, token, randomKeyHash(t)); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("a second keyless claim returned %v, want pgx.ErrNoRows", err)
		}
		// A key-presenting attach hashes to something else again.
		if _, err := family.claim(ctx, q, token, randomKeyHash(t)); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("a key-presenting claim on a keyless row returned %v, want pgx.ErrNoRows", err)
		}
		// And NULL does not match stored bytes either: `attach_key_hash = NULL`
		// is NULL, not true.
		if _, err := family.claim(ctx, q, token, nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("a NULL-key claim on a stamped row returned %v, want pgx.ErrNoRows", err)
		}

		row := readRow(t, pool, family.table, id)
		if row.attachSeq != 1 {
			t.Errorf("stored attach_seq = %d after three refusals, want 1", row.attachSeq)
		}
		if !bytes.Equal(row.attachKeyHash, unmatchable) {
			t.Error("a refused claim overwrote the unmatchable bytes")
		}
	})
}

// The other half of ADR-065 §7, asserted because the ADR is emphatic that the
// column must never be left matching ANYTHING: the SQL does NOT enforce that
// on its own. `attach_key_hash IS NULL` matches every presented key, so a row
// stamped with NULL is freely re-claimable for the rest of its TTL by whoever
// holds the token.
//
// Nothing in production can reach this state — handlers.attachClaimKey generates
// random bytes precisely so the claim never passes NULL — and the column is
// nullable only so migrations 00096/00097 could be additive. This test pins the
// division of labour so that a future caller which "simplifies" attachClaimKey
// away is met with a failure that names the hole rather than opening it
// silently.
func TestTheStatementItselfDoesNotForbidANullKeyHash(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		ctx := context.Background()
		team := testTeam(t, pool)
		id, token := family.mint(t, q, team, time.Minute)

		if _, err := family.claim(ctx, q, token, nil); err != nil {
			t.Fatalf("claim with a NULL key hash: %v", err)
		}
		if row := readRow(t, pool, family.table, id); row.attachKeyHash != nil {
			t.Fatalf("attach_key_hash = %x after a NULL claim, want NULL", row.attachKeyHash)
		}
		// The hole, asserted as such: any attacker's key now matches.
		if _, err := family.claim(ctx, q, token, randomKeyHash(t)); err != nil {
			t.Fatalf("a NULL-stamped row refused a stranger's key (%v) — the statement grew a guard, "+
				"which is good news: delete this test and assert the guard instead", err)
		}
	})
}

// ADR-065 §4: `token_expires_at > now()` is unchanged and is the whole of the
// re-claim window. There is no new lifetime concept in this decision — an
// expired token is refused exactly as before, boundary included.
func TestAnExpiredTokenIsRefusedIncludingOneMillisecondPast(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		ctx := context.Background()
		team := testTeam(t, pool)

		// A token that expired one millisecond ago is refused on first claim.
		fresh, freshToken := family.mint(t, q, team, time.Minute)
		expireToken(t, pool, family.table, fresh)
		if _, err := family.claim(ctx, q, freshToken, randomKeyHash(t)); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("claiming a token that expired 1 ms ago returned %v, want pgx.ErrNoRows", err)
		}
		if row := readRow(t, pool, family.table, fresh); !row.claimedAt.IsZero() || row.attachSeq != 0 {
			t.Errorf("the refused claim still touched the row: claimed_at=%v attach_seq=%d", row.claimedAt, row.attachSeq)
		}

		// And the key that WOULD have succeeded a moment earlier does not
		// survive the boundary: the re-claim window ends with the token, not
		// with the session.
		id, token := family.mint(t, q, team, time.Minute)
		key := randomKeyHash(t)
		first, err := family.claim(ctx, q, token, key)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		expireToken(t, pool, family.table, id)
		if _, err := family.claim(ctx, q, token, key); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("a same-key re-claim past the TTL returned %v, want pgx.ErrNoRows", err)
		}
		if row := readRow(t, pool, family.table, id); row.attachSeq != first.attachSeq {
			t.Errorf("the expired re-claim moved attach_seq to %d, want %d", row.attachSeq, first.attachSeq)
		}
	})
}

// ADR-065 §4: the idempotence lives in the WHERE and nowhere else, because a
// read-then-write would race two rungs of the same ladder into two attaches that
// both believe they own the session — the one failure mode strict single-use
// never had. One statement makes the outcome decidable: both claims succeed,
// they receive DISTINCT generations, and the highest generation is the winner
// the row agrees with.
func TestConcurrentSameKeyClaimsBothSucceedWithDistinctGenerations(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		team := testTeam(t, pool)
		id, token := family.mint(t, q, team, time.Minute)
		key := randomKeyHash(t)

		const attempts = 2
		rows, errs := claimInParallel(t, q, family, token, [attempts][]byte{key, key})

		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent claim %d was refused: %v", i, err)
			}
		}
		if rows[0].attachSeq == rows[1].attachSeq {
			t.Fatalf("both concurrent claims received attach_seq %d — the statement is not serialising them",
				rows[0].attachSeq)
		}
		if lower, upper := min(rows[0].attachSeq, rows[1].attachSeq), max(rows[0].attachSeq, rows[1].attachSeq); lower != 1 || upper != 2 {
			t.Errorf("concurrent generations = %d and %d, want 1 and 2", lower, upper)
		}
		// Both saw the same session, and the loser did not restart its clock.
		if !rows[0].claimedAt.Equal(rows[1].claimedAt) || !rows[0].startedAt.Equal(rows[1].startedAt) {
			t.Error("the two concurrent claims disagree about claimed_at/started_at")
		}

		// Exactly one is the winner: the row's generation names it.
		row := readRow(t, pool, family.table, id)
		winners := 0
		for _, r := range rows {
			if r.attachSeq == row.attachSeq {
				winners++
			}
		}
		if winners != 1 {
			t.Errorf("%d of the concurrent claims hold the row's generation %d, want exactly 1", winners, row.attachSeq)
		}
		if row.attachSeq != attempts {
			t.Errorf("stored attach_seq = %d after %d claims, want %d", row.attachSeq, attempts, attempts)
		}
		if !row.endedAt.IsZero() {
			t.Error("a concurrent claim finalized the session")
		}
	})
}

// The same race with two different keys: one attacher wins the row, the other is
// the replay and gets nothing. Which one wins is a race; that exactly one wins
// is the invariant.
func TestConcurrentDifferentKeyClaimsYieldExactlyOneSuccess(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		team := testTeam(t, pool)
		id, token := family.mint(t, q, team, time.Minute)
		keys := [2][]byte{randomKeyHash(t), randomKeyHash(t)}

		rows, errs := claimInParallel(t, q, family, token, keys)

		successes := 0
		for i, err := range errs {
			switch {
			case err == nil:
				successes++
				if rows[i].attachSeq != 1 {
					t.Errorf("the winning claim got attach_seq %d, want 1", rows[i].attachSeq)
				}
				if !bytes.Equal(rows[i].attachKeyHash, keys[i]) {
					t.Error("the winning claim did not stamp its own key")
				}
			case errors.Is(err, pgx.ErrNoRows):
			default:
				t.Fatalf("claim %d failed unexpectedly: %v", i, err)
			}
		}
		if successes != 1 {
			t.Fatalf("%d of two different-key concurrent claims succeeded, want exactly 1", successes)
		}
		if row := readRow(t, pool, family.table, id); row.attachSeq != 1 {
			t.Errorf("stored attach_seq = %d, want 1 — the loser must not have counted", row.attachSeq)
		}
	})
}

// claimInParallel releases two claims at the same instant on two pooled
// connections. Two connections, not one transaction: the whole question is what
// PostgreSQL does when two committed writers meet on one row.
func claimInParallel(t *testing.T, q *store.Queries, family attachFamily, token string, keys [2][]byte) ([2]sessionRow, [2]error) {
	t.Helper()
	var (
		rows  [2]sessionRow
		errs  [2]error
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rows[i], errs[i] = family.claim(context.Background(), q, token, keys[i])
		}()
	}
	close(start)
	wg.Wait()
	return rows, errs
}

// ADR-065 §4/§8: a re-claim is the one path that can arrive after an
// authorization changed, so the claim re-checks both. A revocation that lands
// between two claims beats the re-claim — through `ended_at`, and on the
// port-forward path through `authorized_until` independently of it.
func TestARevocationBetweenTwoClaimsBeatsTheReclaim(t *testing.T) {
	forEachAttachFamily(t, func(t *testing.T, pool *pgxpool.Pool, q *store.Queries, family attachFamily) {
		ctx := context.Background()
		team := testTeam(t, pool)

		t.Run("ended_at", func(t *testing.T) {
			id, token := family.mint(t, q, team, time.Minute)
			key := randomKeyHash(t)
			if _, err := family.claim(ctx, q, token, key); err != nil {
				t.Fatalf("first claim: %v", err)
			}
			// What endSessionsOfGrant, the operator cut and the sweep all do.
			execOnRow(t, pool,
				"UPDATE "+family.table+" SET ended_at = now(), end_reason = 'revoked' WHERE id = $1", id)

			if _, err := family.claim(ctx, q, token, key); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("a re-claim after revocation returned %v, want pgx.ErrNoRows", err)
			}
			if row := readRow(t, pool, family.table, id); row.endReason != "revoked" || row.attachSeq != 1 {
				t.Errorf("the refused re-claim disturbed the finalized row: end_reason=%q attach_seq=%d",
					row.endReason, row.attachSeq)
			}
		})

		if family.expireGrant == nil {
			// terminal_sessions is not grant-bound and has no authorized_until
			// column; ADR-065 §4 drops the clause for that reason alone.
			return
		}
		t.Run("authorized_until", func(t *testing.T) {
			id, token := family.mint(t, q, team, time.Minute)
			key := randomKeyHash(t)
			if _, err := family.claim(ctx, q, token, key); err != nil {
				t.Fatalf("first claim: %v", err)
			}
			// The belt to ended_at's brace: the grant lapses, the row is still
			// open, and the re-claim must still be refused.
			family.expireGrant(t, pool, id)

			if _, err := family.claim(ctx, q, token, key); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("a re-claim after the grant expired returned %v, want pgx.ErrNoRows", err)
			}
			if row := readRow(t, pool, family.table, id); !row.endedAt.IsZero() {
				t.Error("the claim finalized the row — refusing is the claim's job, ending is the sweep's")
			}
		})
	})
}

// ADR-065 §6: because an abandoned attach now leaves its row claimed and
// re-claimable instead of ended, and CountOpenTerminalSessions has no freshness
// test, a row that never carried its PTY would hold one of the twenty per-team
// slots until the max-duration ceiling hours later. `streamed_at` is what tells
// an abandoned claim from a live shell, and the sweep's third clause is what
// bounds it at the TTL plus one sweep interval.
//
// Terminal only: the port-forward path reaches the same instant through its
// heartbeat floor, which is pre-existing machinery this decision did not change.
func TestTerminalSweepClosesAnAbandonedClaimedRowAndSparesAStreamedOne(t *testing.T) {
	pool := testDB(t)
	q := store.New(pool)
	ctx := context.Background()
	family := terminalFamily()
	team := testTeam(t, pool)

	// Abandoned: claimed, never streamed, token dead.
	abandoned, abandonedToken := family.mint(t, q, team, time.Minute)
	if _, err := family.claim(ctx, q, abandonedToken, randomKeyHash(t)); err != nil {
		t.Fatalf("claiming the abandoned session: %v", err)
	}

	// Live: claimed, its single data stream joined, token equally dead — a
	// terminal outlives its 60 s token by hours, which is the whole reason the
	// clause needs streamed_at rather than the TTL alone.
	live, liveToken := family.mint(t, q, team, time.Minute)
	liveKey := randomKeyHash(t)
	if _, err := family.claim(ctx, q, liveToken, liveKey); err != nil {
		t.Fatalf("claiming the live session: %v", err)
	}
	n, err := q.MarkTerminalSessionStreamed(ctx, live)
	if err != nil || n != 1 {
		t.Fatalf("MarkTerminalSessionStreamed = %d, %v; want 1, nil", n, err)
	}
	// Stamped exactly once (ADR-065 §6): the second stream of a session that
	// cannot have one must not move it.
	if n, err := q.MarkTerminalSessionStreamed(ctx, live); err != nil || n != 0 {
		t.Errorf("re-stamping streamed_at = %d, %v; want 0, nil", n, err)
	}
	// And streamed_at is emphatically NOT a re-claim condition: a session that
	// has served bytes is re-claimable exactly like one that has not.
	reclaimed, err := family.claim(ctx, q, liveToken, liveKey)
	if err != nil {
		t.Fatalf("a streamed session refused a same-key re-claim inside its TTL: %v", err)
	}
	if reclaimed.attachSeq != 2 {
		t.Errorf("attach_seq after re-claiming a streamed session = %d, want 2", reclaimed.attachSeq)
	}

	expireToken(t, pool, family.table, abandoned)
	expireToken(t, pool, family.table, live)

	// The sweep is global, like the scheduler's: it judges rows, not teams. The
	// tests in this package are sequential, so the only rows in flight are the
	// two above, and the assertions name them by id regardless.
	if _, err := q.SweepTerminalSessions(ctx, int32((4 * time.Hour).Seconds())); err != nil {
		t.Fatalf("SweepTerminalSessions: %v", err)
	}

	if row := readRow(t, pool, family.table, abandoned); row.endedAt.IsZero() {
		t.Error("the sweep left an abandoned claimed row open past its token — it holds a cap slot for hours")
	} else if row.endReason != "disconnect" {
		t.Errorf("the abandoned row ended as %q, want \"disconnect\" — which is what happened", row.endReason)
	}

	if row := readRow(t, pool, family.table, live); !row.endedAt.IsZero() {
		t.Errorf("the sweep closed a streamed session at its token's expiry as %q — a shell outlives its 60 s token",
			row.endReason)
	}
}
