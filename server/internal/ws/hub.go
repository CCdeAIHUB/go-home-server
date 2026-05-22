package ws

import (
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"gohome/server/internal/store"
	"gohome/shared/protocol"
	"gohome/shared/security"
)

type Hub struct {
	store    *store.Store
	started  time.Time
	upgrader websocket.Upgrader

	mu      sync.RWMutex
	admin   *Session
	devices map[string]*Session
}

type Session struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	probeMu   sync.Mutex
	kind      string
	deviceID  string
	token     string
	publicKey string
	udpPort   int
	remote    string
	failures  int
	probes    map[string]time.Time
	latencyMS int64
}

func NewHub(s *store.Store) *Hub {
	h := &Hub{
		store:   s,
		started: time.Now(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		devices: map[string]*Session{},
	}
	go h.latencyProbeLoop()
	return h
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade websocket: %v", err)
		return
	}
	session := &Session{conn: conn, remote: r.RemoteAddr}
	defer h.closeSession(session)

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		reply := h.handle(session, env)
		if reply.ID != "" || reply.Error != nil || reply.Result != nil {
			session.write(reply)
		}
	}
}

func (h *Hub) handle(s *Session, env protocol.Envelope) protocol.Envelope {
	switch env.Action {
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
			params, err := protocol.DecodeParams[protocol.BindHomeServerParams](env.Params)
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
			params, err := protocol.DecodeParams[protocol.FamilyGrantParams](env.Params)
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
				h.forceOffline(params.DeviceID, "blacklisted")
			}
			if params.Value {
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
			var params struct {
				Limit int `json:"limit"`
			}
			if len(env.Params) > 0 {
				decoded, err := protocol.DecodeParams[struct {
					Limit int `json:"limit"`
				}](env.Params)
				if err != nil {
					return nil, err
				}
				params = decoded
			}
			return h.store.ListLogs(params.Limit)
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
			params, err := protocol.DecodeParams[struct {
				AuthCode string `json:"auth_code"`
			}](env.Params)
			if err != nil {
				return nil, err
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
			params, err := protocol.DecodeParams[struct {
				OldPassword string `json:"old_password"`
				NewPassword string `json:"new_password"`
			}](env.Params)
			if err != nil {
				return nil, err
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
		return protocol.Result(env.ID, map[string]any{"pong": true, "server_now": time.Now(), "latency_ms": s.currentLatency()})
	default:
		return protocol.Error(env.ID, "unknown_action", "unknown action")
	}
}

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

func (h *Hub) deviceAuth(s *Session, env protocol.Envelope) protocol.Envelope {
	params, err := protocol.DecodeParams[protocol.DeviceAuthParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	if params.DeviceID == "" || params.DeviceType == "" {
		return protocol.Error(env.ID, "bad_request", "device_id and device_type are required")
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
		return protocol.Error(env.ID, "unauthorized", "authorization code is incorrect")
	}
	validTime, skew := security.ValidateTimeKey(authCode, params.TimeKey, params.Timestamp, time.Now(), 2)
	if !validTime {
		s.failures++
		if s.failures >= 3 {
			_ = s.conn.Close()
		}
		if skew {
			return protocol.Error(env.ID, "clock_skew", "time check failed, please check system time")
		}
		return protocol.Error(env.ID, "time_key_invalid", "time key invalid")
	}
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
	if old := h.devices[params.DeviceID]; old != nil && old != s {
		_ = old.conn.Close()
	}
	h.devices[params.DeviceID] = s
	h.mu.Unlock()

	h.store.AddLog("info", "device", "设备上线: "+params.DeviceID+" ("+params.DeviceType+")")
	h.dataChanged("device.auth")

	return protocol.Result(env.ID, protocol.DeviceAuthResult{
		Token:      token,
		ServerNow:  time.Now(),
		DeviceID:   params.DeviceID,
		DeviceType: params.DeviceType,
	})
}

func (h *Hub) deviceLANReport(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" || s.kind != protocol.DeviceTypeHomeServer {
		return protocol.Error(env.ID, "unauthorized", "home server auth required")
	}
	params, err := protocol.DecodeParams[protocol.LANReportParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	if err := h.store.UpdateLAN(s.deviceID, params.LANCIDR); err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	h.store.AddLog("info", "lan", "家庭服务器上报网段: "+params.LANCIDR)
	h.dataChanged("device.lan_report")
	return protocol.Result(env.ID, ok())
}

func (h *Hub) clientFamilyList(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" || s.kind != protocol.DeviceTypeClient {
		return protocol.Error(env.ID, "unauthorized", "client auth required")
	}
	families, err := h.store.ListFamiliesForDevice(s.deviceID)
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	h.markFamilyOnline(families)
	return protocol.Result(env.ID, families)
}

func (h *Hub) holePunchRequest(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" || s.kind != protocol.DeviceTypeClient {
		return protocol.Error(env.ID, "unauthorized", "client auth required")
	}
	params, err := protocol.DecodeParams[protocol.HolePunchRequestParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
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

	client := protocol.PeerCandidate{
		DeviceID:  s.deviceID,
		Endpoint:  peerEndpoint(s),
		UDPPort:   s.udpPort,
		PublicKey: s.publicKey,
	}
	server := protocol.PeerCandidate{
		DeviceID:  home.DeviceID,
		Endpoint:  peerEndpoint(homeSession),
		UDPPort:   homeSession.udpPort,
		LANCIDR:   home.LANCIDR,
		PublicKey: homeSession.publicKey,
	}
	offer := protocol.HolePunchOffer{
		SessionID: sessionID,
		FamilyID:  params.FamilyID,
		Request:   params,
		Client:    client,
		Server:    server,
	}
	if event, err := protocol.Event(protocol.EventP2PHolePunchOffer, offer); err == nil {
		homeSession.write(event)
	}
	h.store.AddLog("info", "p2p", "发起打洞: "+s.deviceID+" -> "+home.DeviceID)
	h.dataChanged("p2p.hole_punch")
	return protocol.Result(env.ID, offer)
}

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

func (h *Hub) latencyPong(s *Session, env protocol.Envelope) protocol.Envelope {
	if s.deviceID == "" {
		return protocol.Error(env.ID, "unauthorized", "device auth required")
	}
	params, err := protocol.DecodeParams[protocol.LatencyPongParams](env.Params)
	if err != nil {
		return protocol.Error(env.ID, "bad_request", err.Error())
	}
	s.probeMu.Lock()
	started, ok := s.probes[params.ProbeID]
	if ok {
		delete(s.probes, params.ProbeID)
		s.latencyMS = time.Since(started).Milliseconds()
	}
	s.probeMu.Unlock()
	if ok {
		h.dataChanged("stats.latency")
	}
	return protocol.Result(env.ID, map[string]any{"latency_ms": s.currentLatency()})
}

func (h *Hub) requireAdmin(s *Session, env protocol.Envelope, fn func() (any, error)) protocol.Envelope {
	if s.kind != protocol.DeviceTypeConsole {
		return protocol.Error(env.ID, "unauthorized", "admin login required")
	}
	result, err := fn()
	if err != nil {
		return protocol.Error(env.ID, "internal_error", err.Error())
	}
	return protocol.Result(env.ID, result)
}

func (h *Hub) closeSession(s *Session) {
	_ = s.conn.Close()
	deviceID := s.deviceID
	kind := s.kind
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
	}
}

func (h *Hub) onlineDeviceCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.devices)
}

func (h *Hub) familiesWithOnline() ([]protocol.Family, error) {
	families, err := h.store.ListFamilies()
	if err != nil {
		return nil, err
	}
	h.markFamilyOnline(families)
	return families, nil
}

func (h *Hub) markFamilyOnline(families []protocol.Family) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := range families {
		families[i].HomeServerOnline = h.devices[families[i].HomeServerID] != nil
	}
}

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

func (s *Session) write(env protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.WriteJSON(env)
}

func (s *Session) currentLatency() int64 {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	return s.latencyMS
}

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

func peerEndpoint(s *Session) string {
	host, _, err := net.SplitHostPort(s.remote)
	if err != nil {
		host = s.remote
	}
	if s.udpPort <= 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(s.udpPort))
}

func ok() map[string]bool {
	return map[string]bool{"ok": true}
}
