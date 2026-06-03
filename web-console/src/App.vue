<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import {
  ArrowLeft,
  Ban,
  Cable,
  Check,
  FileText,
  Home,
  KeyRound,
  LayoutDashboard,
  Lock,
  LogIn,
  LogOut,
  Monitor,
  Moon,
  Plus,
  Search,
  Router,
  Settings,
  Shield,
  Sun,
  Smartphone,
  Trash2,
  Users,
  X
} from '@lucide/vue'

const tabs = [
  { id: 'dashboard', label: '仪表盘', icon: LayoutDashboard },
  { id: 'families', label: '家庭', icon: Home },
  { id: 'devices', label: '设备', icon: Users },
  { id: 'logs', label: '日志', icon: FileText },
  { id: 'settings', label: '设置', icon: Settings }
]

const state = reactive({
  ws: null,
  pending: new Map(),
  connected: false,
  loggedIn: false,
  password: '',
  token: '',
  tab: 'dashboard',
  error: '',
  dashboard: { online_devices: 0, total_bytes: 0, uptime_seconds: 0 },
  families: [],
  devices: [],
  logs: [],
  config: { auth_code: '' },
  newFamilyName: '',
  newFamilyVisibility: 'private',
  familySearch: '',
  familyVisibilityFilter: 'all',
  familyOnlineFilter: 'all',
  newAuthCode: '',
  oldPassword: '',
  newPassword: '',
  confirmPassword: '',
  toasts: [],
  // Detail views
  familyDetail: null,
  deviceDetail: null,
  familyDetailData: null,
  deviceDetailData: null,
  deviceSearch: '',
  // Device note editing
  editingNote: false,
  noteValue: '',
  // Grant family to device
  grantingFamily: false,
  selectedFamilyId: ''
})

const loading = ref(false)
const theme = ref('system')
const activeTheme = ref('light')
let refreshTimer = null

function applyTheme(value) {
  const root = document.documentElement
  activeTheme.value = value === 'system'
    ? (window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')
    : value
  root.dataset.theme = activeTheme.value
}

function setTheme(value) {
  theme.value = value
  localStorage.setItem('go-home-console-theme', value)
  applyTheme(value)
}

function toggleTheme() {
  setTheme(activeTheme.value === 'dark' ? 'light' : 'dark')
}

onMounted(() => {
  theme.value = localStorage.getItem('go-home-console-theme') || 'system'
  applyTheme(theme.value)
  window.matchMedia?.('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if (theme.value === 'system') applyTheme('system')
  })
})

const onlineHomeServers = computed(() =>
  state.devices.filter((device) => device.device_type === 'home-server' && device.online && !device.family_id)
)

const clientDevices = computed(() =>
  state.devices.filter((device) => device.device_type === 'client' && !device.is_blacklisted)
)

const filteredFamilies = computed(() => {
  const keyword = state.familySearch.trim().toLowerCase()
  return state.families.filter((family) => {
    const matchesKeyword =
      !keyword ||
      family.name.toLowerCase().includes(keyword) ||
      String(family.id).includes(keyword) ||
      (family.lan_cidr || '').toLowerCase().includes(keyword) ||
      (family.home_server_id || '').toLowerCase().includes(keyword)
    const matchesVisibility =
      state.familyVisibilityFilter === 'all' || family.visibility === state.familyVisibilityFilter
    const matchesOnline =
      state.familyOnlineFilter === 'all' ||
      (state.familyOnlineFilter === 'online' && family.home_server_online) ||
      (state.familyOnlineFilter === 'offline' && !family.home_server_online)
    return matchesKeyword && matchesVisibility && matchesOnline
  })
})

const filteredDevices = computed(() => {
  const keyword = state.deviceSearch.trim().toLowerCase()
  return state.devices.filter((device) => {
    return !keyword ||
      device.device_id.toLowerCase().includes(keyword) ||
      device.device_type.toLowerCase().includes(keyword) ||
      (device.note || '').toLowerCase().includes(keyword)
  })
})

const privateFamilies = computed(() =>
  state.families.filter(f => f.visibility === 'private')
)

function wsURL() {
  if (import.meta.env.VITE_GO_HOME_WS) return import.meta.env.VITE_GO_HOME_WS
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}/ws`
}

function connect() {
  if (state.ws && state.ws.readyState === WebSocket.OPEN) return Promise.resolve()
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsURL())
    state.ws = ws
    ws.onopen = () => {
      state.connected = true
      resolve()
    }
    ws.onerror = () => reject(new Error('无法连接服务器'))
    ws.onclose = () => {
      state.connected = false
      // WebSocket 断开时不立即清除 loggedIn，等重连后再判断
    }
    ws.onmessage = (event) => handleMessage(JSON.parse(event.data))
  })
}

// 尝试用 localStorage 中保存的 token 恢复登录状态
async function tryRestoreSession() {
  const savedToken = localStorage.getItem('go-home-token')
  if (!savedToken) return false
  state.token = savedToken
  try {
    await connect()
    // 发送一个需要鉴权的请求来验证 token 是否仍然有效
    await rpc('front.dashboard')
    state.loggedIn = true
    await refreshAll()
    return true
  } catch {
    // token 失效（服务器重启等），清除并要求重新登录
    state.token = ''
    localStorage.removeItem('go-home-token')
    return false
  }
}

function handleMessage(message) {
  if (message.action === 'front.session.revoked') {
    notify('账号在别处登录', 'error')
    state.loggedIn = false
    state.token = ''
    localStorage.removeItem('go-home-token')
    return
  }
  if (message.action === 'front.data_changed') {
    scheduleRefresh()
    return
  }
  if (!message.id) return
  const task = state.pending.get(message.id)
  if (!task) return
  state.pending.delete(message.id)
  if (message.error) {
    const err = new Error(message.error.message || message.error.code)
    // 如果收到鉴权失败，清除登录状态
    if (message.error.code === 'unauthorized') {
      state.loggedIn = false
      state.token = ''
      localStorage.removeItem('go-home-token')
    }
    task.reject(err)
  } else {
    task.resolve(message.result)
  }
}

async function rpc(action, params = {}) {
  await connect()
  const id = `${Date.now()}-${Math.random().toString(16).slice(2)}`
  const msg = { jsonrpc: '2.0', id, action, params }
  // 如果已有 token，附带在请求中用于服务端鉴权
  if (state.token) msg.token = state.token
  state.ws.send(JSON.stringify(msg))
  return new Promise((resolve, reject) => {
    state.pending.set(id, { resolve, reject })
    setTimeout(() => {
      if (state.pending.has(id)) {
        state.pending.delete(id)
        reject(new Error('请求超时'))
      }
    }, 10000)
  })
}

async function login() {
  state.error = ''
  loading.value = true
  try {
    const result = await rpc('front.login', { password: state.password })
    state.token = result.token
    localStorage.setItem('go-home-token', state.token)
    state.loggedIn = true
    await refreshAll()
    notify('登录成功')
  } catch (error) {
    state.error = error.message
    notify(error.message, 'error')
  } finally {
    loading.value = false
  }
}

async function refreshAll() {
  const [dashboard, families, devices, config, logs] = await Promise.all([
    rpc('front.dashboard'),
    rpc('front.family.list'),
    rpc('front.device.list'),
    rpc('front.config.get'),
    rpc('front.log.list', { limit: 120 })
  ])
  state.dashboard = dashboard
  state.families = families || []
  state.devices = devices || []
  state.config = config || { auth_code: '' }
  state.logs = logs || []
  state.newAuthCode = state.config.auth_code || ''
}

async function createFamily() {
  if (!state.newFamilyName.trim()) return
  await command('家庭已创建', async () => {
    await rpc('front.family.create', {
      name: state.newFamilyName.trim(),
      visibility: state.newFamilyVisibility
    })
    state.newFamilyName = ''
    await refreshAll()
  })
}

async function setVisibility(family, visibility) {
  await command('家庭可见性已保存', async () => {
    await rpc('front.family.set_visibility', { family_id: family.id, visibility })
    await refreshAll()
  })
}

async function bindHomeServer(family, deviceID) {
  if (!deviceID) return
  await command('家庭服务器已绑定', async () => {
    await rpc('front.family.bind_home_server', { family_id: family.id, home_server_id: deviceID })
    await refreshAll()
  })
}

async function grantFamily(family, deviceID) {
  if (!deviceID) return
  await command('客户端授权已保存', async () => {
    await rpc('front.family.grant_device', { family_id: family.id, device_id: deviceID })
    await refreshAll()
    if (state.familyDetailData) await loadFamilyDetail(state.familyDetail)
  })
}

async function revokeFamily(familyID, deviceID) {
  if (!confirm('确认撤销该客户端的访问权限？')) return
  await command('授权已撤销', async () => {
    await rpc('front.family.revoke_device', { family_id: familyID, device_id: deviceID })
    await refreshAll()
    if (state.familyDetailData) await loadFamilyDetail(state.familyDetail)
  })
}

async function unbindHomeServer(family) {
  if (!confirm(`确认解绑家庭 ${family.name} 的家庭服务器？`)) return
  await command('家庭服务器已解绑', async () => {
    await rpc('front.family.unbind_home_server', { family_id: family.id })
    await refreshAll()
  })
}

async function forceOffline(device) {
  if (!confirm('是否强制该设备下线？')) return
  await command('强制下线指令已提交', async () => {
    await rpc('front.device.force_offline', { device_id: device.device_id })
    await refreshAll()
  })
}

async function setBlacklist(device, value) {
  if (value && !confirm('拉黑后设备即使持有授权码也无法连接，是否继续？')) return
  await command(value ? '设备已拉黑' : '设备已解除拉黑', async () => {
    await rpc('front.device.blacklist', { device_id: device.device_id, value })
    await refreshAll()
  })
}

async function updateAuthCode() {
  await command('授权码已更新', async () => {
    await rpc('front.config.update_auth_code', { auth_code: state.newAuthCode })
    await refreshAll()
  })
}

async function updatePassword() {
  if (state.newPassword !== state.confirmPassword) {
    state.error = '两次新密码不一致'
    notify(state.error, 'error')
    return
  }
  await command('管理员密码已更新', async () => {
    await rpc('front.config.update_password', {
      old_password: state.oldPassword,
      new_password: state.newPassword
    })
    state.oldPassword = ''
    state.newPassword = ''
    state.confirmPassword = ''
  })
}

// ── Family Detail ──

async function openFamilyDetail(family) {
  state.familyDetail = family
  state.familyDetailData = null
  await loadFamilyDetail(family)
}

async function loadFamilyDetail(family) {
  try {
    state.familyDetailData = await rpc('front.family.detail', { family_id: family.id })
  } catch (e) {
    notify(e.message, 'error')
  }
}

function closeFamilyDetail() {
  state.familyDetail = null
  state.familyDetailData = null
}

async function familyBlacklist(familyID, deviceID) {
  if (!confirm('将该设备永久禁止加入此家庭，是否继续？')) return
  await command('设备已加入家庭黑名单', async () => {
    await rpc('front.family.blacklist', { family_id: familyID, device_id: deviceID })
    await refreshAll()
    await loadFamilyDetail(state.familyDetail)
  })
}

async function familyUnblacklist(familyID, deviceID) {
  await command('设备已移出家庭黑名单', async () => {
    await rpc('front.family.unblacklist', { family_id: familyID, device_id: deviceID })
    await refreshAll()
    await loadFamilyDetail(state.familyDetail)
  })
}

// ── Device Detail ──

async function openDeviceDetail(device) {
  state.deviceDetail = device
  state.deviceDetailData = null
  await loadDeviceDetail(device)
}

async function loadDeviceDetail(device) {
  try {
    state.deviceDetailData = await rpc('front.device.detail', { device_id: device.device_id })
  } catch (e) {
    notify(e.message, 'error')
  }
}

function closeDeviceDetail() {
  state.deviceDetail = null
  state.deviceDetailData = null
  state.editingNote = false
  state.grantingFamily = false
}

async function saveDeviceNote() {
  await command('备注已保存', async () => {
    await rpc('front.device.set_note', { device_id: state.deviceDetail.device_id, note: state.noteValue })
    state.editingNote = false
    await refreshAll()
    await loadDeviceDetail(state.deviceDetail)
  })
}

function startEditNote() {
  state.noteValue = state.deviceDetailData?.note || ''
  state.editingNote = true
}

async function grantDeviceToFamily() {
  if (!state.selectedFamilyId) return
  await command('客户端已授权加入家庭', async () => {
    await rpc('front.device.grant_family', { family_id: Number(state.selectedFamilyId), device_id: state.deviceDetail.device_id })
    state.grantingFamily = false
    state.selectedFamilyId = ''
    await refreshAll()
    await loadDeviceDetail(state.deviceDetail)
  })
}

// ── Helpers ──

async function command(message, task) {
  state.error = ''
  try {
    await task()
    notify(message)
  } catch (error) {
    state.error = error.message
    notify(error.message, 'error')
  }
}

function notify(message, type = 'success') {
  const id = `${Date.now()}-${Math.random().toString(16).slice(2)}`
  state.toasts.push({ id, message, type })
  setTimeout(() => {
    state.toasts = state.toasts.filter((toast) => toast.id !== id)
  }, 3200)
}

function scheduleRefresh() {
  if (!state.loggedIn) return
  clearTimeout(refreshTimer)
  refreshTimer = setTimeout(() => {
    refreshAll().catch((error) => notify(error.message, 'error'))
  }, 250)
}

function formatBytes(value) {
  if (!value) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let size = value
  let unit = 0
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024
    unit += 1
  }
  return `${size.toFixed(size >= 10 ? 0 : 1)} ${units[unit]}`
}

function formatDuration(seconds) {
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  return `${h}小时 ${m}分钟`
}

function timeAgo(ts) {
  if (!ts) return '从未'
  const sec = Math.floor((Date.now() - new Date(ts).getTime()) / 1000)
  if (sec < 60) return `${sec}秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)}分钟前`
  if (sec < 86400) return `${Math.floor(sec / 3600)}小时前`
  return `${Math.floor(sec / 86400)}天前`
}

// 页面加载时尝试恢复之前的登录会话
tryRestoreSession()

onBeforeUnmount(() => {
  clearTimeout(refreshTimer)
  if (state.ws) state.ws.close()
})
</script>

<template>
  <main class="console-shell">
    <div class="toast-stack">
      <div v-for="toast in state.toasts" :key="toast.id" class="toast" :class="toast.type">
        {{ toast.message }}
      </div>
    </div>

    <section v-if="!state.loggedIn" class="login-panel">
      <button class="theme-toggle floating" type="button" :title="activeTheme === 'dark' ? '切换浅色模式' : '切换深色模式'" @click="toggleTheme">
        <Sun v-if="activeTheme === 'dark'" :size="18" />
        <Moon v-else :size="18" />
      </button>
      <div class="brand-row">
        <Shield :size="30" />
        <div>
          <h1>Go Home 控制台</h1>
          <p>公网服务器管理中心</p>
        </div>
      </div>
      <form class="login-form" @submit.prevent="login">
        <label>
          管理员密码
          <input v-model="state.password" type="password" autocomplete="current-password" />
        </label>
        <button type="submit" :disabled="loading">
          <LogIn :size="18" />
          {{ loading ? '登录中' : '登录' }}
        </button>
        <p v-if="state.error" class="error-text">{{ state.error }}</p>
      </form>
    </section>

    <section v-else class="app-layout">
      <aside class="sidebar">
        <div class="brand-row compact">
          <Shield :size="24" />
          <strong>Go Home</strong>
        </div>
        <nav>
          <button class="theme-toggle" type="button" @click="toggleTheme">
            <Sun v-if="activeTheme === 'dark'" :size="18" />
            <Moon v-else :size="18" />
            {{ activeTheme === 'dark' ? '浅色' : '深色' }}
          </button>
          <button
            v-for="tab in tabs"
            :key="tab.id"
            :class="{ active: state.tab === tab.id }"
            @click="state.tab = tab.id"
          >
            <component :is="tab.icon" :size="18" />
            {{ tab.label }}
          </button>
        </nav>
      </aside>

      <section class="content">
        <header class="content-header">
          <div>
            <p class="eyebrow">服务器</p>
            <h2>{{ tabs.find((item) => item.id === state.tab)?.label }}</h2>
          </div>
          <span class="status-dot" :class="{ online: state.connected }">
            {{ state.connected ? '已连接' : '已断开' }}
          </span>
        </header>

        <!-- Dashboard -->
        <section v-if="state.tab === 'dashboard'" class="metric-grid">
          <article class="metric">
            <Users :size="22" />
            <span>在线设备</span>
            <strong>{{ state.dashboard.online_devices }}</strong>
          </article>
          <article class="metric">
            <Cable :size="22" />
            <span>累计流量</span>
            <strong>{{ formatBytes(state.dashboard.total_bytes) }}</strong>
          </article>
          <article class="metric">
            <Router :size="22" />
            <span>运行时长</span>
            <strong>{{ formatDuration(state.dashboard.uptime_seconds) }}</strong>
          </article>
        </section>

        <!-- Families -->
        <section v-if="state.tab === 'families'" class="stack">
          <form class="toolbar" @submit.prevent="createFamily">
            <input v-model="state.newFamilyName" placeholder="家庭名称" />
            <select v-model="state.newFamilyVisibility">
              <option value="private">私密</option>
              <option value="public">公开</option>
            </select>
            <button type="submit">
              <Plus :size="18" />
              创建
            </button>
          </form>

          <div class="filterbar">
            <label class="search-box">
              <Search :size="18" />
              <input v-model="state.familySearch" placeholder="搜索名称、网段、设备 ID" />
            </label>
            <select v-model="state.familyVisibilityFilter">
              <option value="all">全部可见性</option>
              <option value="public">公开家庭</option>
              <option value="private">私密家庭</option>
            </select>
            <select v-model="state.familyOnlineFilter">
              <option value="all">全部状态</option>
              <option value="online">家庭服务器在线</option>
              <option value="offline">家庭服务器离线</option>
            </select>
          </div>

          <article v-for="family in filteredFamilies" :key="family.id" class="list-row clickable" @click="openFamilyDetail(family)">
            <div class="row-main">
              <strong>{{ family.name }}</strong>
              <span class="tag" :class="family.visibility === 'public' ? 'tag-public' : 'tag-private'">{{ family.visibility === 'public' ? '公开' : '私密' }}</span>
              <span>{{ family.home_server_online ? '在线' : '离线' }}</span>
              <span>LAN {{ family.lan_cidr || '未上报' }}</span>
            </div>
            <div class="row-actions" @click.stop>
              <button class="icon-button" title="设为公开" @click="setVisibility(family, 'public')">
                <Check :size="17" />
              </button>
              <button class="icon-button" title="设为私密" @click="setVisibility(family, 'private')">
                <Lock :size="17" />
              </button>
              <select v-if="!family.home_server_id" @change="bindHomeServer(family, $event.target.value)">
                <option value="">绑定服务器</option>
                <option v-for="device in onlineHomeServers" :key="device.device_id" :value="device.device_id">
                  {{ device.device_id }}
                </option>
              </select>
              <button v-if="family.home_server_id" class="danger" @click="unbindHomeServer(family)">
                <LogOut :size="17" />
                解绑
              </button>
            </div>
          </article>
        </section>

        <!-- Family Detail Modal -->
        <section v-if="state.familyDetail" class="detail-overlay" @click.self="closeFamilyDetail">
          <article class="detail-panel">
            <header class="detail-header">
              <div>
                <p class="eyebrow">家庭详情</p>
                <h2>{{ state.familyDetail.name }}</h2>
              </div>
              <button class="icon-button" @click="closeFamilyDetail">
                <X :size="20" />
              </button>
            </header>

            <div v-if="state.familyDetailData" class="detail-body">
              <div class="metric-grid">
                <article class="metric">
                  <span>可见性</span>
                  <strong>{{ state.familyDetailData.family.visibility === 'public' ? '公开' : '私密' }}</strong>
                </article>
                <article class="metric">
                  <span>家庭服务器</span>
                  <strong>{{ state.familyDetailData.family.home_server_online ? '在线' : '离线' }}</strong>
                </article>
                <article class="metric">
                  <span>局域网网段</span>
                  <strong>{{ state.familyDetailData.family.lan_cidr || '未上报' }}</strong>
                </article>
              </div>

              <div class="metric-grid">
                <article class="metric">
                  <span>上行流量</span>
                  <strong>{{ formatBytes(state.familyDetailData.traffic.up_bytes) }}</strong>
                </article>
                <article class="metric">
                  <span>下行流量</span>
                  <strong>{{ formatBytes(state.familyDetailData.traffic.down_bytes) }}</strong>
                </article>
              </div>

              <!-- Authorized Devices -->
              <div class="sub-section">
                <h3><Users :size="18" /> 已授权设备</h3>
                <div class="toolbar compact" v-if="state.familyDetail.visibility === 'private'">
                  <select @change="grantFamily(state.familyDetail, $event.target.value)">
                    <option value="">添加客户端</option>
                    <option v-for="device in clientDevices" :key="device.device_id" :value="device.device_id">
                      {{ device.note || device.device_id }}
                    </option>
                  </select>
                </div>
                <article v-for="dev in state.familyDetailData.devices" :key="dev.device_id" class="list-row compact">
                  <div class="row-main">
                    <Monitor v-if="dev.device_type === 'home-server'" :size="16" />
                    <Smartphone v-else :size="16" />
                    <strong>{{ dev.note || dev.device_id.substring(0, 16) + '…' }}</strong>
                    <span class="tag" :class="dev.device_type === 'home-server' ? 'tag-public' : 'tag-private'">
                      {{ dev.device_type === 'home-server' ? '家庭服务器' : '客户端' }}
                    </span>
                    <span :class="dev.online ? 'text-success' : 'text-muted'">{{ dev.online ? '在线' : '离线' }}</span>
                    <span v-if="dev.latency_ms">延迟 {{ dev.latency_ms }} ms</span>
                  </div>
                  <div class="row-actions">
                    <button v-if="dev.device_type === 'client'" class="icon-button" title="撤销授权" @click="revokeFamily(state.familyDetail.id, dev.device_id)">
                      <Trash2 :size="16" />
                    </button>
                    <button class="icon-button" title="查看详情" @click="closeFamilyDetail(); openDeviceDetail(dev)">
                      <ArrowLeft :size="16" />
                    </button>
                  </div>
                </article>
                <p v-if="!state.familyDetailData.devices.length" class="empty-text">暂无已授权设备</p>
              </div>

              <!-- Blacklisted Devices -->
              <div class="sub-section" v-if="state.familyDetailData.blacklisted_devices.length">
                <h3><Ban :size="18" /> 家庭黑名单</h3>
                <article v-for="deviceId in state.familyDetailData.blacklisted_devices" :key="deviceId" class="list-row compact">
                  <div class="row-main">
                    <Ban :size="16" class="text-danger" />
                    <strong>{{ deviceId.substring(0, 24) }}…</strong>
                    <span class="text-muted">永久禁止加入</span>
                  </div>
                  <div class="row-actions">
                    <button @click="familyUnblacklist(state.familyDetail.id, deviceId)">
                      <Check :size="16" />
                      移出黑名单
                    </button>
                  </div>
                </article>
              </div>

              <!-- Add to Blacklist -->
              <div class="sub-section">
                <h3><Ban :size="18" /> 加入黑名单</h3>
                <div class="toolbar compact">
                  <select @change="familyBlacklist(state.familyDetail.id, $event.target.value)">
                    <option value="">选择设备加入黑名单</option>
                    <option v-for="device in clientDevices" :key="device.device_id" :value="device.device_id">
                      {{ device.note || device.device_id }}
                    </option>
                  </select>
                </div>
              </div>
            </div>
            <div v-else class="detail-body">
              <p>加载中...</p>
            </div>
          </article>
        </section>

        <!-- Devices -->
        <section v-if="state.tab === 'devices'" class="stack">
          <div class="filterbar">
            <label class="search-box">
              <Search :size="18" />
              <input v-model="state.deviceSearch" placeholder="搜索设备 ID、类型、备注" />
            </label>
          </div>

          <article v-for="device in filteredDevices" :key="device.device_id" class="list-row clickable" @click="openDeviceDetail(device)">
            <div class="row-main">
              <Monitor v-if="device.device_type === 'home-server'" :size="16" />
              <Smartphone v-else :size="16" />
              <strong>{{ device.note || device.device_id }}</strong>
              <span class="tag" :class="device.device_type === 'home-server' ? 'tag-public' : 'tag-private'">
                {{ device.device_type === 'home-server' ? '家庭服务器' : '客户端' }}
              </span>
              <span :class="device.online ? 'text-success' : 'text-muted'">{{ device.online ? '在线' : '离线' }}</span>
              <span v-if="device.latency_ms">延迟 {{ device.latency_ms }} ms</span>
              <span v-if="device.is_blacklisted" class="text-danger">已拉黑</span>
            </div>
            <div class="row-actions" @click.stop>
              <button @click="forceOffline(device)">
                <LogOut :size="17" />
                下线
              </button>
              <button v-if="!device.is_blacklisted" class="danger" @click="setBlacklist(device, true)">
                <Ban :size="17" />
                拉黑
              </button>
              <button v-else @click="setBlacklist(device, false)">
                <Check :size="17" />
                解除
              </button>
            </div>
          </article>
        </section>

        <!-- Device Detail Modal -->
        <section v-if="state.deviceDetail" class="detail-overlay" @click.self="closeDeviceDetail">
          <article class="detail-panel">
            <header class="detail-header">
              <div>
                <p class="eyebrow">设备详情</p>
                <h2>{{ state.deviceDetailData?.note || state.deviceDetail.device_id }}</h2>
              </div>
              <button class="icon-button" @click="closeDeviceDetail">
                <X :size="20" />
              </button>
            </header>

            <div v-if="state.deviceDetailData" class="detail-body">
              <div class="metric-grid">
                <article class="metric">
                  <span>设备类型</span>
                  <strong>{{ state.deviceDetailData.device.device_type === 'home-server' ? '家庭服务器' : '客户端' }}</strong>
                </article>
                <article class="metric">
                  <span>在线状态</span>
                  <strong :class="state.deviceDetailData.device.online ? 'text-success' : 'text-muted'">
                    {{ state.deviceDetailData.device.online ? '在线' : '离线' }}
                  </strong>
                </article>
                <article class="metric">
                  <span>延迟</span>
                  <strong>{{ state.deviceDetailData.device.latency_ms || 0 }} ms</strong>
                </article>
                <article class="metric">
                  <span>最后在线</span>
                  <strong>{{ timeAgo(state.deviceDetailData.device.last_online) }}</strong>
                </article>
              </div>

              <div class="metric-grid">
                <article class="metric">
                  <span>上行流量</span>
                  <strong>{{ formatBytes(state.deviceDetailData.traffic.up_bytes) }}</strong>
                </article>
                <article class="metric">
                  <span>下行流量</span>
                  <strong>{{ formatBytes(state.deviceDetailData.traffic.down_bytes) }}</strong>
                </article>
                <article class="metric">
                  <span>局域网网段</span>
                  <strong>{{ state.deviceDetailData.device.lan_cidr || '-' }}</strong>
                </article>
                <article class="metric">
                  <span>UDP 端口</span>
                  <strong>{{ state.deviceDetailData.device.udp_port || '-' }}</strong>
                </article>
              </div>

              <!-- Device Note -->
              <div class="sub-section">
                <h3>备注</h3>
                <div v-if="!state.editingNote" class="note-row">
                  <span>{{ state.deviceDetailData.note || '暂无备注' }}</span>
                  <button @click="startEditNote">编辑备注</button>
                </div>
                <div v-else class="toolbar compact">
                  <input v-model="state.noteValue" placeholder="输入设备备注" />
                  <button @click="saveDeviceNote">
                    <Check :size="16" />
                    保存
                  </button>
                  <button class="ghost-button" @click="state.editingNote = false">取消</button>
                </div>
              </div>

              <!-- Authorized Families -->
              <div class="sub-section">
                <h3><Home :size="18" /> 已加入的家庭</h3>
                <article v-for="family in state.deviceDetailData.families" :key="family.id" class="list-row compact">
                  <div class="row-main">
                    <Home :size="16" />
                    <strong>{{ family.name }}</strong>
                    <span class="tag" :class="family.visibility === 'public' ? 'tag-public' : 'tag-private'">
                      {{ family.visibility === 'public' ? '公开' : '私密' }}
                    </span>
                  </div>
                </article>
                <p v-if="!state.deviceDetailData.families.length" class="empty-text">暂未加入任何家庭</p>

                <!-- Grant to Private Family -->
                <div v-if="state.deviceDetailData.device.device_type === 'client'" class="toolbar compact" style="margin-top: 10px;">
                  <template v-if="!state.grantingFamily">
                    <button @click="state.grantingFamily = true">
                      <Plus :size="16" />
                      加入私密家庭
                    </button>
                  </template>
                  <template v-else>
                    <select v-model="state.selectedFamilyId">
                      <option value="">选择私密家庭</option>
                      <option v-for="family in privateFamilies" :key="family.id" :value="family.id">
                        {{ family.name }} (ID: {{ family.id }})
                      </option>
                    </select>
                    <button @click="grantDeviceToFamily" :disabled="!state.selectedFamilyId">
                      <Check :size="16" />
                      确认
                    </button>
                    <button class="ghost-button" @click="state.grantingFamily = false">取消</button>
                  </template>
                </div>
              </div>

              <!-- Blacklist Status -->
              <div class="sub-section">
                <h3>黑名单状态</h3>
                <div class="note-row">
                  <span :class="state.deviceDetailData.device.is_blacklisted ? 'text-danger' : 'text-success'">
                    {{ state.deviceDetailData.device.is_blacklisted ? '已拉黑 - 设备无法连接服务器' : '正常 - 设备可正常连接' }}
                  </span>
                  <button v-if="!state.deviceDetailData.device.is_blacklisted" class="danger" @click="setBlacklist(state.deviceDetailData.device, true); closeDeviceDetail()">
                    <Ban :size="16" />
                    拉黑
                  </button>
                  <button v-else @click="setBlacklist(state.deviceDetailData.device, false); closeDeviceDetail()">
                    <Check :size="16" />
                    解除拉黑
                  </button>
                </div>
              </div>
            </div>
            <div v-else class="detail-body">
              <p>加载中...</p>
            </div>
          </article>
        </section>

        <!-- Logs -->
        <section v-if="state.tab === 'logs'" class="stack">
          <article v-for="entry in state.logs" :key="entry.id" class="log-row">
            <span :class="['log-level', entry.level]">{{ entry.level }}</span>
            <strong>{{ entry.source }}</strong>
            <p>{{ entry.message }}</p>
            <time>{{ new Date(entry.created_at).toLocaleString() }}</time>
          </article>
        </section>

        <!-- Settings -->
        <section v-if="state.tab === 'settings'" class="settings-grid">
          <form class="settings-panel" @submit.prevent="updatePassword">
            <h3>管理员密码</h3>
            <input v-model="state.oldPassword" type="password" placeholder="旧密码" />
            <input v-model="state.newPassword" type="password" placeholder="新密码" />
            <input v-model="state.confirmPassword" type="password" placeholder="确认新密码" />
            <button type="submit">
              <KeyRound :size="17" />
              提交
            </button>
          </form>
          <form class="settings-panel" @submit.prevent="updateAuthCode">
            <h3>授权码</h3>
            <input v-model="state.newAuthCode" />
            <button type="submit">
              <Check :size="17" />
              更新授权码
            </button>
          </form>
        </section>
      </section>
    </section>
  </main>
</template>
