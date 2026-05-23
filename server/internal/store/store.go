// Package store 提供公网服务器的 SQLite 数据持久层。
//
// 职责：
//   - 数据库迁移与表结构管理
//   - 系统配置的读写（管理员密码哈希、授权码）
//   - 家庭 CRUD（创建、可见性设置、绑定/解绑家庭服务器、授权/撤销客户端）
//   - 设备管理（注册/更新、黑名单、最后在线时间、LAN 网段）
//   - 流量日志与服务器日志的记录和查询
//   - 仪表盘统计数据聚合
//
// 注意：SQLite 使用 WAL 模式，通过 SetMaxOpenConns(1) 保证串行访问。
// 所有写操作应通过 Hub 层调用，以确保数据变更后触发 front.data_changed 事件。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gohome/shared/protocol"
	"gohome/shared/security"

	_ "modernc.org/sqlite"
)

// 系统配置键名常量。
const (
	// ConfigAdminPassword 管理员密码哈希的配置键。
	ConfigAdminPassword = "admin_password_hash"
	// ConfigAuthCode 授权码的配置键。
	ConfigAuthCode = "auth_code"
)

// Store 封装了 SQLite 数据库连接，提供所有数据访问方法。
type Store struct {
	db *sql.DB
}

// Open 打开或创建 SQLite 数据库，并执行表结构迁移。
// path 为数据库文件路径，父目录必须已存在。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite 串行访问：限制最大打开连接数为 1，避免并发写入冲突。
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate 执行数据库表结构迁移。
// 使用 CREATE TABLE IF NOT EXISTS 保证幂等性，多次调用不会报错。
// 对于已有表的新增列，使用 ALTER TABLE ADD COLUMN 迁移。
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
  note TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(family_id) REFERENCES families(id)
);

CREATE TABLE IF NOT EXISTS family_blacklists (
  family_id INTEGER NOT NULL,
  device_id TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (family_id, device_id),
  FOREIGN KEY(family_id) REFERENCES families(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(device_id) ON DELETE CASCADE
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
	if err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	// 增量迁移：为已有的表添加新列（CREATE TABLE IF NOT EXISTS 不会修改已有表结构）
	s.migrateAddColumn("devices", "note", "TEXT NOT NULL DEFAULT ''")
	s.migrateAddColumn("devices", "ws_endpoint", "TEXT")

	return nil
}

// migrateAddColumn 尝试为已有表添加新列，忽略"列已存在"的错误。
func (s *Store) migrateAddColumn(table, column, definition string) {
	_, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	if err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			// 列已存在，无需操作
			return
		}
		log.Printf("migration warning: ALTER TABLE %s ADD COLUMN %s: %v", table, column, err)
	}
}

// InitDefaults 在首次启动时初始化默认配置值。
// 如果数据库中已有对应配置项，则不做任何修改（幂等）。
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

// ============================================================
// 系统配置
// ============================================================

// GetConfig 读取系统配置项的值。
// 如果配置项不存在，返回 sql.ErrNoRows。
func (s *Store) GetConfig(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM system_config WHERE key = ?`, key).Scan(&value)
	return value, err
}

// SetConfig 写入系统配置项（upsert 语义：存在则更新，不存在则插入）。
func (s *Store) SetConfig(key, value string) error {
	_, err := s.db.Exec(`
INSERT INTO system_config(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value
`, key, value)
	return err
}

// CheckAdminPassword 验证管理员密码是否正确。
// 返回 true 表示密码正确，false 表示密码错误。
func (s *Store) CheckAdminPassword(password string) (bool, error) {
	hash, err := s.GetConfig(ConfigAdminPassword)
	if err != nil {
		return false, err
	}
	return security.VerifyPassword(password, hash), nil
}

// UpdateAdminPassword 更新管理员密码（先哈希后存储）。
func (s *Store) UpdateAdminPassword(password string) error {
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	return s.SetConfig(ConfigAdminPassword, hash)
}

// ============================================================
// 设备管理
// ============================================================

// UpsertDevice 注册或更新设备信息。
// 如果设备已存在则更新类型、令牌、公钥、在线时间、UDP 端口和端点地址。
// token 为认证成功后分配的设备令牌，endpoint 为 WebSocket 连接的远程地址。
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

// TouchDevice 更新设备的最后在线时间为当前时刻。
// 通常在收到设备心跳（ping）时调用。
func (s *Store) TouchDevice(deviceID string) error {
	_, err := s.db.Exec(`UPDATE devices SET last_online = CURRENT_TIMESTAMP WHERE device_id = ?`, deviceID)
	return err
}

// IsBlacklisted 检查设备是否在黑名单中。
// 如果设备不存在，返回 false（不视为黑名单设备）。
func (s *Store) IsBlacklisted(deviceID string) (bool, error) {
	var value bool
	err := s.db.QueryRow(`SELECT is_blacklisted FROM devices WHERE device_id = ?`, deviceID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value, err
}

// SetBlacklisted 设置设备的黑名单状态。
// value=true 表示拉黑，value=false 表示解除拉黑。
func (s *Store) SetBlacklisted(deviceID string, value bool) error {
	_, err := s.db.Exec(`UPDATE devices SET is_blacklisted = ? WHERE device_id = ?`, value, deviceID)
	return err
}

// UpdateLAN 更新家庭服务器的局域网网段信息。
// 返回 (changed, error)：changed=true 表示网段发生了变化（与之前不同）。
// 网段变化时需要通知相关客户端。
func (s *Store) UpdateLAN(deviceID, cidr string) (bool, error) {
	var previous string
	if err := s.db.QueryRow(`SELECT COALESCE(lan_cidr, '') FROM devices WHERE device_id = ?`, deviceID).Scan(&previous); err != nil {
		return false, err
	}
	_, err := s.db.Exec(`UPDATE devices SET lan_cidr = ?, lan_updated_at = CURRENT_TIMESTAMP WHERE device_id = ?`, cidr, deviceID)
	return previous != cidr, err
}

// ============================================================
// 家庭管理
// ============================================================

// CreateFamily 创建一个新的家庭，返回自增的家庭 ID。
// visibility 默认为 "private"，必须是 "public" 或 "private"。
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

// SetFamilyVisibility 设置家庭的可见性。
func (s *Store) SetFamilyVisibility(familyID int64, visibility string) error {
	if visibility != protocol.FamilyVisibilityPublic && visibility != protocol.FamilyVisibilityPrivate {
		return fmt.Errorf("invalid visibility: %s", visibility)
	}
	_, err := s.db.Exec(`UPDATE families SET visibility = ? WHERE id = ?`, visibility, familyID)
	return err
}

// BindHomeServer 将家庭服务器绑定到指定家庭。
// 使用事务确保：1) 目标设备确实是 home-server 类型；2) 家庭的 home_server_id 更新；3) 设备的 family_id 更新。
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

// GrantFamilyDevice 授权客户端访问指定家庭（用于私密家庭）。
// 使用 ON CONFLICT DO NOTHING 保证幂等性（重复授权不报错）。
func (s *Store) GrantFamilyDevice(familyID int64, deviceID string) error {
	_, err := s.db.Exec(`
INSERT INTO family_device_grants(family_id, device_id) VALUES(?, ?)
ON CONFLICT(family_id, device_id) DO NOTHING
`, familyID, deviceID)
	return err
}

// RevokeFamilyDevice 撤销客户端对指定家庭的访问权限。
func (s *Store) RevokeFamilyDevice(familyID int64, deviceID string) error {
	_, err := s.db.Exec(`DELETE FROM family_device_grants WHERE family_id = ? AND device_id = ?`, familyID, deviceID)
	return err
}

// UnbindHomeServer 解绑家庭的家庭服务器。
// 使用事务确保：1) 将家庭服务器的 family_id 置空；2) 将家庭的 home_server_id 置空。
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

// ============================================================
// 家庭查询
// ============================================================

// ListFamiliesForDevice 获取指定设备可访问的家庭列表。
// 返回所有公开家庭 + 该设备被授权访问的私密家庭。
// 设备通常是客户端，用于 client.family.list 请求。
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

// CanDeviceAccessFamily 检查指定设备是否有权访问指定家庭。
// 公开家庭对所有已认证设备开放，私密家庭仅对被授权设备开放。
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

// ListFamilies 获取所有家庭列表（管理端使用）。
// 返回所有家庭，不论可见性。
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

// scanFamilies 从 SQL 查询结果扫描家庭列表。
// online 参数不为 nil 时，填充家庭服务器的在线状态。
func scanFamilies(rows *sql.Rows, online map[string]bool) ([]protocol.Family, error) {
	families := make([]protocol.Family, 0)
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

// GetFamilyHomeServer 获取指定家庭绑定的家庭服务器设备信息。
// 用于 P2P 打洞时查找目标家庭服务器。
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

// ============================================================
// 设备查询
// ============================================================

// ListDevices 获取所有设备列表（管理端使用）。
// 按最后在线时间倒序排列。
func (s *Store) ListDevices() ([]protocol.Device, error) {
	rows, err := s.db.Query(`
SELECT device_id, device_type, family_id, token, is_blacklisted, last_online,
       COALESCE(lan_cidr, ''), udp_port, COALESCE(note, ''), COALESCE(ws_endpoint, '')
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
		if err := rows.Scan(&d.DeviceID, &d.DeviceType, &familyID, &d.Token, &d.IsBlacklisted, &lastOnline, &d.LANCIDR, &d.UDPPort, &d.Note, &d.WSEndpoint); err != nil {
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

// ============================================================
// 日志与统计
// ============================================================

// LogTraffic 记录一条流量日志。
// direction 为 "in" 或 "out"，bytes 为本周期流量字节数。
func (s *Store) LogTraffic(deviceID, direction string, bytes int64) error {
	_, err := s.db.Exec(`INSERT INTO traffic_logs(device_id, direction, bytes) VALUES(?, ?, ?)`, deviceID, direction, bytes)
	return err
}

// AddLog 添加一条服务器日志。
// level 为日志级别（info/warn/error），source 为来源模块，message 为日志内容。
// 注意：此方法忽略写入错误，因为日志写入失败不应中断业务流程。
func (s *Store) AddLog(level, source, message string) {
	_, _ = s.db.Exec(`INSERT INTO server_logs(level, source, message) VALUES(?, ?, ?)`, level, source, message)
}

// ListLogs 获取服务器日志列表，按 ID 倒序（最新在前）。
// limit 为返回条数上限，默认 100，最大 500。
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

// Dashboard 获取仪表盘统计数据。
// onlineDevices 为当前在线设备数（由 Hub 层提供），started 为服务器启动时间。
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

// ============================================================
// 家庭详情与统计
// ============================================================

// FamilyDetail 获取家庭详情，包括授权设备列表、流量统计和黑名单。
func (s *Store) FamilyDetail(familyID int64) (protocol.FamilyDetail, error) {
	var detail protocol.FamilyDetail

	// 获取家庭基本信息
	families, err := s.ListFamilies()
	if err != nil {
		return detail, err
	}
	for _, f := range families {
		if f.ID == familyID {
			detail.Family = f
			break
		}
	}
	if detail.Family.ID == 0 {
		return detail, fmt.Errorf("family not found")
	}

	// 获取授权设备列表
	detail.Devices, err = s.listFamilyDevices(familyID)
	if err != nil {
		return detail, err
	}

	// 获取流量统计
	detail.Traffic, err = s.familyTraffic(familyID)
	if err != nil {
		return detail, err
	}

	// 获取黑名单
	detail.BlacklistedDevices, err = s.listFamilyBlacklist(familyID)
	if err != nil {
		return detail, err
	}

	return detail, nil
}

// listFamilyDevices 获取指定家庭关联的设备列表。
func (s *Store) listFamilyDevices(familyID int64) ([]protocol.Device, error) {
	rows, err := s.db.Query(`
SELECT d.device_id, d.device_type, d.family_id, d.token, d.is_blacklisted,
       d.last_online, COALESCE(d.lan_cidr, ''), d.udp_port, COALESCE(d.note, '')
FROM devices d
WHERE d.device_id = (SELECT f.home_server_id FROM families f WHERE f.id = ?)
   OR d.device_id IN (
     SELECT g.device_id FROM family_device_grants g WHERE g.family_id = ?
   )
ORDER BY d.last_online DESC
`, familyID, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []protocol.Device
	for rows.Next() {
		var d protocol.Device
		var familyIDVal sql.NullInt64
		var lastOnline sql.NullTime
		var note string
		if err := rows.Scan(&d.DeviceID, &d.DeviceType, &familyIDVal, &d.Token, &d.IsBlacklisted, &lastOnline, &d.LANCIDR, &d.UDPPort, &note); err != nil {
			return nil, err
		}
		if familyIDVal.Valid {
			v := familyIDVal.Int64
			d.FamilyID = &v
		}
		if lastOnline.Valid {
			d.LastOnline = &lastOnline.Time
		}
		_ = note
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// familyTraffic 获取指定家庭的流量统计。
func (s *Store) familyTraffic(familyID int64) (protocol.TrafficStats, error) {
	var stats protocol.TrafficStats
	// Get traffic from home server device
	err := s.db.QueryRow(`
SELECT
  COALESCE(SUM(CASE WHEN tl.direction IN ('up', 'out') THEN tl.bytes ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN tl.direction IN ('down', 'in') THEN tl.bytes ELSE 0 END), 0)
FROM traffic_logs tl
WHERE tl.device_id = (SELECT f.home_server_id FROM families f WHERE f.id = ?)
`, familyID).Scan(&stats.UpBytes, &stats.DownBytes)
	if err != nil {
		return stats, err
	}
	return stats, nil
}

// FamilyTraffic 获取指定家庭的流量统计（公开方法）。
func (s *Store) FamilyTraffic(familyID int64) (protocol.TrafficStats, error) {
	return s.familyTraffic(familyID)
}

// listFamilyBlacklist 获取指定家庭的黑名单设备 ID 列表。
func (s *Store) listFamilyBlacklist(familyID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT device_id FROM family_blacklists WHERE family_id = ?`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AddFamilyBlacklist 将设备加入家庭黑名单。
func (s *Store) AddFamilyBlacklist(familyID int64, deviceID string) error {
	_, err := s.db.Exec(`
INSERT INTO family_blacklists(family_id, device_id) VALUES(?, ?)
ON CONFLICT(family_id, device_id) DO NOTHING
`, familyID, deviceID)
	if err != nil {
		return err
	}
	// Also revoke the grant if exists
	return s.RevokeFamilyDevice(familyID, deviceID)
}

// RemoveFamilyBlacklist 将设备从家庭黑名单中移除。
func (s *Store) RemoveFamilyBlacklist(familyID int64, deviceID string) error {
	_, err := s.db.Exec(`DELETE FROM family_blacklists WHERE family_id = ? AND device_id = ?`, familyID, deviceID)
	return err
}

// IsFamilyBlacklisted 检查设备是否在指定家庭的黑名单中。
func (s *Store) IsFamilyBlacklisted(familyID int64, deviceID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM family_blacklists WHERE family_id = ? AND device_id = ?`, familyID, deviceID).Scan(&count)
	return count > 0, err
}

// ============================================================
// 设备详情与统计
// ============================================================

// DeviceDetail 获取设备详情，包括所属家庭、流量统计和备注。
func (s *Store) DeviceDetail(deviceID string) (protocol.DeviceDetail, error) {
	var detail protocol.DeviceDetail

	// 获取设备基本信息
	d, err := s.getDevice(deviceID)
	if err != nil {
		return detail, err
	}
	detail.Device = d

	// 获取设备所属/被授权的家庭
	detail.Families, err = s.listDeviceFamilies(deviceID)
	if err != nil {
		return detail, err
	}

	// 获取流量统计
	detail.Traffic, err = s.DeviceTraffic(deviceID)
	if err != nil {
		return detail, err
	}

	// 获取备注
	detail.Note = d.Note

	return detail, nil
}

// getDevice 获取单个设备信息。
func (s *Store) getDevice(deviceID string) (protocol.Device, error) {
	var d protocol.Device
	var familyID sql.NullInt64
	var lastOnline sql.NullTime
	var note string
	err := s.db.QueryRow(`
SELECT device_id, device_type, family_id, token, is_blacklisted, last_online,
       COALESCE(lan_cidr, ''), udp_port, COALESCE(note, '')
FROM devices
WHERE device_id = ?
`, deviceID).Scan(&d.DeviceID, &d.DeviceType, &familyID, &d.Token, &d.IsBlacklisted, &lastOnline, &d.LANCIDR, &d.UDPPort, &note)
	if err != nil {
		return d, err
	}
	if familyID.Valid {
		v := familyID.Int64
		d.FamilyID = &v
	}
	if lastOnline.Valid {
		d.LastOnline = &lastOnline.Time
	}
	d.Note = note
	return d, nil
}

// listDeviceFamilies 获取设备关联的家庭列表。
func (s *Store) listDeviceFamilies(deviceID string) ([]protocol.Family, error) {
	rows, err := s.db.Query(`
SELECT f.id, f.name, f.visibility, f.created_at, COALESCE(f.home_server_id, ''),
       COALESCE(d.lan_cidr, ''), d.lan_updated_at
FROM families f
LEFT JOIN devices d ON d.device_id = f.home_server_id
WHERE f.home_server_id = ?
   OR EXISTS (
     SELECT 1 FROM family_device_grants g
     WHERE g.family_id = f.id AND g.device_id = ?
   )
ORDER BY f.created_at DESC
`, deviceID, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFamilies(rows, nil)
}

// DeviceTraffic 获取指定设备的流量统计。
func (s *Store) DeviceTraffic(deviceID string) (protocol.TrafficStats, error) {
	var stats protocol.TrafficStats
	err := s.db.QueryRow(`
SELECT
  COALESCE(SUM(CASE WHEN direction IN ('up', 'out') THEN bytes ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN direction IN ('down', 'in') THEN bytes ELSE 0 END), 0)
FROM traffic_logs
WHERE device_id = ?
`, deviceID).Scan(&stats.UpBytes, &stats.DownBytes)
	return stats, err
}

// SetDeviceNote 设置设备备注。
func (s *Store) SetDeviceNote(deviceID, note string) error {
	_, err := s.db.Exec(`UPDATE devices SET note = ? WHERE device_id = ?`, note, deviceID)
	return err
}

// ListFamiliesForDeviceWithBlacklist 获取设备可访问的家庭列表（排除黑名单）。
func (s *Store) ListFamiliesForDeviceWithBlacklist(deviceID string) ([]protocol.Family, error) {
	rows, err := s.db.Query(`
SELECT f.id, f.name, f.visibility, f.created_at, COALESCE(f.home_server_id, ''),
       COALESCE(d.lan_cidr, ''), d.lan_updated_at
FROM families f
LEFT JOIN devices d ON d.device_id = f.home_server_id
WHERE (f.visibility = 'public'
   OR EXISTS (
     SELECT 1 FROM family_device_grants g
     WHERE g.family_id = f.id AND g.device_id = ?
   ))
  AND NOT EXISTS (
     SELECT 1 FROM family_blacklists b
     WHERE b.family_id = f.id AND b.device_id = ?
   )
ORDER BY f.created_at DESC
`, deviceID, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFamilies(rows, nil)
}
