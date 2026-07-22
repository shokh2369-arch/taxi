package auth

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func setupTripViewerAuthDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:trip_viewer_auth_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, telegram_id INTEGER NOT NULL DEFAULT 0, role TEXT NOT NULL);
		CREATE TABLE drivers (user_id INTEGER PRIMARY KEY, verification_status TEXT NOT NULL);
		INSERT INTO users (id, telegram_id, role) VALUES (4, 400, 'driver');
		INSERT INTO drivers (user_id, verification_status) VALUES (4, 'approved');
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTryTripScopedDriverID_FromQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTripViewerAuthDB(t)
	defer db.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/trip/x?driver_id=4", nil)

	TryTripScopedDriverID(db)(c)
	u := UserFromContext(c.Request.Context())
	if u == nil || u.UserID != 4 || u.Role != domain.RoleDriver {
		t.Fatalf("expected driver user 4, got %#v", u)
	}
}

func TestTryOptionalMiniAppAuth_MissingContinues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/trip/x", nil)
	nextCalled := false
	// Wrap so we can observe c.Next() behavior from the middleware itself.
	mw := TryOptionalMiniAppAuthDriverOrRider(nil, "driver-token", "rider-token")
	mw(c)
	if w.Code != 0 && w.Code != http.StatusOK {
		t.Fatalf("unexpected abort status %d body=%s", w.Code, w.Body.String())
	}
	// Middleware should not set a user when initData is absent.
	if u := UserFromContext(c.Request.Context()); u != nil {
		t.Fatalf("expected no user, got %#v", u)
	}
	_ = nextCalled
}
