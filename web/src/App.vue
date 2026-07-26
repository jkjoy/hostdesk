<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, provide, reactive, ref, watch, type Component } from "vue";
import { RouterView, useRoute, useRouter } from "vue-router";
import {
  Activity,
  ArrowDownToLine,
  ArrowUpFromLine,
  BadgeCheck,
  Box,
  Boxes,
  ChevronRight,
  CircleAlert,
  Cloud,
  Container,
  Database,
  Download,
  ExternalLink,
  FileCode2,
  Folder,
  FolderSync,
  Gauge,
  Globe2,
  HardDrive,
  KeyRound,
  LogIn,
  LogOut,
  MemoryStick,
  Menu,
  Network,
  PackageMinus,
  PanelsTopLeft,
  Play,
  RefreshCw,
  RotateCw,
  Save,
  Server,
  Settings,
  ShieldCheck,
  SquareTerminal,
  UserRoundCheck,
  X,
} from "@lucide/vue";
import { api, ApiError, setCSRF } from "./api";
import type { Overview, ServiceStatus, Session, UpdateStatus } from "./types";
import { uiKey } from "./ui";
import type { ViewID } from "./router";

type Screen = "boot" | "login" | "setup" | "app";
type ToastKind = "success" | "error";

const screen = ref<Screen>("boot");
const session = ref<Session | null>(null);
const overview = ref<Overview | null>(null);
const update = ref<UpdateStatus | null>(null);
const loadingOverview = ref(false);
const updating = ref(false);
const activeAction = ref("");
const mobileNavOpen = ref(false);
const authBusy = ref(false);
const authError = ref("");
const login = reactive({ username: "admin", password: "" });
const setup = reactive({ username: "admin", password: "", confirm: "" });
const toasts = ref<Array<{ id: number; kind: ToastKind; message: string }>>([]);
const confirmation = reactive({ open: false, title: "", message: "", confirmText: "确认", danger: false, resolve: null as null | ((value: boolean) => void) });
const route = useRoute();
const router = useRouter();
const currentView = computed<ViewID>(() => typeof route.meta.view === "string" ? route.meta.view as ViewID : "overview");
const fileInitialPath = computed(() => typeof route.query.path === "string" ? route.query.path : "");
let toastID = 0;
let refreshTimer: number | undefined;

const navGroups = [
  {
    label: "工作台",
    items: [
      { id: "overview", label: "概览", icon: Gauge },
      { id: "files", label: "文件", icon: Folder },
    ],
  },
  {
    label: "站点服务",
    items: [
      { id: "sites", label: "网站", icon: Globe2 },
      { id: "certificates", label: "证书", icon: ShieldCheck },
      { id: "nginx", label: "Nginx", icon: Network },
      { id: "php", label: "PHP", icon: FileCode2 },
      { id: "databases", label: "数据库", icon: Database },
      { id: "ftp", label: "FTP", icon: FolderSync },
    ],
  },
  {
    label: "系统",
    items: [
      { id: "containers", label: "容器", icon: Container },
      { id: "terminal", label: "终端", icon: SquareTerminal },
      { id: "account", label: "账号安全", icon: KeyRound },
      { id: "server-settings", label: "服务器设置", icon: Settings },
    ],
  },
];

const serviceNames: Record<string, string> = { nginx: "Nginx", php: "PHP-FPM", mysql: "MariaDB", ftp: "vsftpd" };
const serviceIcons: Record<string, Component> = { nginx: Network, php: FileCode2, mysql: Database, ftp: FolderSync };
const system = computed(() => overview.value?.system);
const viewLabels = computed(() => Object.fromEntries(navGroups.flatMap(group => group.items.map(item => [item.id, item.label]))));
function notify(message: string, kind: ToastKind = "success") {
  const id = ++toastID;
  toasts.value.push({ id, kind, message });
  window.setTimeout(() => { toasts.value = toasts.value.filter((item) => item.id !== id); }, 3600);
}

function ask(title: string, message: string, confirmText = "确认", danger = false) {
  confirmation.open = true;
  confirmation.title = title;
  confirmation.message = message;
  confirmation.confirmText = confirmText;
  confirmation.danger = danger;
  return new Promise<boolean>((resolve) => { confirmation.resolve = resolve; });
}
provide(uiKey, { notify, confirm: ask });

function selectView(id: string) {
  void router.push({ name: id });
  mobileNavOpen.value = false;
}

function openSiteFiles(absolutePath: string) {
  const root = session.value?.fileRoot === "/" ? "/" : session.value?.fileRoot.replace(/\/+$/, "") || "/";
  const normalized = absolutePath.replace(/\/+/g, "/").replace(/\/$/, "") || "/";
  const initialPath = root === "/" ? normalized.replace(/^\//, "") : normalized.startsWith(`${root}/`) ? normalized.slice(root.length + 1) : "";
  void router.push({ name: "files", query: initialPath ? { path: initialPath } : {} });
}

function filePathChanged(path: string) {
  const query = path ? { path } : {};
  if (fileInitialPath.value !== path) void router.replace({ name: "files", query });
}

function accountChanged(username: string) {
  if (session.value) session.value.user = username;
}

function closeConfirmation(value: boolean) {
  confirmation.open = false;
  confirmation.resolve?.(value);
  confirmation.resolve = null;
}

function formatBytes(bytes = 0) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) { value /= 1024; index += 1; }
  return `${value >= 10 || index === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

function formatUptime(seconds = 0) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days) return `${days} 天 ${hours} 小时`;
  if (hours) return `${hours} 小时 ${minutes} 分钟`;
  return `${minutes} 分钟`;
}

function percent(used = 0, total = 0) {
  return total > 0 ? Math.min(100, Math.max(0, used / total * 100)) : 0;
}

function serviceLabel(service: ServiceStatus) {
  return serviceNames[service.name] || service.name;
}

function serviceIcon(service: ServiceStatus) {
  return serviceIcons[service.name] || Box;
}

function serviceStatusLabel(service: ServiceStatus) {
  if (!service.installed) return "未安装";
  return service.running ? "运行中" : "已停止";
}

function enterApp(value: Session) {
  session.value = value;
  setCSRF(value.csrf);
  login.username = value.user;
  login.password = "";
  screen.value = "app";
  if (currentView.value === "overview") void loadOverview();
}

async function boot() {
  try {
    const setupState = await api<{ required: boolean }>("/api/setup");
    if (setupState.required) {
      screen.value = "setup";
      return;
    }
    try {
      enterApp(await api<Session>("/api/session"));
    } catch {
      screen.value = "login";
    }
  } catch (error) {
    authError.value = error instanceof Error ? error.message : "无法连接服务器";
    screen.value = "login";
  }
}

async function submitLogin() {
  authBusy.value = true;
  authError.value = "";
  try {
    const value = await api<Session>("/api/login", { method: "POST", body: { username: login.username.trim(), password: login.password } });
    enterApp(value);
  } catch (error) {
    authError.value = error instanceof Error ? error.message : "登录失败";
  } finally {
    authBusy.value = false;
  }
}

async function submitSetup() {
  authError.value = "";
  if (setup.password !== setup.confirm) {
    authError.value = "两次输入的密码不一致";
    return;
  }
  authBusy.value = true;
  try {
    const value = await api<Session>("/api/setup", { method: "POST", body: { username: setup.username.trim(), password: setup.password } });
    setup.password = "";
    setup.confirm = "";
    enterApp(value);
  } catch (error) {
    authError.value = error instanceof Error ? error.message : "初始化失败";
  } finally {
    authBusy.value = false;
  }
}

async function logout() {
  try { await api("/api/logout", { method: "POST" }); } catch { /* Session is cleared locally either way. */ }
  session.value = null;
  overview.value = null;
  setCSRF("");
  screen.value = "login";
}

function handleAPIError(error: unknown) {
  if (error instanceof ApiError && error.status === 401) {
    screen.value = "login";
    session.value = null;
    setCSRF("");
  }
  notify(error instanceof Error ? error.message : "操作失败", "error");
}

async function loadOverview(forceUpdate = false) {
  if (loadingOverview.value) return;
  loadingOverview.value = true;
  try {
    const [overviewData, updateData] = await Promise.all([
      api<Overview>("/api/admin/overview"),
      api<UpdateStatus>(`/api/admin/update${forceUpdate ? "?refresh=1" : ""}`),
    ]);
    overview.value = overviewData;
    update.value = updateData;
  } catch (error) {
    handleAPIError(error);
  } finally {
    loadingOverview.value = false;
  }
}

async function installUpdate() {
  if (!update.value?.updateAvailable || !update.value.latestVersion) return;
  const targetVersion = update.value.latestVersion;
  const approved = await ask(`更新到 ${targetVersion}`, "HostDesk 将下载并校验新版本，更新完成后自动重启服务。", "立即更新");
  if (!approved) return;
  updating.value = true;
  try {
    await api<{ version: string; restarting: boolean }>("/api/admin/update", { method: "POST" });
    notify(`${targetVersion} 已安装，服务正在重启`);
    window.setTimeout(() => window.location.reload(), 4500);
  } catch (error) {
    updating.value = false;
    handleAPIError(error);
  }
}

async function runServiceAction(service: ServiceStatus, action: "install" | "start" | "restart" | "remove") {
  if (action === "remove") {
    const approved = await ask(`卸载 ${serviceLabel(service)}`, "程序包将被移除，网站、数据库及用户数据目录会保留。", "确认卸载", true);
    if (!approved) return;
  }
  const key = `${service.name}:${action}`;
  activeAction.value = key;
  try {
    if (action === "install" || action === "remove") {
      await api(`/api/admin/components/${service.name}/${action}`, { method: "POST" });
    } else {
      await api(`/api/admin/services/${service.service}/${action}`, { method: "POST" });
    }
    notify(`${serviceLabel(service)} 状态已更新`);
    await loadOverview();
  } catch (error) {
    handleAPIError(error);
  } finally {
    activeAction.value = "";
  }
}

onMounted(() => {
  void boot();
  refreshTimer = window.setInterval(() => {
    if (screen.value === "app" && document.visibilityState === "visible") void loadOverview();
  }, 30000);
});

watch(currentView, (view) => {
  mobileNavOpen.value = false;
  if (view === "overview" && screen.value === "app") void loadOverview();
});

onBeforeUnmount(() => {
  if (refreshTimer) window.clearInterval(refreshTimer);
});
</script>

<template>
  <div v-if="screen === 'boot'" class="boot-screen" aria-label="正在加载">
    <div class="brand-symbol"><Server :size="22" /></div>
    <span class="boot-pulse"></span>
  </div>

  <main v-else-if="screen === 'login' || screen === 'setup'" class="auth-screen">
    <section class="auth-shell">
      <header class="auth-brand">
        <div class="brand-symbol"><Server :size="21" /></div>
        <div><strong>HostDesk</strong><span>主机工作台</span></div>
      </header>
      <form v-if="screen === 'login'" class="auth-form" @submit.prevent="submitLogin">
        <div class="auth-heading">
          <span>安全登录</span>
          <h1>管理这台服务器</h1>
        </div>
        <label>管理员账号<input v-model="login.username" autocomplete="username" required></label>
        <label>密码<input v-model="login.password" type="password" autocomplete="current-password" required autofocus></label>
        <p v-if="authError" class="form-error"><CircleAlert :size="15" />{{ authError }}</p>
        <button class="button primary auth-submit" type="submit" :disabled="authBusy">
          <RefreshCw v-if="authBusy" class="spin" :size="17" />
          <LogIn v-else :size="17" />
          <span>登录</span>
        </button>
      </form>
      <form v-else class="auth-form" @submit.prevent="submitSetup">
        <div class="auth-heading">
          <span>首次初始化</span>
          <h1>创建管理员账号</h1>
        </div>
        <label>管理员账号<input v-model="setup.username" minlength="3" maxlength="32" pattern="[A-Za-z0-9_.-]{3,32}" autocomplete="username" required></label>
        <label>管理员密码<input v-model="setup.password" type="password" minlength="12" autocomplete="new-password" required autofocus></label>
        <label>确认密码<input v-model="setup.confirm" type="password" minlength="12" autocomplete="new-password" required></label>
        <p v-if="authError" class="form-error"><CircleAlert :size="15" />{{ authError }}</p>
        <button class="button primary auth-submit" type="submit" :disabled="authBusy">
          <RefreshCw v-if="authBusy" class="spin" :size="17" />
          <UserRoundCheck v-else :size="17" />
          <span>创建并进入</span>
        </button>
      </form>
    </section>
  </main>

  <div v-else class="app-layout">
    <div v-if="mobileNavOpen" class="nav-scrim" @click="mobileNavOpen = false"></div>
    <aside class="app-sidebar" :class="{ open: mobileNavOpen }">
      <div class="sidebar-brand">
        <div class="brand-symbol small"><Server :size="18" /></div>
        <div><strong>HostDesk</strong><span>主机工作台</span></div>
        <button class="icon-button close-nav" type="button" title="关闭导航" @click="mobileNavOpen = false"><X :size="19" /></button>
      </div>
      <nav class="main-nav" aria-label="主导航">
        <section v-for="group in navGroups" :key="group.label" class="nav-group">
          <h2>{{ group.label }}</h2>
          <button v-for="item in group.items" :key="item.id" class="nav-item" :class="{ active: currentView === item.id }" type="button" @click="selectView(item.id)">
            <component :is="item.icon" :size="17" /><span>{{ item.label }}</span><ChevronRight v-if="currentView === item.id" :size="15" />
          </button>
        </section>
      </nav>
      <div class="sidebar-account">
        <span class="presence-dot"></span>
        <div><strong>{{ session?.user }}</strong><span>管理员</span></div>
        <button class="icon-button dark" type="button" title="退出登录" @click="logout"><LogOut :size="17" /></button>
      </div>
    </aside>

    <main class="app-main">
      <header class="topbar">
        <button class="icon-button mobile-menu" type="button" title="打开导航" @click="mobileNavOpen = true"><Menu :size="20" /></button>
        <div class="breadcrumbs"><span>服务器</span><ChevronRight :size="14" /><strong>{{ viewLabels[currentView] || '概览' }}</strong></div>
        <div class="topbar-actions">
          <span class="platform-label">{{ overview?.platform || 'Alpine Linux / OpenRC' }}</span>
          <button v-if="currentView === 'overview'" class="icon-button" type="button" title="刷新概览" :disabled="loadingOverview" @click="loadOverview(true)"><RefreshCw :class="{ spin: loadingOverview }" :size="18" /></button>
        </div>
      </header>

      <div class="page-content">
        <RouterView v-if="currentView !== 'overview'" v-slot="{ Component, route: activeRoute }">
          <component :is="Component" :key="activeRoute.name" :initial-path="fileInitialPath" @path-changed="filePathChanged" @open-files="openSiteFiles" @account-changed="accountChanged" />
        </RouterView>
        <template v-else>
        <header class="page-heading">
          <div><span class="page-kicker">SERVER OVERVIEW</span><h1>服务器概览</h1></div>
          <span class="last-updated">{{ update?.checkedAt ? `更新于 ${new Date(update.checkedAt).toLocaleTimeString('zh-CN', { hour12: false })}` : '正在读取状态' }}</span>
        </header>

        <template v-if="overview && system">
          <section class="server-band">
            <div class="server-identity">
              <div class="server-icon"><Server :size="24" /></div>
              <div><span>当前主机</span><strong>{{ system.hostname || '未命名服务器' }}</strong></div>
            </div>
            <dl class="server-facts">
              <div><dt>公网 IP</dt><dd>{{ system.publicIpAddress || '暂未获取' }}</dd></div>
              <div><dt>内核</dt><dd :title="system.kernel">{{ system.kernel || '-' }}</dd></div>
              <div><dt>运行时间</dt><dd>{{ formatUptime(system.uptimeSeconds) }}</dd></div>
            </dl>
          </section>

          <section class="section-block">
            <div class="section-title"><div><h2>资源使用</h2><span>主机当前负载与容量</span></div><Activity :size="18" /></div>
            <div class="metric-grid">
              <article class="metric-card">
                <div class="metric-label"><Activity :size="16" /><span>CPU</span><strong>{{ system.cpu.usagePercent.toFixed(1) }}%</strong></div>
                <div class="metric-bar"><span :style="{ width: `${system.cpu.usagePercent}%` }"></span></div>
                <div class="metric-detail"><span>{{ system.cpu.cores }} 核</span><span>负载 {{ system.cpu.loadAverage?.[0]?.toFixed(2) || '0.00' }}</span></div>
              </article>
              <article class="metric-card">
                <div class="metric-label"><MemoryStick :size="16" /><span>内存</span><strong>{{ percent(system.memory.used, system.memory.total).toFixed(1) }}%</strong></div>
                <div class="metric-bar"><span :style="{ width: `${percent(system.memory.used, system.memory.total)}%` }"></span></div>
                <div class="metric-detail"><span>{{ formatBytes(system.memory.used) }}</span><span>{{ formatBytes(system.memory.total) }}</span></div>
              </article>
              <article class="metric-card">
                <div class="metric-label"><HardDrive :size="16" /><span>磁盘</span><strong>{{ percent(system.disk.used, system.disk.total).toFixed(1) }}%</strong></div>
                <div class="metric-bar disk"><span :style="{ width: `${percent(system.disk.used, system.disk.total)}%` }"></span></div>
                <div class="metric-detail"><span>{{ formatBytes(system.disk.used) }}</span><span>{{ formatBytes(system.disk.total) }}</span></div>
              </article>
              <article class="metric-card network-metric">
                <div class="metric-label"><Network :size="16" /><span>网络流量</span></div>
                <div class="network-values">
                  <div><ArrowDownToLine :size="15" /><span>接收</span><strong>{{ formatBytes(system.network.receivedBytes) }}</strong></div>
                  <div><ArrowUpFromLine :size="15" /><span>发送</span><strong>{{ formatBytes(system.network.transmittedBytes) }}</strong></div>
                </div>
              </article>
            </div>
          </section>

          <section v-if="update" class="update-band" :class="{ available: update.updateAvailable, error: update.error }">
            <div class="update-icon"><CircleAlert v-if="update.error" :size="20" /><Cloud v-else-if="update.updateAvailable" :size="20" /><BadgeCheck v-else :size="20" /></div>
            <div class="update-copy">
              <strong>{{ update.error ? '暂时无法检查更新' : update.updateAvailable ? `发现新版本 ${update.latestVersion}` : 'HostDesk 已是最新版本' }}</strong>
              <span>{{ update.error || `当前 ${update.currentVersion === 'dev' ? '开发构建' : update.currentVersion}${update.latestVersion ? ` · 最新 ${update.latestVersion}` : ''}` }}</span>
            </div>
            <div class="update-actions">
              <button class="button quiet" type="button" :disabled="loadingOverview || updating" @click="loadOverview(true)"><RefreshCw :size="16" />检查更新</button>
              <a v-if="update.releaseUrl" class="button" :href="update.releaseUrl" target="_blank" rel="noopener"><ExternalLink :size="16" />查看版本</a>
              <button v-if="update.updateAvailable" class="button primary" type="button" :disabled="updating" @click="installUpdate"><RefreshCw v-if="updating" class="spin" :size="16" /><Download v-else :size="16" />{{ updating ? '正在更新' : '立即更新' }}</button>
            </div>
          </section>

          <section class="section-block services-section">
            <div class="section-title"><div><h2>运行环境</h2><span>服务组件与 OpenRC 状态</span></div><Boxes :size="18" /></div>
            <div class="service-list">
              <article v-for="service in overview.services" :key="service.name" class="service-item">
                <div class="service-main">
                  <div class="service-symbol"><component :is="serviceIcon(service)" :size="19" /></div>
                  <div class="service-copy">
                    <div class="service-name"><strong>{{ serviceLabel(service) }}</strong><span class="status-pill" :class="{ running: service.running, stopped: service.installed && !service.running }">{{ serviceStatusLabel(service) }}</span></div>
                    <span class="service-version" :title="service.version || undefined">{{ service.version || (service.installed ? '已安装' : '尚未安装') }}</span>
                  </div>
                </div>
                <div class="service-meta"><span>开机启动</span><strong>{{ service.enabled ? '已启用' : '未启用' }}</strong></div>
                <div class="service-buttons">
                  <button v-if="!service.installed" class="button primary" type="button" :disabled="!!activeAction" @click="runServiceAction(service, 'install')"><Download :size="16" />安装</button>
                  <template v-else>
                    <button class="button" type="button" :disabled="!!activeAction" @click="runServiceAction(service, service.running ? 'restart' : 'start')">
                      <RotateCw v-if="service.running" :class="{ spin: activeAction === `${service.name}:restart` }" :size="16" />
                      <Play v-else :size="16" />{{ service.running ? '重启' : '启动' }}
                    </button>
                    <button class="icon-button danger" type="button" title="卸载组件" :disabled="!!activeAction" @click="runServiceAction(service, 'remove')"><PackageMinus :size="17" /></button>
                  </template>
                </div>
              </article>
            </div>
          </section>
        </template>

        <div v-else class="overview-skeleton" aria-label="正在加载服务器概览">
          <div class="skeleton server"></div><div class="skeleton-grid"><div v-for="index in 4" :key="index" class="skeleton metric"></div></div><div class="skeleton services"></div>
        </div>
        </template>
      </div>
    </main>
  </div>

  <Teleport to="body">
    <div v-if="confirmation.open" class="modal-layer" @mousedown.self="closeConfirmation(false)">
      <section class="confirm-dialog" role="alertdialog" aria-modal="true" :aria-label="confirmation.title">
        <header><div class="dialog-icon" :class="{ danger: confirmation.danger }"><CircleAlert :size="20" /></div><div><h2>{{ confirmation.title }}</h2><p>{{ confirmation.message }}</p></div></header>
        <footer><button class="button quiet" type="button" @click="closeConfirmation(false)">取消</button><button class="button" :class="confirmation.danger ? 'danger-solid' : 'primary'" type="button" @click="closeConfirmation(true)">{{ confirmation.confirmText }}</button></footer>
      </section>
    </div>
    <div class="toast-stack" aria-live="polite">
      <div v-for="toast in toasts" :key="toast.id" class="toast" :class="toast.kind"><BadgeCheck v-if="toast.kind === 'success'" :size="17" /><CircleAlert v-else :size="17" /><span>{{ toast.message }}</span></div>
    </div>
  </Teleport>
</template>
