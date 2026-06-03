// Package ws 实现了公网服务器的 WebSocket Hub，负责：
//   - 管理所有 WebSocket 连接会话（管理员控制台 + 设备端）
//   - 路由 JSON-RPC 2.0 请求到对应的处理函数
//   - 转发 P2P 打洞信令和候选地址
//   - 推送服务器事件（数据变更、延迟探测、强制下线等）
//   - 维护在线设备状态和延迟信息
package ws

import (
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"gohome/server/internal/store"
	"gohome/shared/protocol"
	"gohome/shared/security"
	"gohome/shared/tunnel"
)

// Hub 是 WebSocket 连接的中心管理器，维护所有活跃会话。
// 管理员（Web 控制台）最多同时只有一个活跃会话（单点登录）。
// 设备端按 device_id 索引，同一设备的新连接会顶替旧连接。
type Hub struct {
	// store 数据持久层引用。
	store *store.Store
	// started 服务器启动时间，用于计算运行时长。
	started time.Time
	// upgrader WebSocket 升级器。
	upgrader websocket.Upgrader
	// udpPort 公网服务器的主 UDP 监听端口，0 表示未启用。
	udpPort int
	// udpPorts 公网服务器的 UDP 观测端口列表。
	udpPorts []int

	// mu 保护 admin 和 devices 的并发访问。
	mu sync.RWMutex
	// admin 当前活跃的管理员会话，nil 表示未登录。
	admin *Session
	// devices 在线设备会话映射，key 为 device_id。
	devices map[string]*Session
	// punches 保存短期活跃打洞会话，用于实时转发新发现的候选端点。
	punches map[string]punchAssist
}

type punchAssist struct {
	sessionID string
	clientID  string
	homeID    string
	expiresAt time.Time
}

// Session 表示一个 WebSocket 连接会话。
// 每个会话可能属于管理员（Web 控制台）或设备（客户端/家庭服务器）。
type Session struct {
	// conn 底层 WebSocket 连接。
	conn *websocket.Conn
	// mu 保护 conn 的并发写入（WebSocket 不支持并发写）。
	mu sync.Mutex
	// probeMu 保护 probes 和 latencyMS 的并发访问。
	probeMu sync.Mutex
	// kind 会话类型，对应 protocol.DeviceType* 常量。空字符串表示未认证。
	kind string
	// deviceID 设备唯一标识，认证后设置。
	deviceID string
	// token 认证令牌，用于后续请求校验。
	token string
	// publicKey 设备的 SM2 公钥（PEM 格式），用于 P2P 密钥交换。
	publicKey string
	// udpPort 设备监听的 UDP 端口，用于 P2P 打洞。
	udpPort int
	// udpPorts 设备为本轮 P2P 打洞准备的 UDP 端口集合。
	udpPorts []int
	// remote 客户端远程地址（host:port），用于 P2P 端点推断。
	remote string
	// observedEndpoint 服务器通过 UDP 注册探测观察到的 NAT 映射公网端点（host:port）。
	observedEndpoint string
	// observedEndpoints 服务器通过多 UDP 观测端口收集到的 NAT 映射公网端点。
	observedEndpoints []string
	// failures 连续 time_key 校验失败次数，达到 3 次强制断开连接。
	failures int
	// probes 延迟探测映射，key 为 probe_id，value 为发送时间。
	probes map[string]time.Time
	// latencyMS 最近一次测量的延迟（毫秒）。
	latencyMS int64
}

// NewHub 创建一个新的 Hub 实例并启动延迟探测循环。
// udpPorts 为公网服务器的 UDP 监听端口列表，空列表表示未启用 UDP 端点发现。
func NewHub(s *store.Store, udpPorts ...int) *Hub {
	ports := normalizeUDPPorts(udpPorts)
	primaryPort := 0
	if len(ports) > 0 {
		primaryPort = ports[0]
	}
	h := &Hub{
		store:   s,
		started: time.Now(),
		upgrader: websocket.Upgrader{
			// 允许所有来源的 WebSocket 连接。
			// 生产环境应验证 Origin 头，防止 CSRF 攻击。
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		udpPort:  primaryPort,
		udpPorts: ports,
		devices:  map[string]*Session{},
		punches:  map[string]punchAssist{},
	}
	go h.latencyProbeLoop()
	return h
}

func normalizeUDPPorts(ports []int) []int {
	out := make([]int, 0, len(ports))
	seen := map[int]bool{}
	for _, port := range ports {
		if port < 1 || port > 65535 || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	return out
}

// ServeHTTP 处理 WebSocket 升级请求，进入消息读取循环。
// 连接断开后自动清理会话资源。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade websocket: %v", err)
		return
	}
	log.Printf("[ws] new connection from %s", r.RemoteAddr)
	session := &Session{conn: conn, remote: r.RemoteAddr}
	defer h.closeSession(session)

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			// 读取失败（连接关闭或格式错误），退出循环触发 closeSession。
			return
		}
		reply := h.handle(session, env)
		if reply.ID != "" || reply.Error != nil || reply.Result != nil {
			session.write(reply)
		}
	}
}

// handle 将 JSON-RPC 请求路由到对应的处理函数。
// 返回的 Envelope 作为响应发送回客户端。
func (h *Hub) handle(s *Session, env protocol.Envelope) protocol.Envelope {
	if env.Action != "" {
		log.Printf("[rpc] action=%s id=%s from=%s", env.Action, env.ID, s.deviceID)
	}
	switch env.Action {
	// ===== 管理控制台操作（需要管理员登录）=====
	case protocol.ActionFrontLogin:
		return h.frontLogin(s, env)
	case protocol.ActionFrontDashboard:
		return h.requireAdmin(s, env, func() (any, error) {
			return h.store.Dashboard(h.onlineDeviceCount(), h.started)
		})
	case protocol.ActionFrontFamilyList:
		return h.requireAdmin(s, env, func() (any, error) {
			return h.familiesWithOnline()
		})
	case protocol.ActionFrontFamilyCreate:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.CreateFamilyParams](env.Params)
			if err != nil {
				return nil, err
			}
			id, err := h.store.CreateFamily(params.Name, params.Visibility)
			if err != nil {
				return nil, err
			}
			h.store.AddLog("info", "family", "创建家庭: "+params.Name)
			h.dataChanged("family.create")
			return map[string]any{"id": id}, nil
		})
	case protocol.ActionFrontFamilySetVisible:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.SetFamilyVisibilityParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.SetFamilyVisibility(params.FamilyID, params.Visibility); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "family", "家庭可见性已更新")
			h.dataChanged("family.visibility")
			return ok(), nil
		})
	case protocol.ActionFrontFamilyBindServer:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.BindHomeServerParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.BindHomeServer(params.FamilyID, params.HomeServerID); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "family", "绑定家庭服务器: "+params.HomeServerID)
			h.dataChanged("family.bind_home_server")
			return ok(), nil
		})
	case protocol.ActionFrontFamilyUnbindServer:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.UnbindHomeServerParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.UnbindHomeServer(params.FamilyID); err != nil {
				return nil, err
			}
			h.store.AddLog("warn", "family", "家庭服务器已解绑")
			h.dataChanged("family.unbind_home_server")
			return ok(), nil
		})
	case protocol.ActionFrontFamilyGrantDevice:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyGrantParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.GrantFamilyDevice(params.FamilyID, params.DeviceID); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "family", "授权私密家庭客户端: "+params.DeviceID)
			h.dataChanged("family.grant")
			return ok(), nil
		})
	case protocol.ActionFrontFamilyRevokeDevice:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyRevokeParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.RevokeFamilyDevice(params.FamilyID, params.DeviceID); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "family", "撤销私密家庭客户端授权: "+params.DeviceID)
			h.dataChanged("family.revoke")
			return ok(), nil
		})
	case protocol.ActionFrontDeviceList:
		return h.requireAdmin(s, env, func() (any, error) {
			devices, err := h.store.ListDevices()
			if err != nil {
				return nil, err
			}
			h.markOnline(devices)
			return devices, nil
		})
	case protocol.ActionFrontDeviceBlacklist:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.DeviceTargetParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.SetBlacklisted(params.DeviceID, params.Value); err != nil {
				return nil, err
			}
			if params.Value {
				// 拉黑后立即强制设备下线
				h.forceOffline(params.DeviceID, "blacklisted")
				h.store.AddLog("warn", "device", "设备已拉黑: "+params.DeviceID)
			} else {
				h.store.AddLog("info", "device", "设备已解除拉黑: "+params.DeviceID)
			}
			h.dataChanged("device.blacklist")
			return ok(), nil
		})
	case protocol.ActionFrontDeviceForceOffline:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.DeviceTargetParams](env.Params)
			if err != nil {
				return nil, err
			}
			h.forceOffline(params.DeviceID, "forced")
			h.store.AddLog("warn", "device", "设备被强制下线: "+params.DeviceID)
			h.dataChanged("device.force_offline")
			return ok(), nil
		})
	case protocol.ActionFrontLogList:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.LogListParams](env.Params)
			if err != nil {
				// 参数解析失败时使用默认 limit
				params.Limit = 0
			}
			return h.store.ListLogs(params.Limit)
		})
	case protocol.ActionFrontFamilyDetail:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyDetailParams](env.Params)
			if err != nil {
				return nil, err
			}
			detail, err := h.store.FamilyDetail(params.FamilyID)
			if err != nil {
				return nil, err
			}
			h.markOnline(detail.Devices)
			// 直接设置 HomeServerOnline，而非创建值拷贝切片
			h.mu.RLock()
			detail.Family.HomeServerOnline = h.devices[detail.Family.HomeServerID] != nil
			h.mu.RUnlock()
			return detail, nil
		})
	case protocol.ActionFrontFamilyTraffic:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyDetailParams](env.Params)
			if err != nil {
				return nil, err
			}
			return h.store.FamilyTraffic(params.FamilyID)
		})
	case protocol.ActionFrontFamilyBlacklist:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyBlacklistParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.AddFamilyBlacklist(params.FamilyID, params.DeviceID); err != nil {
				return nil, err
			}
			h.store.AddLog("warn", "family", "设备加入家庭黑名单: "+params.DeviceID)
			h.dataChanged("family.blacklist")
			return ok(), nil
		})
	case protocol.ActionFrontFamilyUnblacklist:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyBlacklistParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.RemoveFamilyBlacklist(params.FamilyID, params.DeviceID); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "family", "设备移出家庭黑名单: "+params.DeviceID)
			h.dataChanged("family.unblacklist")
			return ok(), nil
		})
	case protocol.ActionFrontDeviceDetail:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.DeviceDetailParams](env.Params)
			if err != nil {
				return nil, err
			}
			detail, err := h.store.DeviceDetail(params.DeviceID)
			if err != nil {
				return nil, err
			}
			// 直接设置设备在线状态和延迟，而非创建值拷贝切片
			h.mu.RLock()
			if session := h.devices[detail.Device.DeviceID]; session != nil {
				detail.Device.Online = true
				detail.Device.LatencyMS = session.currentLatency()
			}
			h.mu.RUnlock()
			h.markFamilyOnline(detail.Families)
			return detail, nil
		})
	case protocol.ActionFrontDeviceTraffic:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.DeviceDetailParams](env.Params)
			if err != nil {
				return nil, err
			}
			return h.store.DeviceTraffic(params.DeviceID)
		})
	case protocol.ActionFrontDeviceSetNote:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.DeviceNoteParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.SetDeviceNote(params.DeviceID, params.Note); err != nil {
				return nil, err
			}
			h.dataChanged("device.note")
			return ok(), nil
		})
	case protocol.ActionFrontDeviceGrantFamily:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.FamilyGrantParams](env.Params)
			if err != nil {
				return nil, err
			}
			if err := h.store.GrantFamilyDevice(params.FamilyID, params.DeviceID); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "device", "客户端授权加入家庭: "+params.DeviceID)
			h.dataChanged("device.grant_family")
			return ok(), nil
		})
	case protocol.ActionFrontConfigGet:
		return h.requireAdmin(s, env, func() (any, error) {
			authCode, err := h.store.GetConfig(store.ConfigAuthCode)
			if err != nil {
				return nil, err
			}
			return map[string]any{"auth_code": authCode}, nil
		})
	case protocol.ActionFrontConfigUpdateAuth:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.ConfigUpdateAuthParams](env.Params)
			if err != nil {
				return nil, err
			}
			if params.AuthCode == "" {
				return nil, errors.New("auth_code cannot be empty")
			}
			if err := h.store.SetConfig(store.ConfigAuthCode, params.AuthCode); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "config", "授权码已更新")
			h.dataChanged("config.auth_code")
			return ok(), nil
		})
	case protocol.ActionFrontConfigUpdatePass:
		return h.requireAdmin(s, env, func() (any, error) {
			params, err := protocol.DecodeParams[protocol.ConfigUpdatePassParams](env.Params)
			if err != nil {
				return nil, err
			}
			if params.NewPassword == "" {
				return nil, errors.New("new password cannot be empty")
			}
			okPassword, err := h.store.CheckAdminPassword(params.OldPassword)
			if err != nil {
				return nil, err
			}
			if !okPassword {
				return nil, errors.New("old password is incorrect")
			}
			if err := h.store.UpdateAdminPassword(params.NewPassword); err != nil {
				return nil, err
			}
			h.store.AddLog("info", "config", "管理员密码已更新")
			h.dataChanged("config.password")
			return ok(), nil
		})

	// ===== 设备端操作 =====
	case protocol.ActionDeviceAuth:
		return h.deviceAuth(s, env)
	case protocol.ActionDeviceLANReport:
		return h.deviceLANReport(s, env)
	case protocol.ActionClientFamilyList:
		return h.clientFamilyList(s, env)
	case protocol.ActionP2PHolePunchReq:
		return h.holePunchRequest(s, env)
	case protocol.ActionP2PCandidate:
		return h.candidateRelay(s, env)
	case protocol.ActionStatsTraffic:
		return h.trafficReport(s, env)
	case protocol.ActionStatsLatencyPong:
		return h.latencyPong(s, env)
	case protocol.ActionPing:
		return h.ping(s, env)

	default:
		return protocol.Error(env.ID, "unknown_action", "unknown action: "+env.Action)
	}
}

// ============================================================
// 管理控制台操作
// ============================================================

// frontLogin 处理管理员登录请求。
// 实现单点登录：新登录会踢掉旧的管理员会话。
// 登录成功后返回 token，后续管理请求需校验此 token。
func (h *Hub) frontLogin(s *Session, env protocol.Envelope) protocol.Envelope {
	params, err := protocol.DecodeParams[protocol.LoginParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	valid, err := h.store.CheckAdminPassword(params.Password)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	if !valid {
		return protocol.Error(env.ID, "unauthorized", "password is incorrect")
	}
	token, err := security.NewToken(8)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}

	h.mu.Lock()
	// 踢掉旧的管理员会话（单点登录）
	if h.admin != nil && h.admin != s {
		if event, err := protocol.Event(protocol.EventFrontSessionRevoked, map[string]any{"reason": "账号在别处登录"}); err == nil {
			h.admin.write(event)
		}
		_ = h.admin.conn.Close()
	}
	s.kind = protocol.DeviceTypeConsole
	s.token = token
	h.admin = s
	h.mu.Unlock()

	h.store.AddLog("info", "auth", "Web 控制台登录")
	h.dataChanged("front.login")

	return protocol.Result(env.ID, protocol.LoginResult{Token: token})
}

// ============================================================
// 设备认证与心跳
// ============================================================

// deviceAuth 处理设备认证请求。
// 校验流程：1) 检查必填字段；2) 验证设备类型合法性；3) 检查黑名单；
// 4) 验证授权码；5) 校验 time_key（防重放）；6) 注册或更新设备信息。
func (h *Hub) deviceAuth(s *Session, env protocol.Envelope) protocol.Envelope {
	params, err := protocol.DecodeParams[protocol.DeviceAuthParams](env.Params)
	if err != nil {
		log.Printf("[auth] decode error: %v", err)
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	log.Printf("[auth] device=%s type=%s", params.DeviceID, params.DeviceType)
	if params.DeviceID == "" || params.DeviceType == "" {
		return protocol.Error(env.ID, "bad_request", "device_id and device_type are required")
	}
	// 校验设备类型合法性
	if params.DeviceType != protocol.DeviceTypeClient && params.DeviceType != protocol.DeviceTypeHomeServer {
		return protocol.Error(env.ID, "bad_request", "device_type must be 'client' or 'home-server'")
	}
	blacklisted, err := h.store.IsBlacklisted(params.DeviceID)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	if blacklisted {
		return protocol.Error(env.ID, "blacklisted", "device is blacklisted")
	}
	authCode, err := h.store.GetConfig(store.ConfigAuthCode)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	if params.AuthCode != authCode {
		log.Printf("[auth] device=%s auth code mismatch", params.DeviceID)
		return protocol.Error(env.ID, "unauthorized", "authorization code is incorrect")
	}
	// 校验 time_key（基于 SM3-HMAC 的时间窗口密钥，防重放攻击）
	if reply := validateTimeKey(s, env.ID, authCode, params.TimeKey, params.Timestamp); reply != nil {
		log.Printf("[auth] device=%s time_key validation failed", params.DeviceID)
		return *reply
	}
	log.Printf("[auth] device=%s authenticated successfully", params.DeviceID)
	token, err := security.NewToken(16)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	if err := h.store.UpsertDevice(params, token, s.remote); err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}

	h.mu.Lock()
	s.kind = params.DeviceType
	s.deviceID = params.DeviceID
	s.token = token
	s.publicKey = params.PublicKey
	s.udpPort = params.UDPPort
	s.udpPorts = normalizeDeviceUDPPorts(nil, params.UDPPort)
	s.observedEndpoint = ""
	s.observedEndpoints = nil
	// 同一设备的新连接顶替旧连接（单设备单会话）
	if old := h.devices[params.DeviceID]; old != nil && old != s {
		_ = old.conn.Close()
	}
	h.devices[params.DeviceID] = s
	h.mu.Unlock()

	h.store.AddLog("info", "device", "设备上线: "+params.DeviceID+" ("+params.DeviceType+")")
	h.dataChanged("device.auth")

	// 家庭服务器上线时，通知所有有权限的客户端刷新家庭列表
	if params.DeviceType == protocol.DeviceTypeHomeServer {
		go h.notifyHomeServerChanged(params.DeviceID, true)
	}

	return protocol.Result(env.ID, protocol.DeviceAuthResult{
		Token:          token,
		ServerNow:      time.Now(),
		DeviceID:       params.DeviceID,
		DeviceType:     params.DeviceType,
		ServerUDPPort:  h.udpPort,
		ServerUDPPorts: append([]int(nil), h.udpPorts...),
	})
}

// ping 处理设备心跳请求。
// 已认证设备需携带 time_key 进行校验，同时更新最后在线时间。
func (h *Hub) ping(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID != "" {
		params, err := protocol.DecodeParams[protocol.HeartbeatParams](env.Params)
		if err != nil {
			return protocol.Error(env.ID, "bad_request", err.Error())
		}
		authCode, err := h.store.GetConfig(store.ConfigAuthCode)
		if err != nil {
			return protocol.Error(env.ID, "internal_error", err.Error())
		}
		if reply := validateTimeKey(s, env.ID, authCode, params.TimeKey, params.Timestamp); reply != nil {
			return *reply
		}
		if err := h.store.TouchDevice(s.deviceID); err != nil {
			return protocol.Error(env.ID, "internal_error", err.Error())
		}
	}
	return protocol.Result(env.ID, map[string]any{"pong": true, "server_now": time.Now(), "latency_ms": s.currentLatency()})
}

// validateTimeKey 校验请求中的 time_key 和 timestamp。
// secret 为用于生成 time_key 的密钥（设备使用 auth_code）。
// 连续 3 次校验失败将强制断开连接，防止暴力破解。
func validateTimeKey(s *Session, envID, secret, timeKey string, timestamp int64) *protocol.Envelope {
	validTime, skew := security.ValidateTimeKey(secret, timeKey, timestamp, time.Now(), 2)
	if validTime {
		s.failures = 0
		return nil
	}
	s.failures++
	if s.failures >= 3 && s.conn != nil {
		// 连续 3 次失败，强制断开连接
		_ = s.conn.Close()
	}
	if skew {
		reply := protocol.Error(envID, "clock_skew", "time check failed, please check system time")
		return &reply
	}
	reply := protocol.Error(envID, "time_key_invalid", "time key invalid")
	return &reply
}

// ============================================================
// 设备操作
// ============================================================

// deviceLANReport 处理家庭服务器的局域网网段上报。
// 仅家庭服务器可调用。当网段发生变化时，通知所有相关客户端。
func (h *Hub) deviceLANReport(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" || s.kind != protocol.DeviceTypeHomeServer {
		return protocol.Error(env.ID, "unauthorized", "home server auth required")
	}
	params, err := protocol.DecodeParams[protocol.LANReportParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	changed, err := h.store.UpdateLAN(s.deviceID, params.LANCIDR)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	h.store.AddLog("info", "lan", "家庭服务器上报网段: "+params.LANCIDR)
	if changed {
		h.familyLANChanged(s.deviceID, params.LANCIDR)
	}
	h.dataChanged("device.lan_report")
	return protocol.Result(env.ID, ok())
}

// clientFamilyList 处理客户端获取可访问家庭列表的请求。
// 返回公开家庭 + 该客户端被授权访问的私密家庭，排除家庭黑名单中的家庭。
func (h *Hub) clientFamilyList(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" || s.kind != protocol.DeviceTypeClient {
		return protocol.Error(env.ID, "unauthorized", "client auth required")
	}
	families, err := h.store.ListFamiliesForDeviceWithBlacklist(s.deviceID)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	// 确保 nil slice 被序列化为 [] 而非 null（安卓客户端需解析数组）
	if families == nil {
		families = []protocol.Family{}
	}
	h.markFamilyOnline(families)
	return protocol.Result(env.ID, families)
}

// holePunchRequest 处理客户端的 P2P 打洞请求。
// 流程：1) 校验客户端权限；2) 查找目标家庭服务器；3) 生成打洞会话 ID；
// 4) 构建打洞邀请（含双方连接信息）；5) 转发给家庭服务器；6) 返回给客户端。
func (h *Hub) holePunchRequest(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" || s.kind != protocol.DeviceTypeClient {
		return protocol.Error(env.ID, "unauthorized", "client auth required")
	}
	params, err := protocol.DecodeParams[protocol.HolePunchRequestParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	if params.ClientUDPPort > 0 {
		s.udpPort = params.ClientUDPPort
	}
	s.udpPorts = normalizeDeviceUDPPorts(params.ClientUDPPorts, s.udpPort)
	allowed, err := h.store.CanDeviceAccessFamily(s.deviceID, params.FamilyID)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	if !allowed {
		return protocol.Error(env.ID, "forbidden", "client cannot access family")
	}
	home, err := h.store.GetFamilyHomeServer(params.FamilyID)
	if err != nil {
		return protocol.Error(env.ID, "not_available", "family has no bound home server")
	}
	h.mu.RLock()
	homeSession := h.devices[home.DeviceID]
	h.mu.RUnlock()
	if homeSession == nil {
		return protocol.Error(env.ID, "not_available", "home server is offline")
	}
	if homeSession.publicKey == "" {
		return protocol.Error(env.ID, "not_available", "home server has no SM2 public key")
	}
	sessionID, err := security.NewToken(16)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}

	// 构建双方的连接信息
	client := protocol.PeerCandidate{
		DeviceID:         s.deviceID,
		Endpoint:         peerEndpoint(s),
		ObservedEndpoint: s.observedEndpoint,
		Candidates:       peerCandidates(s),
		UDPPort:          s.udpPort,
		RemoteAddr:       s.remote,
		PublicKey:        s.publicKey,
	}
	server := protocol.PeerCandidate{
		DeviceID:         home.DeviceID,
		Endpoint:         peerEndpoint(homeSession),
		ObservedEndpoint: homeSession.observedEndpoint,
		Candidates:       peerCandidates(homeSession),
		UDPPort:          homeSession.udpPort,
		RemoteAddr:       homeSession.remote,
		LANCIDR:          home.LANCIDR,
		PublicKey:        homeSession.publicKey,
	}
	if len(client.Candidates) == 0 {
		return protocol.Error(env.ID, "not_available", "client has no usable IPv4 UDP candidate")
	}
	if len(server.Candidates) == 0 {
		return protocol.Error(env.ID, "not_available", "home server has no usable IPv4 UDP candidate")
	}
	offer := protocol.HolePunchOffer{
		SessionID: sessionID,
		FamilyID:  params.FamilyID,
		Request:   params,
		Client:    client,
		Server:    server,
	}
	h.mu.Lock()
	h.punches[sessionID] = punchAssist{
		sessionID: sessionID,
		clientID:  s.deviceID,
		homeID:    home.DeviceID,
		expiresAt: time.Now().Add(90 * time.Second),
	}
	h.mu.Unlock()
	// 将打洞邀请转发给家庭服务器
	if event, err := protocol.Event(protocol.EventP2PHolePunchOffer, offer); err == nil {
		homeSession.write(event)
	}
	h.store.AddLog("info", "p2p", "发起打洞: "+s.deviceID+" -> "+home.DeviceID)
	h.dataChanged("p2p.hole_punch")
	return protocol.Result(env.ID, offer)
}

// candidateRelay 处理候选地址转发请求。
// 一端发现新的候选地址后，通过服务器中继转发给对端。
func (h *Hub) candidateRelay(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" {
		return protocol.Error(env.ID, "unauthorized", "device auth required")
	}
	params, err := protocol.DecodeParams[protocol.CandidateRelayParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	h.mu.RLock()
	target := h.devices[params.TargetDeviceID]
	h.mu.RUnlock()
	if target == nil {
		return protocol.Error(env.ID, "not_available", "target device offline")
	}
	if event, err := protocol.Event(protocol.EventP2PCandidate, map[string]any{
		"from_device_id": s.deviceID,
		"candidate":      params.Candidate,
		"session_id":     params.SessionID,
	}); err == nil {
		target.write(event)
	}
	return protocol.Result(env.ID, ok())
}

// trafficReport 处理设备流量上报。
func (h *Hub) trafficReport(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" {
		return protocol.Error(env.ID, "unauthorized", "device auth required")
	}
	params, err := protocol.DecodeParams[protocol.TrafficReportParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	if err := h.store.LogTraffic(s.deviceID, params.Direction, params.Bytes); err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	h.dataChanged("stats.traffic")
	return protocol.Result(env.ID, ok())
}

// latencyPong 处理设备回复的延迟探测。
// 根据 probe_id 找到对应的探测开始时间，计算延迟。
func (h *Hub) latencyPong(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" {
		return protocol.Error(env.ID, "unauthorized", "device auth required")
	}
	params, err := protocol.DecodeParams[protocol.LatencyPongParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	s.probeMu.Lock()
	started, found := s.probes[params.ProbeID]
	if found {
		delete(s.probes, params.ProbeID)
		s.latencyMS = time.Since(started).Milliseconds()
	}
	s.probeMu.Unlock()
	if found {
		h.dataChanged("stats.latency")
	}
	return protocol.Result(env.ID, map[string]any{"latency_ms": s.currentLatency()})
}

// ============================================================
// 权限校验
// ============================================================

// requireAdmin 校验管理员权限后执行回调函数。
// 支持两种鉴权方式：
//  1. 当前会话已通过 front.login 认证（s.kind == console 且 s.token 匹配）
//  2. 请求消息中携带了有效的 token（用于页面刷新后 WebSocket 重连时恢复会话）
func (h *Hub) requireAdmin(s *Session, env protocol.Envelope, fn func() (any, error)) protocol.Envelope {
	h.mu.RLock()
	expectedToken := ""
	if h.admin != nil {
		expectedToken = h.admin.token
	}
	h.mu.RUnlock()

	// 方式1：当前 session 已登录
	sessionValid := s.kind == protocol.DeviceTypeConsole && s.token != "" && s.token == expectedToken
	// 方式2：消息中携带了有效 token（页面刷新重连场景）
	tokenValid := env.Token != "" && env.Token == expectedToken

	if !sessionValid && !tokenValid {
		return protocol.Error(env.ID, "unauthorized", "admin login required")
	}

	// 如果是通过消息 token 验证的，将此 session 也标记为管理员
	if tokenValid && !sessionValid {
		s.kind = protocol.DeviceTypeConsole
		s.token = env.Token
	}

	result, err := fn()
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	return protocol.Result(env.ID, result)
}

// ============================================================
// 会话管理与事件推送
// ============================================================

// closeSession 清理断开连接的会话资源。
// 如果是管理员会话则清除 admin 引用，如果是设备会话则从 devices 映射中移除。
func (h *Hub) closeSession(s *Session) {
	_ = s.conn.Close()
	deviceID := s.deviceID
	kind := s.kind
	log.Printf("[ws] connection closed: device=%s kind=%s", deviceID, kind)
	h.mu.Lock()
	if h.admin == s {
		h.admin = nil
	}
	if s.deviceID != "" && h.devices[s.deviceID] == s {
		delete(h.devices, s.deviceID)
	}
	h.mu.Unlock()
	if deviceID != "" {
		h.store.AddLog("info", "device", "设备离线: "+deviceID+" ("+kind+")")
		h.dataChanged("device.offline")
		// 家庭服务器离线时，通知所有有权限的客户端刷新家庭列表
		if kind == protocol.DeviceTypeHomeServer {
			go h.notifyHomeServerChanged(deviceID, false)
		}
	}
}

// onlineDeviceCount 返回当前在线设备数量。
func (h *Hub) onlineDeviceCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.devices)
}

// familiesWithOnline 获取所有家庭列表并标记家庭服务器在线状态。
func (h *Hub) familiesWithOnline() ([]protocol.Family, error) {
	families, err := h.store.ListFamilies()
	if err != nil {
		return nil, err
	}
	h.markFamilyOnline(families)
	return families, nil
}

// markFamilyOnline 为家庭列表标记家庭服务器的在线状态。
func (h *Hub) markFamilyOnline(families []protocol.Family) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := range families {
		families[i].HomeServerOnline = h.devices[families[i].HomeServerID] != nil
	}
}

// markOnline 为设备列表标记在线状态和延迟信息。
func (h *Hub) markOnline(devices []protocol.Device) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := range devices {
		if session := h.devices[devices[i].DeviceID]; session != nil {
			devices[i].Online = true
			devices[i].LatencyMS = session.currentLatency()
		}
	}
}

// forceOffline 强制设备下线。
// 先发送 force_offline 事件通知设备，然后关闭其 WebSocket 连接。
func (h *Hub) forceOffline(deviceID, reason string) {
	h.mu.RLock()
	target := h.devices[deviceID]
	h.mu.RUnlock()
	if target == nil {
		return
	}
	if event, err := protocol.Event(protocol.EventDeviceForceOffline, map[string]any{"reason": reason}); err == nil {
		target.write(event)
	}
	_ = target.conn.Close()
}

// write 向 WebSocket 连接写入一条消息（线程安全）。
func (s *Session) write(env protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.WriteJSON(env)
}

// currentLatency 返回当前延迟值（毫秒）。
func (s *Session) currentLatency() int64 {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	return s.latencyMS
}

// dataChanged 向管理员推送数据变更事件。
// reason 描述变更原因，前端收到后应刷新对应的数据面板。
func (h *Hub) dataChanged(reason string) {
	event, err := protocol.Event(protocol.EventFrontDataChanged, map[string]any{
		"reason": reason,
		"at":     time.Now(),
	})
	if err != nil {
		return
	}
	h.mu.RLock()
	admin := h.admin
	h.mu.RUnlock()
	if admin != nil {
		admin.write(event)
	}
}

// familyLANChanged 在家庭网段变化时通知所有有权限的客户端。
func (h *Hub) familyLANChanged(homeServerID, cidr string) {
	families, err := h.store.ListFamilies()
	if err != nil {
		log.Printf("list families for LAN change: %v", err)
		return
	}
	var family protocol.Family
	for _, candidate := range families {
		if candidate.HomeServerID == homeServerID {
			family = candidate
			break
		}
	}
	if family.ID == 0 {
		return
	}
	event, err := protocol.Event(protocol.EventFamilyLANChanged, map[string]any{
		"family_id": family.ID,
		"lan_cidr":  cidr,
	})
	if err != nil {
		return
	}

	// 遍历所有在线客户端，检查权限后推送
	h.mu.RLock()
	sessions := make([]*Session, 0, len(h.devices))
	for _, session := range h.devices {
		if session.kind == protocol.DeviceTypeClient {
			sessions = append(sessions, session)
		}
	}
	h.mu.RUnlock()
	for _, session := range sessions {
		allowed, err := h.store.CanDeviceAccessFamily(session.deviceID, family.ID)
		if err != nil {
			log.Printf("LAN change access check for %s: %v", session.deviceID, err)
			continue
		}
		if allowed {
			session.write(event)
		}
	}
}

// notifyHomeServerChanged 在家庭服务器上线或离线时通知所有有权限的客户端。
func (h *Hub) notifyHomeServerChanged(homeServerID string, online bool) {
	families, err := h.store.ListFamilies()
	if err != nil {
		log.Printf("list families for home server change: %v", err)
		return
	}
	// 找到该 home-server 关联的所有 family
	type familyChange struct {
		FamilyID     int64
		HomeServerID string
	}
	var changes []familyChange
	for _, f := range families {
		if f.HomeServerID == homeServerID {
			changes = append(changes, familyChange{FamilyID: f.ID, HomeServerID: homeServerID})
		}
	}
	if len(changes) == 0 {
		return
	}

	// 收集所有在线客户端
	h.mu.RLock()
	sessions := make([]*Session, 0, len(h.devices))
	for _, session := range h.devices {
		if session.kind == protocol.DeviceTypeClient {
			sessions = append(sessions, session)
		}
	}
	h.mu.RUnlock()

	for _, change := range changes {
		event, err := protocol.Event(protocol.EventFamilyHomeServerChanged, map[string]any{
			"family_id":      change.FamilyID,
			"home_server_id": change.HomeServerID,
			"online":         online,
		})
		if err != nil {
			continue
		}
		for _, session := range sessions {
			allowed, err := h.store.CanDeviceAccessFamily(session.deviceID, change.FamilyID)
			if err != nil {
				continue
			}
			if allowed {
				session.write(event)
			}
		}
	}
}

// latencyProbeLoop 定期向所有在线设备发送延迟探测。
// 每 10 秒一轮，每个设备收到探测后应回复 stats.latency_pong。
func (h *Hub) latencyProbeLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.RLock()
		sessions := make([]*Session, 0, len(h.devices))
		for _, session := range h.devices {
			sessions = append(sessions, session)
		}
		h.mu.RUnlock()
		for _, session := range sessions {
			h.sendLatencyProbe(session)
		}
	}
}

// sendLatencyProbe 向指定会话发送一条延迟探测事件。
// 记录发送时间，设备回复后用于计算延迟。
func (h *Hub) sendLatencyProbe(s *Session) {
	probeID, err := security.NewToken(8)
	if err != nil {
		return
	}
	s.probeMu.Lock()
	if s.probes == nil {
		s.probes = map[string]time.Time{}
	}
	s.probes[probeID] = time.Now()
	// 清理超过 60 秒未回复的探测记录，防止内存泄漏
	now := time.Now()
	for id, t := range s.probes {
		if now.Sub(t) > 60*time.Second {
			delete(s.probes, id)
		}
	}
	s.probeMu.Unlock()
	event, err := protocol.Event(protocol.EventDeviceLatencyProbe, map[string]any{
		"probe_id": probeID,
		"sent_at":  time.Now(),
	})
	if err != nil {
		return
	}
	s.write(event)
}

// peerEndpoint returns the preferred IPv4 UDP endpoint for a session.
func peerEndpoint(s *Session) string {
	candidates := peerCandidates(s)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// peerCandidates builds ordered, deduplicated IPv4 endpoints for direct UDP punching.
func peerCandidates(s *Session) []string {
	if s == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(endpoint string) {
		normalized, ok := normalizeIPv4Endpoint(endpoint)
		if !ok || seen[normalized] {
			return
		}
		seen[normalized] = true
		out = append(out, normalized)
	}

	for _, endpoint := range s.observedEndpoints {
		add(endpoint)
	}
	add(s.observedEndpoint)
	for _, host := range peerCandidateHosts(s) {
		for _, port := range peerCandidatePorts(s) {
			add(net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	return out
}

func normalizeDeviceUDPPorts(ports []int, primary int) []int {
	out := make([]int, 0, len(ports)+1)
	seen := map[int]bool{}
	add := func(port int) {
		if port < 1 || port > 65535 || seen[port] {
			return
		}
		seen[port] = true
		out = append(out, port)
	}
	add(primary)
	for _, port := range ports {
		add(port)
	}
	return out
}

func peerCandidatePorts(s *Session) []int {
	if len(s.udpPorts) > 0 {
		return s.udpPorts
	}
	return normalizeDeviceUDPPorts(nil, s.udpPort)
}

func peerCandidateHosts(s *Session) []string {
	hosts := make([]string, 0, len(s.observedEndpoints)+2)
	seen := map[string]bool{}
	add := func(endpoint string) {
		host, ok := endpointHost(endpoint)
		if !ok || seen[host] {
			return
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	for _, endpoint := range s.observedEndpoints {
		add(endpoint)
	}
	add(s.observedEndpoint)
	add(s.remote)
	return hosts
}

func endpointHost(endpoint string) (string, bool) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || ip.To4() == nil {
		return "", false
	}
	return ip.To4().String(), true
}

func normalizeIPv4Endpoint(endpoint string) (string, bool) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false
	}
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || ip.To4() == nil {
		return "", false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	return net.JoinHostPort(ip.To4().String(), strconv.Itoa(port)), true
}

// ok 返回通用成功响应 {ok: true}。
func ok() map[string]bool {
	return map[string]bool{"ok": true}
}

// ServeUDP 处理 UDP 注册探测包，发现设备 NAT 映射后的公网端点。
// 设备认证后应定期发送 GHU1 Register 包到此端口，服务器记录源地址。
func (h *Hub) ServeUDP(conn net.PacketConn) {
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			log.Printf("[udp] read error: %v", err)
			return
		}
		packet := append([]byte(nil), buf[:n]...)
		ack := h.handleUDPPacket(packet, addr)
		if ack == nil {
			continue
		}
		reply, err := tunnel.MarshalRegisterAck(*ack)
		if err != nil {
			log.Printf("[udp] marshal register ack: %v", err)
			continue
		}
		if _, err := conn.WriteTo(reply, addr); err != nil {
			log.Printf("[udp] write register ack to %s: %v", addr.String(), err)
		}
	}
}

// handleUDPPacket 处理单个 UDP 数据包。
func (h *Hub) handleUDPPacket(packet []byte, addr net.Addr) *tunnel.RegisterAck {
	kind, err := tunnel.PacketKind(packet)
	if err != nil {
		return nil
	}
	if kind != tunnel.PacketRegister {
		return nil
	}
	var reg tunnel.Register
	if err := tunnel.UnmarshalControl(packet, &reg); err != nil {
		return nil
	}
	if reg.DeviceID == "" || reg.Token == "" {
		return nil
	}
	h.mu.RLock()
	session := h.devices[reg.DeviceID]
	h.mu.RUnlock()
	if session == nil || session.token != reg.Token {
		return nil
	}
	endpoint, ok := normalizeIPv4Endpoint(addr.String())
	if !ok {
		return nil
	}
	h.mu.Lock()
	if reg.UDPPort > 0 {
		session.udpPort = reg.UDPPort
		session.udpPorts = normalizeDeviceUDPPorts(session.udpPorts, session.udpPort)
	}
	isNew := !containsEndpoint(session.observedEndpoints, endpoint)
	session.observedEndpoint = endpoint
	session.observedEndpoints = rememberObservedEndpoint(session.observedEndpoints, endpoint)
	observedCount := len(session.observedEndpoints)
	udpPort := session.udpPort
	notifications := h.candidateNotificationsLocked(reg.DeviceID, endpoint, isNew)
	h.mu.Unlock()
	if isNew {
		log.Printf("[udp] NAT endpoint for %s: %s (source=%s, local_port=%d, observed=%d)", reg.DeviceID, endpoint, addr.String(), udpPort, observedCount)
	}
	for _, notification := range notifications {
		notification.target.write(notification.event)
	}
	return &tunnel.RegisterAck{ObservedEndpoint: endpoint}
}

type candidateNotification struct {
	target *Session
	event  protocol.Envelope
}

func (h *Hub) candidateNotificationsLocked(deviceID, endpoint string, isNew bool) []candidateNotification {
	if !isNew {
		return nil
	}
	now := time.Now()
	var notifications []candidateNotification
	for sessionID, assist := range h.punches {
		if now.After(assist.expiresAt) {
			delete(h.punches, sessionID)
			continue
		}
		targetID := ""
		switch deviceID {
		case assist.clientID:
			targetID = assist.homeID
		case assist.homeID:
			targetID = assist.clientID
		default:
			continue
		}
		target := h.devices[targetID]
		if target == nil {
			continue
		}
		event, err := protocol.Event(protocol.EventP2PCandidate, map[string]any{
			"from_device_id": deviceID,
			"candidate":      endpoint,
			"session_id":     sessionID,
		})
		if err == nil {
			notifications = append(notifications, candidateNotification{target: target, event: event})
		}
	}
	return notifications
}

func containsEndpoint(endpoints []string, endpoint string) bool {
	for _, existing := range endpoints {
		if existing == endpoint {
			return true
		}
	}
	return false
}

func rememberObservedEndpoint(endpoints []string, endpoint string) []string {
	const maxObservedEndpoints = 48
	capacity := len(endpoints) + 1
	if capacity > maxObservedEndpoints {
		capacity = maxObservedEndpoints
	}
	out := make([]string, 0, capacity)
	out = append(out, endpoint)
	for _, existing := range endpoints {
		if existing == endpoint {
			continue
		}
		out = append(out, existing)
		if len(out) >= maxObservedEndpoints {
			break
		}
	}
	return out
}
