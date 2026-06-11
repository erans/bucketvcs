package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestSessionLifecycle(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "alice", true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	raw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("session id too short: %q", raw)
	}

	got, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if got.UserID != uid || got.Name != "alice" || !got.IsAdmin || got.Provider != "password" {
		t.Fatalf("session = %+v", got)
	}

	// unknown id
	if _, err := s.LookupSession(ctx, "does-not-exist"); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("unknown id: want ErrNoSession, got %v", err)
	}

	// delete (logout)
	if err := s.DeleteSession(ctx, raw); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.LookupSession(ctx, raw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("after delete: want ErrNoSession, got %v", err)
	}
}

func TestLookupSession_RejectsDisabledUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	// Two admins so disabling one does not trip the last-admin guard.
	if _, err := s.CreateUser(ctx, "keeper", true); err != nil {
		t.Fatalf("CreateUser keeper: %v", err)
	}
	uid, err := s.CreateUser(ctx, "dave", true)
	if err != nil {
		t.Fatalf("CreateUser dave: %v", err)
	}
	if err := s.SetPassword(ctx, "dave", "pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	raw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Sanity: the session works while the user is enabled.
	if _, err := s.LookupSession(ctx, raw); err != nil {
		t.Fatalf("LookupSession (enabled): %v", err)
	}

	if err := s.SetUserDisabled(ctx, "dave", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}

	// A disabled user's session must no longer resolve.
	if _, err := s.LookupSession(ctx, raw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("disabled-user lookup: want ErrNoSession, got %v", err)
	}
}

func TestDeleteSessionsForUser(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "alice", true)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	otherUID, err := s.CreateUser(ctx, "mallory", false)
	if err != nil {
		t.Fatalf("CreateUser mallory: %v", err)
	}

	raw1, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	raw2, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	raw3, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 3: %v", err)
	}
	otherRaw, err := s.CreateSession(ctx, otherUID, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession other: %v", err)
	}

	// Delete alice's other sessions, keeping raw2 (the "current" one).
	n, err := s.DeleteSessionsForUser(ctx, uid, raw2)
	if err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d sessions, want 2", n)
	}

	// raw2 survives.
	if _, err := s.LookupSession(ctx, raw2); err != nil {
		t.Fatalf("LookupSession raw2 (kept): %v", err)
	}
	// raw1 + raw3 are gone.
	if _, err := s.LookupSession(ctx, raw1); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("LookupSession raw1: want ErrNoSession, got %v", err)
	}
	if _, err := s.LookupSession(ctx, raw3); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("LookupSession raw3: want ErrNoSession, got %v", err)
	}
	// The other user's session is untouched.
	if _, err := s.LookupSession(ctx, otherRaw); err != nil {
		t.Fatalf("LookupSession other user: %v", err)
	}

	// exceptRawID == "" deletes all remaining sessions for the user.
	n, err = s.DeleteSessionsForUser(ctx, uid, "")
	if err != nil {
		t.Fatalf("DeleteSessionsForUser (all): %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d sessions, want 1", n)
	}
	if _, err := s.LookupSession(ctx, raw2); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("LookupSession raw2 after delete-all: want ErrNoSession, got %v", err)
	}
	if _, err := s.LookupSession(ctx, otherRaw); err != nil {
		t.Fatalf("LookupSession other user after delete-all: %v", err)
	}
}

func TestListSessionsForUser_MarksCurrentAndOrders(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Three sessions; the second one is the "current" one (the caller's cookie).
	raw1, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	rawCur, err := s.CreateSession(ctx, uid, "oidc", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession cur: %v", err)
	}
	raw3, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession 3: %v", err)
	}

	// Stagger last_seen so ordering (newest-first) is deterministic:
	// raw3 newest, rawCur middle, raw1 oldest.
	now := time.Now().Unix()
	for raw, ls := range map[string]int64{raw1: now - 300, rawCur: now - 200, raw3: now - 100} {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE sessions SET last_seen = ? WHERE id_hash = ?`, ls, hashSessionID(raw)); err != nil {
			t.Fatalf("age last_seen: %v", err)
		}
	}

	infos, err := s.ListSessionsForUser(ctx, uid, rawCur)
	if err != nil {
		t.Fatalf("ListSessionsForUser: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("len = %d, want 3", len(infos))
	}

	// Ordered newest-first by last_seen: raw3, rawCur, raw1.
	wantOrder := []string{hashSessionID(raw3), hashSessionID(rawCur), hashSessionID(raw1)}
	for i, want := range wantOrder {
		if infos[i].IDHash != want {
			t.Fatalf("order[%d] = %q, want %q", i, infos[i].IDHash, want)
		}
	}

	// Exactly the current session is marked.
	curHash := hashSessionID(rawCur)
	var currentCount int
	for _, info := range infos {
		if info.IsCurrent {
			currentCount++
			if info.IDHash != curHash {
				t.Fatalf("IsCurrent set on %q, want %q", info.IDHash, curHash)
			}
		}
	}
	if currentCount != 1 {
		t.Fatalf("IsCurrent count = %d, want 1", currentCount)
	}

	// Fields are populated on the current row.
	for _, info := range infos {
		if info.IDHash == curHash {
			if info.Provider != "oidc" {
				t.Fatalf("Provider = %q, want oidc", info.Provider)
			}
			if info.CreatedAt == 0 || info.ExpiresAt == 0 || info.LastSeen == 0 {
				t.Fatalf("timestamp fields unset: %+v", info)
			}
		}
	}

	// The raw id must never leak into the view.
	for _, info := range infos {
		if info.IDHash == rawCur || info.IDHash == raw1 || info.IDHash == raw3 {
			t.Fatalf("IDHash %q equals a raw cookie id — raw id leaked", info.IDHash)
		}
	}
}

func TestDeleteSessionByHashForUser_IsUserScoped(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	otherUID, err := s.CreateUser(ctx, "mallory", false)
	if err != nil {
		t.Fatalf("CreateUser mallory: %v", err)
	}

	aliceRaw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession alice: %v", err)
	}
	otherRaw, err := s.CreateSession(ctx, otherUID, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession other: %v", err)
	}

	otherHash := hashSessionID(otherRaw)

	// SECURITY BOUNDARY: alice cannot delete mallory's session by hash.
	n, err := s.DeleteSessionByHashForUser(ctx, uid, otherHash)
	if err != nil {
		t.Fatalf("DeleteSessionByHashForUser (cross-user): %v", err)
	}
	if n != 0 {
		t.Fatalf("cross-user delete affected %d rows, want 0 (security boundary breached)", n)
	}
	// Mallory's session must still resolve.
	if _, err := s.LookupSession(ctx, otherRaw); err != nil {
		t.Fatalf("mallory session must survive cross-user delete: %v", err)
	}

	// Self-scoped delete works: alice deletes her own.
	aliceHash := hashSessionID(aliceRaw)
	n, err = s.DeleteSessionByHashForUser(ctx, uid, aliceHash)
	if err != nil {
		t.Fatalf("DeleteSessionByHashForUser (self): %v", err)
	}
	if n != 1 {
		t.Fatalf("self delete affected %d rows, want 1", n)
	}
	if _, err := s.LookupSession(ctx, aliceRaw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("alice session after self-delete: want ErrNoSession, got %v", err)
	}
}

func TestListAllSessionsAndDeleteByHash(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uidA, err := s.CreateUser(ctx, "alice", false)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	uidB, err := s.CreateUser(ctx, "bob", true)
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	aliceRaw, err := s.CreateSession(ctx, uidA, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession alice: %v", err)
	}
	bobRaw, err := s.CreateSession(ctx, uidB, "oidc", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession bob: %v", err)
	}

	all, err := s.ListAllSessions(ctx)
	if err != nil {
		t.Fatalf("ListAllSessions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}

	// The admin view joins the user name.
	byHash := map[string]auth.AdminSessionInfo{}
	for _, a := range all {
		byHash[a.IDHash] = a
	}
	aliceInfo, ok := byHash[hashSessionID(aliceRaw)]
	if !ok {
		t.Fatal("alice session missing from ListAllSessions")
	}
	if aliceInfo.UserName != "alice" || aliceInfo.UserID != uidA {
		t.Fatalf("alice admin info = %+v, want name=alice id=%s", aliceInfo, uidA)
	}
	bobInfo, ok := byHash[hashSessionID(bobRaw)]
	if !ok {
		t.Fatal("bob session missing from ListAllSessions")
	}
	if bobInfo.UserName != "bob" || bobInfo.UserID != uidB || bobInfo.Provider != "oidc" {
		t.Fatalf("bob admin info = %+v", bobInfo)
	}

	// Admin delete-by-hash (no user scoping) removes the session.
	n, err := s.DeleteSessionByHash(ctx, hashSessionID(bobRaw))
	if err != nil {
		t.Fatalf("DeleteSessionByHash: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteSessionByHash affected %d rows, want 1", n)
	}
	if _, err := s.LookupSession(ctx, bobRaw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("bob session after admin delete: want ErrNoSession, got %v", err)
	}
	// Deleting an absent hash is a 0-row no-op (not an error).
	n, err = s.DeleteSessionByHash(ctx, hashSessionID(bobRaw))
	if err != nil {
		t.Fatalf("DeleteSessionByHash (absent): %v", err)
	}
	if n != 0 {
		t.Fatalf("absent delete affected %d rows, want 0", n)
	}
}

func TestSessionExpiryAndSweep(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()
	uid, err := s.CreateUser(ctx, "bob", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// already-expired session (negative TTL)
	raw, err := s.CreateSession(ctx, uid, "password", -time.Minute)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.LookupSession(ctx, raw); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expired lookup: want ErrNoSession, got %v", err)
	}

	n, err := s.SweepExpiredSessions(ctx, time.Now())
	if err != nil {
		t.Fatalf("SweepExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
}

func TestTouchSession_SlidesAndThrottles(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	uid, err := s.CreateUser(ctx, "carol", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	raw, err := s.CreateSession(ctx, uid, "password", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// --- assertion 1: no-op within the minute ---
	// last_seen was just set to "now" at insert, so last_seen <= now-60 is false.
	before, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession (before touch): %v", err)
	}

	if err := s.TouchSession(ctx, raw, 2*time.Hour); err != nil {
		t.Fatalf("TouchSession (throttled): %v", err)
	}

	afterThrottle, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession (after throttled touch): %v", err)
	}
	if afterThrottle.ExpiresAt.Unix() != before.ExpiresAt.Unix() {
		t.Fatalf("throttle guard failed: ExpiresAt changed from %v to %v (want no change)",
			before.ExpiresAt, afterThrottle.ExpiresAt)
	}

	// --- assertion 2: slides when last_seen is old ---
	// Force last_seen 120 seconds into the past so the guard allows an update.
	idh := hashSessionID(raw)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen = ? WHERE id_hash = ?`,
		time.Now().Unix()-120, idh); err != nil {
		t.Fatalf("age last_seen: %v", err)
	}

	if err := s.TouchSession(ctx, raw, 2*time.Hour); err != nil {
		t.Fatalf("TouchSession (slide): %v", err)
	}

	afterSlide, err := s.LookupSession(ctx, raw)
	if err != nil {
		t.Fatalf("LookupSession (after slide touch): %v", err)
	}
	// New ExpiresAt should be ~now+2h; original was ~now+1h.
	// Require at least original+50min to tolerate second-boundary races.
	minExpected := before.ExpiresAt.Add(50 * time.Minute)
	if !afterSlide.ExpiresAt.After(minExpected) {
		t.Fatalf("ExpiresAt did not slide: got %v, want > %v", afterSlide.ExpiresAt, minExpected)
	}
}
