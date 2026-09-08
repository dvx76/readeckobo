package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds everything parsed from the on-device KEY=VALUE config file
// plus the values that only make sense at runtime (like the config path).
//
// Recognized keys (uppercase, case-insensitive at parse time):
//
//	SERVER_URL         required; base URL of the readeckobo server.
//	TOKEN              required; bearer token (also accepted as ?token= by the server).
//	DEVICE_SERIAL      optional; Nickel serial sent to the server. When unset the
//	                   agent reports "unknown" (see docs/AGENT.md).
//	COLLECTION         optional; shelf name to file imported books into (default Readeck).
//	ARCHIVE_ON_FINISHED optional bool; accepted and parsed for forward
//	                   compatibility, unused in v1 (server-side archive is out of scope).
//	QNDB_PATH          optional; NickelDBus qndb binary (default /usr/bin/qndb).
//	QNDB_ARGS          optional; space-separated qndb arguments
//	                   (default "-t 30000 -s pfmDoneProcessing -m pfmRescanBooksFull").
//	HTTP_TIMEOUT       optional; Go duration for server requests (default 60s).
//	CA_FILE            optional; path to a PEM CA bundle for a private-CA
//	                   server. When set, HTTPS requests trust exactly that
//	                   bundle. Validated at startup: missing/unreadable/
//	                   unparsable files are errors, never silently ignored.
type Config struct {
	ServerURL         string
	Token             string
	DeviceSerial      string
	Collection        string
	ArchiveOnFinished bool
	QNDBPath          string
	QNDBArgs          []string
	HTTPTimeout       time.Duration
	CAFile            string

	// Path is the config file location; the data directory (index.json,
	// ledger.json, agent.log, agent.lock) is its parent directory.
	Path string

	// Warnings collects non-fatal parse issues (unknown keys) for the caller
	// to log.
	Warnings []string
}

const (
	defaultCollection = "Readeck"
	defaultQNDBPath   = "/usr/bin/qndb"
	defaultQNDBArgs   = "-t 30000 -s pfmDoneProcessing -m pfmRescanBooksFull"
	defaultHTTPTo     = 60 * time.Second
)

// loadConfig reads path. A missing file is an error (the agent must not run
// with unknown credentials); a file with unknown keys parses fine but logs
// nothing here (warnings are surfaced through the returned []string so the
// caller can log them through its own logger).
func loadConfig(path string) (*Config, error) {
	cfg := &Config{
		Collection:   defaultCollection,
		QNDBPath:     defaultQNDBPath,
		QNDBArgs:     strings.Fields(defaultQNDBArgs),
		HTTPTimeout:  defaultHTTPTo,
		DeviceSerial: "unknown", // no serial on hand until configured
		Path:         path,
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config %s: %w (create a KEY=VALUE file, see docs/AGENT.md)", path, err)
	}

	var unknown []string
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 1 {
			return nil, fmt.Errorf("config %s line %d: expected KEY=VALUE, got %q", path, i+1, line)
		}
		key := strings.ToUpper(strings.TrimSpace(line[:eq]))
		val := strings.TrimSpace(line[eq+1:])
		switch key {
		case "SERVER_URL":
			cfg.ServerURL = strings.TrimRight(val, "/")
		case "TOKEN":
			cfg.Token = val
		case "DEVICE_SERIAL":
			cfg.DeviceSerial = val
		case "COLLECTION":
			if val != "" {
				cfg.Collection = val
			}
		case "ARCHIVE_ON_FINISHED":
			cfg.ArchiveOnFinished, err = parseFlexBool(val)
			if err != nil {
				return nil, fmt.Errorf("config %s line %d: ARCHIVE_ON_FINISHED: %v", path, i+1, err)
			}
		case "QNDB_PATH":
			cfg.QNDBPath = val
		case "QNDB_ARGS":
			cfg.QNDBArgs = strings.Fields(val)
		case "HTTP_TIMEOUT":
			d, perr := time.ParseDuration(val)
			if perr != nil || d <= 0 {
				return nil, fmt.Errorf("config %s line %d: HTTP_TIMEOUT: %v", path, i+1, perr)
			}
			cfg.HTTPTimeout = d
		case "CA_FILE":
			cfg.CAFile = val
		default:
			unknown = append(unknown, fmt.Sprintf("line %d: %s", i+1, key))
		}
	}

	if cfg.ServerURL == "" {
		return nil, fmt.Errorf("config %s: SERVER_URL is required", path)
	}
	u, err := url.Parse(cfg.ServerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("config %s: SERVER_URL %q is not a valid http(s) URL", path, cfg.ServerURL)
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("config %s: TOKEN is required", path)
	}
	if cfg.QNDBPath == "" {
		return nil, fmt.Errorf("config %s: QNDB_PATH must not be empty", path)
	}
	if cfg.CAFile != "" {
		// Validate the CA bundle at startup (missing/unreadable/unparsable
		// must be a loud error, never silently ignored — HTTPS trust is
		// on the line).
		if _, err := caPoolFromFile(cfg.CAFile); err != nil {
			return nil, fmt.Errorf("config %s: CA_FILE %s: %v", path, cfg.CAFile, err)
		}
	}
	if len(unknown) > 0 {
		cfg.Warnings = unknown
	}
	return cfg, nil
}

// parseFlexBool accepts the value styles used in the wild: 1/0, true/false,
// yes/no, on/off (case-insensitive).
func parseFlexBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("cannot parse %q as a boolean", s)
}
