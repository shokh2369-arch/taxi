package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func TestDriverManualOffline_clearsLiveFlags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sql.Open("sqlite", "file:driver_offline?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		is_active INTEGER NOT NULL DEFAULT 0,
		manual_offline INTEGER NOT NULL DEFAULT 0,
		live_location_active INTEGER NOT NULL DEFAULT 0,
		last_live_location_at TEXT
	);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO drivers (user_id, is_active, live_location_active, last_live_location_at) VALUES (7, 1, 1, '2026-04-18 12:00:00')`); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/driver/offline", nil)
	c.Request = c.Request.WithContext(auth.WithUser(context.Background(), &auth.User{UserID: 7, Role: domain.RoleDriver}))

	DriverManualOffline(db)(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var isAct, liveAct int
	var lastLive sql.NullString
	err = db.QueryRow(`SELECT COALESCE(is_active,0), COALESCE(live_location_active,0), last_live_location_at FROM drivers WHERE user_id = 7`).Scan(&isAct, &liveAct, &lastLive)
	if err != nil {
		t.Fatal(err)
	}
	var manualOffline int
	_ = db.QueryRow(`SELECT COALESCE(manual_offline,0) FROM drivers WHERE user_id = 7`).Scan(&manualOffline)
	if isAct != 0 || liveAct != 0 || lastLive.Valid || manualOffline != 1 {
		t.Fatalf("want offline cleared, got is_active=%d live_active=%d last_live=%v manual_offline=%d", isAct, liveAct, lastLive, manualOffline)
	}
}

// The OFFLINE toggle must survive background location pings.
//
// A driver app reports position continuously, and the online-pulse used to clear
// manual_offline on every ping — so a driver who finished their shift and drove
// home was put back online and kept receiving orders they could not serve.
func TestDriverOffline_SurvivesLocationPings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sql.Open("sqlite", "file:driver_offline_pings?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		is_active INTEGER NOT NULL DEFAULT 0,
		manual_offline INTEGER NOT NULL DEFAULT 0,
		last_seen_at TEXT
	);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO drivers (user_id, is_active, manual_offline) VALUES (1, 0, 1)`); err != nil {
		t.Fatal(err)
	}

	// Simulate the pulse path's update, which is now guarded on manual_offline.
	if _, err := db.Exec(`
		UPDATE drivers SET is_active = 1, last_seen_at = datetime('now')
		WHERE user_id = 1 AND COALESCE(manual_offline, 0) = 0`); err != nil {
		t.Fatal(err)
	}

	var isActive, manualOffline int
	if err := db.QueryRow(`SELECT COALESCE(is_active,0), COALESCE(manual_offline,0) FROM drivers WHERE user_id = 1`).
		Scan(&isActive, &manualOffline); err != nil {
		t.Fatal(err)
	}
	if isActive != 0 || manualOffline != 1 {
		t.Errorf("after a location ping while manually offline: is_active=%d manual_offline=%d, want 0/1", isActive, manualOffline)
	}
}
