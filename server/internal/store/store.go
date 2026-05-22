package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gohome/shared/protocol"
	"gohome/shared/security"

	_ "modernc.org/sqlite"
)

const (
	ConfigAdminPassword = "admin_password_hash"
	ConfigAuthCode      = "auth_code"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS system_config (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS families (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  visibility TEXT NOT NULL DEFAULT 'private',
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  home_server_id TEXT UNIQUE
);

CREATE TABLE IF NOT EXISTS devices (
  device_id TEXT PRIMARY KEY,
  family_id INTEGER,
  device_type TEXT NOT NULL,
  token TEXT,
  public_key TEXT,
  is_blacklisted BOOLEAN NOT NULL DEFAULT 0,
  last_online DATETIME,
  lan_cidr TEXT,
  lan_updated_at DATETIME,
  udp_port INTEGER NOT NULL DEFAULT 0,
  ws_endpoint TEXT,
  FOREIGN KEY(family_id) REFERENCES families(id)
);

CREATE TABLE IF NOT EXISTS family_device_grants (
  family_id INTEGER NOT NULL,
  device_id TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (family_id, device_id),
  FOREIGN KEY(family_id) REFERENCES families(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(device_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS traffic_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id TEXT NOT NULL,
  direction TEXT NOT NULL,
  bytes INTEGER NOT NULL,
  timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS server_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  level TEXT NOT NULL,
  source TEXT NOT NULL,
  message TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`)
	return err
}

func (s *Store) InitDefaults(defaultPassword, defaultAuthCode string) error {
	if _, err := s.GetConfig(ConfigAdminPassword); errors.Is(err, sql.ErrNoRows) {
		hash, err := security.HashPassword(defaultPassword)
		if err != nil {
			return err
		}
		if err := s.SetConfig(ConfigAdminPassword, hash); err != nil {
			return err
		}
	}
	if _, err := s.GetConfig(ConfigAuthCode); errors.Is(err, sql.ErrNoRows) {
		if err := s.SetConfig(ConfigAuthCode, defaultAuthCode); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetConfig(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM system_config WHERE key = ?`, key).Scan(&value)
	return value, err
}

func (s *Store) SetConfig(key, value string) error {
	_, err := s.db.Exec(`
INSERT INTO system_config(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value
`, key, value)
	return err
}

func (s *Store) CheckAdminPassword(password string) (bool, error) {
	hash, err := s.GetConfig(ConfigAdminPassword)
	if err != nil {
		return false, err
	}
	return security.VerifyPassword(password, hash), nil
}

func (s *Store) UpdateAdminPassword(password string) error {
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	return s.SetConfig(ConfigAdminPassword, hash)
}

func (s *Store) UpsertDevice(d protocol.DeviceAuthParams, token, endpoint string) error {
	_, err := s.db.Exec(`
INSERT INTO devices(device_id, device_type, token, public_key, last_online, udp_port, ws_endpoint)
VALUES(?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  device_type = excluded.device_type,
  token = excluded.token,
  public_key = excluded.public_key,
  last_online = CURRENT_TIMESTAMP,
  udp_port = excluded.udp_port,
  ws_endpoint = excluded.ws_endpoint
`, d.DeviceID, d.DeviceType, token, d.PublicKey, d.UDPPort, endpoint)
	return err
}

func (s *Store) TouchDevice(deviceID string) error {
	_, err := s.db.Exec(`UPDATE devices SET last_online = CURRENT_TIMESTAMP WHERE device_id = ?`, deviceID)
	return err
}

func (s *Store) IsBlacklisted(deviceID string) (bool, error) {
	var value bool
	err := s.db.QueryRow(`SELECT is_blacklisted FROM devices WHERE device_id = ?`, deviceID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value, err
}

func (s *Store) SetBlacklisted(deviceID string, value bool) error {
	_, err := s.db.Exec(`UPDATE devices SET is_blacklisted = ? WHERE device_id = ?`, value, deviceID)
	return err
}

func (s *Store) UpdateLAN(deviceID, cidr string) error {
	_, err := s.db.Exec(`UPDATE devices SET lan_cidr = ?, lan_updated_at = CURRENT_TIMESTAMP WHERE device_id = ?`, cidr, deviceID)
	return err
}

func (s *Store) CreateFamily(name, visibility string) (int64, error) {
	if visibility == "" {
		visibility = protocol.FamilyVisibilityPrivate
	}
	if visibility != protocol.FamilyVisibilityPublic && visibility != protocol.FamilyVisibilityPrivate {
		return 0, fmt.Errorf("invalid visibility: %s", visibility)
	}
	res, err := s.db.Exec(`INSERT INTO families(name, visibility) VALUES(?, ?)`, name, visibility)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetFamilyVisibility(familyID int64, visibility string) error {
	if visibility != protocol.FamilyVisibilityPublic && visibility != protocol.FamilyVisibilityPrivate {
		return fmt.Errorf("invalid visibility: %s", visibility)
	}
	_, err := s.db.Exec(`UPDATE families SET visibility = ? WHERE id = ?`, visibility, familyID)
	return err
}

func (s *Store) BindHomeServer(familyID int64, homeServerID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var deviceType string
	if err := tx.QueryRow(`SELECT device_type FROM devices WHERE device_id = ?`, homeServerID).Scan(&deviceType); err != nil {
		return err
	}
	if deviceType != protocol.DeviceTypeHomeServer {
		return fmt.Errorf("device is not a home server")
	}
	if _, err := tx.Exec(`UPDATE families SET home_server_id = ? WHERE id = ?`, homeServerID, familyID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE devices SET family_id = ? WHERE device_id = ?`, familyID, homeServerID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GrantFamilyDevice(familyID int64, deviceID string) error {
	_, err := s.db.Exec(`
INSERT INTO family_device_grants(family_id, device_id) VALUES(?, ?)
ON CONFLICT(family_id, device_id) DO NOTHING
`, familyID, deviceID)
	return err
}

func (s *Store) RevokeFamilyDevice(familyID int64, deviceID string) error {
	_, err := s.db.Exec(`DELETE FROM family_device_grants WHERE family_id = ? AND device_id = ?`, familyID, deviceID)
	return err
}

func (s *Store) UnbindHomeServer(familyID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var homeServerID sql.NullString
	if err := tx.QueryRow(`SELECT home_server_id FROM families WHERE id = ?`, familyID).Scan(&homeServerID); err != nil {
		return err
	}
	if homeServerID.Valid {
		if _, err := tx.Exec(`UPDATE devices SET family_id = NULL WHERE device_id = ?`, homeServerID.String); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE families SET home_server_id = NULL WHERE id = ?`, familyID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListFamiliesForDevice(deviceID string) ([]protocol.Family, error) {
	rows, err := s.db.Query(`
SELECT f.id, f.name, f.visibility, f.created_at, COALESCE(f.home_server_id, ''),
       COALESCE(d.lan_cidr, ''), d.lan_updated_at
FROM families f
LEFT JOIN devices d ON d.device_id = f.home_server_id
WHERE f.visibility = 'public'
   OR EXISTS (
     SELECT 1 FROM family_device_grants g
     WHERE g.family_id = f.id AND g.device_id = ?
   )
ORDER BY f.created_at DESC
`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFamilies(rows, nil)
}

func (s *Store) CanDeviceAccessFamily(deviceID string, familyID int64) (bool, error) {
	var allowed bool
	err := s.db.QueryRow(`
SELECT EXISTS (
  SELECT 1
  FROM families f
  WHERE f.id = ?
    AND (
      f.visibility = 'public'
      OR EXISTS (
        SELECT 1
        FROM family_device_grants g
        WHERE g.family_id = f.id AND g.device_id = ?
      )
    )
)
`, familyID, deviceID).Scan(&allowed)
	return allowed, err
}

func (s *Store) ListFamilies() ([]protocol.Family, error) {
	rows, err := s.db.Query(`
SELECT f.id, f.name, f.visibility, f.created_at, COALESCE(f.home_server_id, ''),
       COALESCE(d.lan_cidr, ''), d.lan_updated_at
FROM families f
LEFT JOIN devices d ON d.device_id = f.home_server_id
ORDER BY f.created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFamilies(rows, nil)
}

func scanFamilies(rows *sql.Rows, online map[string]bool) ([]protocol.Family, error) {
	var families []protocol.Family
	for rows.Next() {
		var f protocol.Family
		var homeServerID string
		var lanUpdated sql.NullTime
		if err := rows.Scan(&f.ID, &f.Name, &f.Visibility, &f.CreatedAt, &homeServerID, &f.LANCIDR, &lanUpdated); err != nil {
			return nil, err
		}
		f.HomeServerID = homeServerID
		if lanUpdated.Valid {
			f.LANUpdatedAt = &lanUpdated.Time
		}
		if online != nil {
			f.HomeServerOnline = online[homeServerID]
		}
		families = append(families, f)
	}
	return families, rows.Err()
}

func (s *Store) GetFamilyHomeServer(familyID int64) (protocol.Device, error) {
	var d protocol.Device
	var familyIDValue sql.NullInt64
	var lastOnline sql.NullTime
	err := s.db.QueryRow(`
SELECT d.device_id, d.device_type, d.family_id, d.token, d.is_blacklisted,
       d.last_online, COALESCE(d.lan_cidr, ''), d.udp_port
FROM families f
JOIN devices d ON d.device_id = f.home_server_id
WHERE f.id = ?
`, familyID).Scan(&d.DeviceID, &d.DeviceType, &familyIDValue, &d.Token, &d.IsBlacklisted, &lastOnline, &d.LANCIDR, &d.UDPPort)
	if familyIDValue.Valid {
		v := familyIDValue.Int64
		d.FamilyID = &v
	}
	if lastOnline.Valid {
		d.LastOnline = &lastOnline.Time
	}
	return d, err
}

func (s *Store) ListDevices() ([]protocol.Device, error) {
	rows, err := s.db.Query(`
SELECT device_id, device_type, family_id, token, is_blacklisted, last_online,
       COALESCE(lan_cidr, ''), udp_port
FROM devices
ORDER BY last_online DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []protocol.Device
	for rows.Next() {
		var d protocol.Device
		var familyID sql.NullInt64
		var lastOnline sql.NullTime
		if err := rows.Scan(&d.DeviceID, &d.DeviceType, &familyID, &d.Token, &d.IsBlacklisted, &lastOnline, &d.LANCIDR, &d.UDPPort); err != nil {
			return nil, err
		}
		if familyID.Valid {
			v := familyID.Int64
			d.FamilyID = &v
		}
		if lastOnline.Valid {
			d.LastOnline = &lastOnline.Time
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) LogTraffic(deviceID, direction string, bytes int64) error {
	_, err := s.db.Exec(`INSERT INTO traffic_logs(device_id, direction, bytes) VALUES(?, ?, ?)`, deviceID, direction, bytes)
	return err
}

func (s *Store) AddLog(level, source, message string) {
	_, _ = s.db.Exec(`INSERT INTO server_logs(level, source, message) VALUES(?, ?, ?)`, level, source, message)
}

func (s *Store) ListLogs(limit int) ([]protocol.LogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`
SELECT id, level, source, message, created_at
FROM server_logs
ORDER BY id DESC
LIMIT ?
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []protocol.LogEntry
	for rows.Next() {
		var entry protocol.LogEntry
		if err := rows.Scan(&entry.ID, &entry.Level, &entry.Source, &entry.Message, &entry.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, entry)
	}
	return logs, rows.Err()
}

func (s *Store) Dashboard(onlineDevices int, started time.Time) (protocol.Dashboard, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(`SELECT SUM(bytes) FROM traffic_logs`).Scan(&total); err != nil {
		return protocol.Dashboard{}, err
	}
	return protocol.Dashboard{
		OnlineDevices: onlineDevices,
		TotalBytes:    total.Int64,
		UptimeSeconds: int64(time.Since(started).Seconds()),
	}, nil
}
