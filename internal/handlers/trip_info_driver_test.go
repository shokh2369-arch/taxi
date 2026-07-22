package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func setupTripInfoTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:trip_info_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		telegram_id INTEGER NOT NULL DEFAULT 0,
		role TEXT NOT NULL DEFAULT 'rider',
		name TEXT,
		phone TEXT
	)`)
	exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		last_lat REAL, last_lng REAL,
		phone TEXT, car_type TEXT, color TEXT, plate TEXT,
		first_name TEXT, last_name TEXT, plate_number TEXT,
		app_lat REAL, app_lng REAL, app_last_seen_at TEXT,
		app_location_active INTEGER NOT NULL DEFAULT 0
	)`)
	exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		pickup_lat REAL NOT NULL,
		pickup_lng REAL NOT NULL,
		drop_lat REAL, drop_lng REAL,
		assigned_driver_user_id INTEGER,
		status TEXT NOT NULL DEFAULT 'ASSIGNED'
	)`)
	exec(`CREATE TABLE trips (
		id TEXT PRIMARY KEY,
		request_id TEXT NOT NULL,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		distance_m INTEGER NOT NULL DEFAULT 0,
		fare_amount INTEGER
	)`)
	return db
}

// TestTripInfo_DriverObjectMarshaled guards against the duplicate json:"driver" tag regression:
// encoding/json dropped both Driver and DriverLegacy, so responses had no driver at all.
func TestTripInfo_DriverObjectMarshaled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTripInfoTestDB(t)
	defer db.Close()

	_, _ = db.Exec(`INSERT INTO users (id, telegram_id, role, name, phone) VALUES
		(7, 700, 'driver', 'Driver Seven', '+998900000007'),
		(2, 200, 'rider', 'Rider Two', '+998900000002')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng, phone, car_type, color, plate, plate_number)
		VALUES (7, 41.30, 69.28, '+998900000007', 'sedan', 'white', '01A001AA', '01A001AA')`)
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, drop_lat, drop_lng, assigned_driver_user_id)
		VALUES ('req-1', 2, 41.31, 69.29, 41.35, 69.33, 7)`)
	_, _ = db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status, distance_m, fare_amount)
		VALUES ('trip-1', 'req-1', 7, 2, 'WAITING', 0, 0)`)

	cfg := &config.Config{StartingFee: 4000, PricePerKm: 1500}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/trip/trip-1", nil)
	c.Request = c.Request.WithContext(auth.WithUser(c.Request.Context(), &auth.User{
		UserID: 2,
		Role:   domain.RoleRider,
	}))
	c.Params = gin.Params{{Key: "id", Value: "trip-1"}}

	TripInfo(db, cfg, nil)(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rawDriver, ok := resp["driver"]
	if !ok {
		t.Fatalf("response missing \"driver\" key; body = %s", w.Body.String())
	}
	var driver map[string]interface{}
	if err := json.Unmarshal(rawDriver, &driver); err != nil {
		t.Fatalf("\"driver\" is not an object: %v (raw: %s)", err, rawDriver)
	}
	if driver["id"] != "7" {
		t.Fatalf("driver.id = %v, want \"7\"", driver["id"])
	}
	if driver["phone"] != "+998900000007" {
		t.Fatalf("driver.phone = %v", driver["phone"])
	}
	car, ok := driver["car"].(map[string]interface{})
	if !ok || car["plate"] != "01A001AA" {
		t.Fatalf("driver.car = %#v", driver["car"])
	}
	// Riders must not receive top-level driver_id.
	if raw, ok := resp["driver_id"]; ok && string(raw) != "0" && string(raw) != "null" {
		t.Fatalf("rider response should omit driver_id; got %s", raw)
	}
	if _, ok := resp["driver_pos"]; !ok {
		t.Fatal("response missing \"driver_pos\"")
	}
}
