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
