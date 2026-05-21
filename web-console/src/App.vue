<script setup>
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import {
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
  Plus,
  Search,
  Router,
  Settings,
  Shield,
  Users
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
  toasts: []
})

const loading = ref(false)
let refreshTimer = null

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
      state.loggedIn = false
    }
    ws.onmessage = (event) => handleMessage(JSON.parse(event.data))
  })
}

function handleMessage(message) {
  if (message.action === 'front.session.revoked') {
    notify('账号在别处登录', 'error')
    state.loggedIn = false
    state.token = ''
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
    task.reject(new Error(message.error.message || message.error.code))
  } else {
    task.resolve(message.result)
  }
}

async function rpc(action, params = {}) {
  await connect()
  const id = `${Date.now()}-${Math.random().toString(16).slice(2)}`
  state.ws.send(JSON.stringify({ jsonrpc: '2.0', id, action, params }))
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

          <article v-for="family in filteredFamilies" :key="family.id" class="list-row">
            <div class="row-main">
              <strong>{{ family.name }}</strong>
              <span>{{ family.visibility === 'public' ? '公开家庭' : '私密家庭' }}</span>
              <span>{{ family.home_server_online ? '家庭服务器在线' : '家庭服务器离线' }}</span>
              <span>LAN {{ family.lan_cidr || '未上报' }}</span>
            </div>
            <div class="row-actions">
              <button class="icon-button" title="设为公开" @click="setVisibility(family, 'public')">
                <Check :size="17" />
              </button>
              <button class="icon-button" title="设为私密" @click="setVisibility(family, 'private')">
                <Lock :size="17" />
              </button>
              <select v-if="!family.home_server_id" @change="bindHomeServer(family, $event.target.value)">
                <option value="">绑定家庭服务器</option>
                <option v-for="device in onlineHomeServers" :key="device.device_id" :value="device.device_id">
                  {{ device.device_id }}
                </option>
              </select>
              <select v-if="family.visibility === 'private'" @change="grantFamily(family, $event.target.value)">
                <option value="">授权客户端</option>
                <option v-for="device in clientDevices" :key="device.device_id" :value="device.device_id">
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

        <section v-if="state.tab === 'devices'" class="stack">
          <article v-for="device in state.devices" :key="device.device_id" class="list-row">
            <div class="row-main">
              <strong>{{ device.device_id }}</strong>
              <span>{{ device.device_type }}</span>
              <span>{{ device.online ? '在线' : '离线' }}</span>
              <span v-if="device.latency_ms">延迟 {{ device.latency_ms }} ms</span>
              <span v-if="device.is_blacklisted">已拉黑</span>
            </div>
            <div class="row-actions">
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

        <section v-if="state.tab === 'logs'" class="stack">
          <article v-for="entry in state.logs" :key="entry.id" class="log-row">
            <span :class="['log-level', entry.level]">{{ entry.level }}</span>
            <strong>{{ entry.source }}</strong>
            <p>{{ entry.message }}</p>
            <time>{{ new Date(entry.created_at).toLocaleString() }}</time>
          </article>
        </section>

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
