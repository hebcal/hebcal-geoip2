package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func testServer() *server {
	return &server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestHandleLookupBadInput(t *testing.T) {
	srv := testServer()
	cases := []struct {
		url  string
		want int
	}{
		{"/lookup", http.StatusBadRequest},              // missing ip
		{"/lookup?ip=", http.StatusBadRequest},          // empty ip
		{"/lookup?ip=not-an-ip", http.StatusBadRequest}, // invalid ip
		{"/lookup?ip=999.1.1.1", http.StatusBadRequest}, // invalid ip
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.url, nil)
		w := httptest.NewRecorder()
		srv.handleLookup(w, req)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.url, w.Code, c.want)
		}
	}
}

func TestHandleLookupNoReader(t *testing.T) {
	srv := testServer() // reader never loaded
	req := httptest.NewRequest(http.MethodGet, "/lookup?ip=8.8.8.8", nil)
	w := httptest.NewRecorder()
	srv.handleLookup(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleHealthNotReady(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestGeoRecordJSON pins the JSON shape: it must match exactly the subset of
// the MaxMind record that hebcal-web's getLocationFromGeoIp() reads, so the
// Node side does not need to change.
func TestGeoRecordJSON(t *testing.T) {
	var rec geoRecord
	rec.Country.ISOCode = "US"
	rec.City.GeoNameID = 5128581
	rec.Location.Latitude = 40.7
	rec.Location.Longitude = -74.0
	rec.Location.AccuracyRadius = 20
	rec.Location.TimeZone = "America/New_York"
	rec.Postal.Code = "10001"

	b, err := json.Marshal(&rec)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	country := got["country"].(map[string]any)
	if country["iso_code"] != "US" {
		t.Errorf("country.iso_code = %v", country["iso_code"])
	}
	city := got["city"].(map[string]any)
	if city["geoname_id"].(float64) != 5128581 {
		t.Errorf("city.geoname_id = %v", city["geoname_id"])
	}
	loc := got["location"].(map[string]any)
	if loc["time_zone"] != "America/New_York" {
		t.Errorf("location.time_zone = %v", loc["time_zone"])
	}
	if loc["accuracy_radius"].(float64) != 20 {
		t.Errorf("location.accuracy_radius = %v", loc["accuracy_radius"])
	}
	postal := got["postal"].(map[string]any)
	if postal["code"] != "10001" {
		t.Errorf("postal.code = %v", postal["code"])
	}
}

// TestLookupRealDatabase runs only when TEST_MMDB points at a real
// GeoLite2-City.mmdb, exercising the actual mmap-backed lookup path.
func TestLookupRealDatabase(t *testing.T) {
	dbPath := os.Getenv("TEST_MMDB")
	if dbPath == "" {
		t.Skip("set TEST_MMDB to a GeoLite2-City.mmdb to run this test")
	}
	srv := testServer()
	srv.dbPath = dbPath
	if err := srv.load(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/lookup?ip=8.8.8.8", nil)
	w := httptest.NewRecorder()
	srv.handleLookup(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var rec geoRecord
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Country.ISOCode != "US" {
		t.Errorf("8.8.8.8 country = %q, want US", rec.Country.ISOCode)
	}
}
