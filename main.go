// Command hebcal-geoip2 is a small, fast GeoIP lookup microservice.
//
// It memory-maps a MaxMind GeoLite2-City.mmdb database (via
// maxminddb-golang, which mmaps the file so resident memory stays small and
// the database pages are shared/reclaimable through the OS page cache) and
// exposes a single HTTP endpoint, GET /lookup?ip=<addr>, returning the subset
// of the MaxMind record that hebcal-web's getLocationFromGeoIp() consumes.
//
// By default it listens on a Unix domain socket so a co-located Node.js
// process can call it with sub-millisecond latency and no TCP overhead. Pass
// -addr to listen on TCP instead (e.g. when running on a separate host in the
// same VPC).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

// geoRecord mirrors the fields of the MaxMind City record that hebcal-web
// actually reads. The maxminddb tags drive decoding from the .mmdb; the json
// tags reproduce the exact shape the Node `maxmind` library returns, so the
// downstream logic in getLocationFromGeoIp() does not have to change.
type geoRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code" json:"iso_code,omitempty"`
	} `maxminddb:"country" json:"country"`
	City struct {
		GeoNameID uint `maxminddb:"geoname_id" json:"geoname_id,omitempty"`
	} `maxminddb:"city" json:"city"`
	Location struct {
		Latitude       float64 `maxminddb:"latitude" json:"latitude,omitempty"`
		Longitude      float64 `maxminddb:"longitude" json:"longitude,omitempty"`
		AccuracyRadius uint16  `maxminddb:"accuracy_radius" json:"accuracy_radius,omitempty"`
		TimeZone       string  `maxminddb:"time_zone" json:"time_zone,omitempty"`
	} `maxminddb:"location" json:"location"`
	Postal struct {
		Code string `maxminddb:"code" json:"code,omitempty"`
	} `maxminddb:"postal" json:"postal"`
}

// server holds the current reader behind an atomic pointer so SIGHUP can swap
// in a freshly-opened database (e.g. after geoipupdate refreshes the file)
// without locking on the hot path.
type server struct {
	dbPath string
	reader atomic.Pointer[maxminddb.Reader]
	log    *slog.Logger
}

func (s *server) load() error {
	r, err := maxminddb.Open(s.dbPath)
	if err != nil {
		return err
	}
	old := s.reader.Swap(r)
	if old != nil {
		// Close the previous reader once outstanding lookups drain. Lookups
		// are quick; a short delay is more than enough.
		go func() {
			time.Sleep(5 * time.Second)
			_ = old.Close()
		}()
	}
	s.log.Info("loaded database", "path", s.dbPath,
		"build_epoch", r.Metadata.BuildEpoch, "node_count", r.Metadata.NodeCount)
	return nil
}

func (s *server) handleLookup(w http.ResponseWriter, r *http.Request) {
	ipStr := r.URL.Query().Get("ip")
	if ipStr == "" {
		http.Error(w, `{"error":"missing ip"}`, http.StatusBadRequest)
		return
	}
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		http.Error(w, `{"error":"invalid ip"}`, http.StatusBadRequest)
		return
	}
	reader := s.reader.Load()
	if reader == nil {
		http.Error(w, `{"error":"database not loaded"}`, http.StatusServiceUnavailable)
		return
	}
	result := reader.Lookup(addr.Unmap())
	if err := result.Err(); err != nil {
		s.log.Warn("lookup error", "ip", ipStr, "err", err)
		http.Error(w, `{"error":"lookup failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !result.Found() {
		// Not in the database: 204, no body. The Node client treats any
		// non-2xx-with-body / empty result as {geo:'none'}.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var rec geoRecord
	if err := result.Decode(&rec); err != nil {
		s.log.Warn("decode error", "ip", ipStr, "err", err)
		http.Error(w, `{"error":"decode failed"}`, http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(&rec); err != nil {
		s.log.Warn("encode error", "ip", ipStr, "err", err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if s.reader.Load() == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

func main() {
	var (
		dbPath     = flag.String("db", "/var/lib/GeoIP/GeoLite2-City.mmdb", "path to GeoLite2-City.mmdb")
		socketPath = flag.String("socket", "/run/hebcal-geoip2/geoip2.sock", "unix domain socket path to listen on (ignored if -addr is set)")
		addr       = flag.String("addr", "", "TCP address to listen on instead of a unix socket, e.g. 127.0.0.1:8090")
		socketMode = flag.Int("socket-mode", 0o666, "permission bits for the unix socket")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	srv := &server{dbPath: *dbPath, log: log}
	if err := srv.load(); err != nil {
		log.Error("failed to open database", "path", *dbPath, "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /lookup", srv.handleLookup)
	mux.HandleFunc("GET /healthz", srv.handleHealth)

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, cleanup, err := listen(*addr, *socketPath, os.FileMode(*socketMode))
	if err != nil {
		log.Error("failed to listen", "err", err)
		os.Exit(1)
	}
	defer cleanup()

	// Reload the database on SIGHUP (e.g. after the mmdb is refreshed).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			log.Info("SIGHUP received, reloading database")
			if err := srv.load(); err != nil {
				log.Error("reload failed", "err", err)
			}
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	log.Info("listening", "network", ln.Addr().Network(), "address", ln.Addr().String())
	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve error", "err", err)
		cleanup()
		os.Exit(1)
	}
}

// listen creates either a TCP listener (when addr is non-empty) or a Unix
// domain socket listener. For the unix socket it removes any stale socket
// file, creates the parent directory, and relaxes the socket permissions so
// the Node.js user can connect. The returned cleanup removes the socket file.
func listen(addr, socketPath string, mode os.FileMode) (net.Listener, func(), error) {
	if addr != "" {
		ln, err := net.Listen("tcp", addr)
		return ln, func() {}, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, nil, err
	}
	// Remove a stale socket left behind by a previous crash.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(socketPath) }
	return ln, cleanup, nil
}
