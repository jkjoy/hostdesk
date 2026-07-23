const $ = (selector) => document.querySelector(selector);
const state = { csrf: "", user: "", root: "", path: "", entries: [], selected: new Set(), editorPath: "", recent: [], adminTab: "overview", overview: null, sites: [], certificates: [], dnsSettings: {}, databases: [], ftpUsers: [], containers: [] };
let terminal;
let fitAddon;
let sshSocket;
let sshPrivateKey = "";
let terminalFitFrame = 0;
let editingSite = null;
let editingContainer = null;
let editingFTPUser = null;

function refreshIcons(root = document) {
  window.lucide?.createIcons({ root, attrs: { "stroke-width": 1.8 } });
}

function toast(message, type = "success") {
  const item = document.createElement("div");
  item.className = `toast ${type}`;
  const icon = document.createElement("i");
  icon.dataset.lucide = type === "error" ? "circle-alert" : "circle-check";
  const text = document.createElement("span");
  text.textContent = message;
  item.append(icon, text);
  $("#toasts").append(item);
  refreshIcons(item);
  setTimeout(() => item.remove(), 3600);
}

async function api(url, options = {}) {
  const init = { ...options, headers: { ...(options.headers || {}) } };
  if (init.body && !(init.body instanceof Blob) && typeof init.body !== "string") {
    init.headers["content-type"] = "application/json";
    init.body = JSON.stringify(init.body);
  }
  if (init.method && !["GET", "HEAD"].includes(init.method)) init.headers["x-csrf-token"] = state.csrf;
  const response = await fetch(url, init);
  const type = response.headers.get("content-type") || "";
  const data = type.includes("application/json") ? await response.json() : await response.text();
  if (!response.ok) {
    if (response.status === 401 && !url.endsWith("/login")) showLogin();
    throw new Error(data.error || data || `请求失败 (${response.status})`);
  }
  return data;
}

function showLogin() {
  document.body.classList.remove("app-active");
  $("#app").hidden = true;
  $("#setup-view").hidden = true;
  $("#login-view").hidden = false;
  $("#login-password").focus();
}

function showSetup() {
  document.body.classList.remove("app-active");
  $("#app").hidden = true;
  $("#login-view").hidden = true;
  $("#setup-view").hidden = false;
  $("#setup-user").focus();
}

async function enterApp(session) {
  state.csrf = session.csrf;
  state.user = session.user;
  state.root = session.fileRoot;
  state.recent = JSON.parse(localStorage.getItem("hostdesk_recent") || "[]");
  $("#account-name").textContent = state.user;
  $("#login-user").value = state.user;
  $("#root-path").textContent = state.root;
  $("#setup-view").hidden = true;
  $("#login-view").hidden = true;
  $("#app").hidden = false;
  document.body.classList.add("app-active");
  renderRecent();
  state.adminTab = "overview";
  switchView("admin");
  await loadFiles("");
}

function joinPath(...parts) {
  return parts.filter(Boolean).join("/").replace(/\/+/g, "/");
}

function parentPath(value) {
  const parts = value.split("/").filter(Boolean);
  parts.pop();
  return parts.join("/");
}

function baseName(value) {
  return value.split("/").filter(Boolean).pop() || "";
}

function formatSize(bytes, type) {
  if (type === "directory") return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let size = bytes / 1024;
  let index = 0;
  while (size >= 1024 && index < units.length - 1) { size /= 1024; index += 1; }
  return `${size >= 10 ? size.toFixed(0) : size.toFixed(1)} ${units[index]}`;
}

function fileIcon(entry) {
  if (entry.type === "directory") return "folder";
  if (entry.type === "link") return "link";
  const ext = entry.name.split(".").pop()?.toLowerCase();
  if (["png", "jpg", "jpeg", "gif", "webp", "svg"].includes(ext)) return "file-image";
  if (["zip", "tar", "gz", "tgz", "7z"].includes(ext)) return "file-archive";
  if (["js", "ts", "go", "py", "sh", "json", "html", "css", "md", "yml", "yaml"].includes(ext)) return "file-code";
  return "file";
}

function iconButton(name, title, action) {
  const button = document.createElement("button");
  button.type = "button";
  button.title = title;
  button.dataset.action = action;
  const icon = document.createElement("i");
  icon.dataset.lucide = name;
  button.append(icon);
  return button;
}

function renderFiles() {
  const tbody = $("#file-list");
  tbody.replaceChildren();
  for (const entry of state.entries) {
    const row = document.createElement("tr");
    row.dataset.path = entry.path;
    if (state.selected.has(entry.path)) row.classList.add("selected");

    const checkCell = document.createElement("td");
    checkCell.className = "check-cell";
    const check = document.createElement("input");
    check.type = "checkbox";
    check.checked = state.selected.has(entry.path);
    check.ariaLabel = `选择 ${entry.name}`;
    checkCell.append(check);

    const nameCell = document.createElement("td");
    const nameWrap = document.createElement("div");
    nameWrap.className = "file-name";
    const icon = document.createElement("i");
    icon.dataset.lucide = fileIcon(entry);
    if (entry.type === "directory") icon.className = "folder-icon";
    const name = document.createElement("span");
    name.textContent = entry.name;
    nameWrap.append(icon, name);
    nameCell.append(nameWrap);

    const size = document.createElement("td");
    size.textContent = formatSize(entry.size, entry.type);
    const modified = document.createElement("td");
    modified.textContent = new Date(entry.modified).toLocaleString("zh-CN", { hour12: false });
    const mode = document.createElement("td");
    mode.textContent = entry.mode;
    const actions = document.createElement("td");
    actions.className = "actions-cell";
    const actionWrap = document.createElement("div");
    actionWrap.className = "row-actions";
    if (entry.type === "directory") actionWrap.append(iconButton("arrow-right", "打开", "open"));
    else {
      actionWrap.append(iconButton("pencil", "编辑", "edit"));
      actionWrap.append(iconButton("download", "下载", "download"));
    }
    actionWrap.append(iconButton("ellipsis", "更多", "more"));
    actions.append(actionWrap);
    row.append(checkCell, nameCell, size, modified, mode, actions);
    tbody.append(row);
  }
  $("#empty-state").hidden = state.entries.length !== 0;
  $("#item-summary").textContent = `${state.entries.length} 项`;
  $("#path-summary").textContent = state.path ? `/${state.path}` : "/";
  $("#select-all").checked = state.entries.length > 0 && state.selected.size === state.entries.length;
  $("#select-all").indeterminate = state.selected.size > 0 && state.selected.size < state.entries.length;
  updateSelectionActions();
  refreshIcons(tbody);
}

function renderBreadcrumbs() {
  const bar = $("#breadcrumbs");
  bar.replaceChildren();
  const parts = state.path.split("/").filter(Boolean);
  const labels = ["根目录", ...parts];
  labels.forEach((label, index) => {
    if (index) {
      const separator = document.createElement("i");
      separator.dataset.lucide = "chevron-right";
      bar.append(separator);
    }
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = label;
    button.dataset.path = parts.slice(0, index).join("/");
    bar.append(button);
  });
  refreshIcons(bar);
  bar.scrollLeft = bar.scrollWidth;
}

function addRecent(value) {
  if (!value) return;
  state.recent = [value, ...state.recent.filter((item) => item !== value)].slice(0, 5);
  localStorage.setItem("hostdesk_recent", JSON.stringify(state.recent));
  renderRecent();
}

function renderRecent() {
  const list = $("#recent-list");
  list.replaceChildren();
  for (const pathValue of state.recent) {
    const button = document.createElement("button");
    button.className = "recent-link";
    button.dataset.jump = pathValue;
    const icon = document.createElement("i");
    icon.dataset.lucide = "folder-clock";
    const text = document.createElement("span");
    text.textContent = pathValue;
    button.append(icon, text);
    list.append(button);
  }
  refreshIcons(list);
}

async function loadFiles(pathValue = state.path) {
  $("#loading-state").hidden = false;
  $("#empty-state").hidden = true;
  try {
    const data = await api(`/api/files?path=${encodeURIComponent(pathValue)}`);
    state.path = data.path;
    state.entries = data.entries;
    state.selected.clear();
    renderBreadcrumbs();
    renderFiles();
    addRecent(state.path);
  } catch (error) {
    toast(error.message, "error");
  } finally {
    $("#loading-state").hidden = true;
  }
}

function updateSelectionActions() {
  const count = state.selected.size;
  $("#selection-actions").hidden = count === 0;
  $("#selection-count").textContent = `已选 ${count} 项`;
}

function setSelected(pathValue, checked) {
  if (checked) state.selected.add(pathValue);
  else state.selected.delete(pathValue);
  renderFiles();
}

function promptDialog({ title, label = "", value = "", message = "", confirmText = "确认", danger = false, input = true }) {
  const dialog = $("#action-dialog");
  $("#dialog-title").textContent = title;
  $("#dialog-message").textContent = message;
  $("#dialog-message").hidden = !message;
  $("#dialog-field").hidden = !input;
  $("#dialog-label").textContent = label;
  $("#dialog-input").value = value;
  $("#dialog-submit").textContent = confirmText;
  $("#dialog-submit").className = danger ? "danger-button" : "primary";
  dialog.showModal();
  if (input) setTimeout(() => $("#dialog-input").select(), 0);
  return new Promise((resolve) => {
    const finish = (result) => {
      dialog.close();
      $("#action-form").onsubmit = null;
      $("#dialog-cancel").onclick = null;
      $("#dialog-close").onclick = null;
      resolve(result);
    };
    $("#action-form").onsubmit = (event) => { event.preventDefault(); finish(input ? $("#dialog-input").value.trim() : true); };
    $("#dialog-cancel").onclick = () => finish(null);
    $("#dialog-close").onclick = () => finish(null);
  });
}

async function createItem(type) {
  const directory = type === "directory";
  const name = await promptDialog({ title: directory ? "新建目录" : "新建文件", label: "名称", value: directory ? "新目录" : "未命名.txt", confirmText: "创建" });
  if (!name) return;
  try {
    await api("/api/create", { method: "POST", body: { path: joinPath(state.path, name), type } });
    toast("创建成功");
    await loadFiles();
  } catch (error) { toast(error.message, "error"); }
}

async function deleteSelected() {
  const paths = [...state.selected];
  const approved = await promptDialog({ title: "删除所选内容", message: `将永久删除 ${paths.length} 项内容，此操作无法撤销。`, confirmText: "删除", danger: true, input: false });
  if (!approved) return;
  try {
    for (const pathValue of paths) await api("/api/delete", { method: "POST", body: { path: pathValue } });
    toast(`已删除 ${paths.length} 项`);
    await loadFiles();
  } catch (error) { toast(error.message, "error"); await loadFiles(); }
}

async function transferSelected(kind) {
  const destination = await promptDialog({ title: kind === "copy" ? "复制到" : "移动到", label: "目标目录（相对管理根目录）", value: state.path, confirmText: kind === "copy" ? "复制" : "移动" });
  if (destination === null) return;
  try {
    for (const from of state.selected) {
      const to = joinPath(destination, baseName(from));
      await api(`/api/${kind}`, { method: "POST", body: { from, to } });
    }
    toast(kind === "copy" ? "复制完成" : "移动完成");
    await loadFiles();
  } catch (error) { toast(error.message, "error"); }
}

async function archiveSelected() {
  const name = await promptDialog({ title: "打包所选内容", label: "压缩包名称", value: `archive-${new Date().toISOString().slice(0, 10)}.tar.gz`, confirmText: "开始打包" });
  if (!name) return;
  try {
    await api("/api/archive", { method: "POST", body: { paths: [...state.selected], name, destination: state.path } });
    toast("打包完成");
    await loadFiles();
  } catch (error) { toast(error.message, "error"); }
}

async function extractEntry(entry) {
  const approved = await promptDialog({ title: "解压文件", message: `将 ${entry.name} 解压到当前目录。`, confirmText: "解压", input: false });
  if (!approved) return;
  try {
    await api("/api/extract", { method: "POST", body: { path: entry.path, destination: state.path } });
    toast("解压完成");
    await loadFiles();
  } catch (error) { toast(error.message, "error"); }
}

async function openEditor(entry) {
  try {
    const data = await api(`/api/file?path=${encodeURIComponent(entry.path)}`);
    state.editorPath = entry.path;
    $("#editor-name").textContent = entry.name;
    $("#editor-path").textContent = `/${entry.path}`;
    $("#editor-content").value = data.content;
    updateEditorStats();
    $("#editor-dialog").showModal();
    $("#editor-content").focus();
  } catch (error) { toast(error.message, "error"); }
}

async function saveEditor() {
  try {
    await api("/api/file", { method: "PUT", body: { path: state.editorPath, content: $("#editor-content").value } });
    toast("文件已保存");
    $("#editor-dialog").close();
    await loadFiles();
  } catch (error) { toast(error.message, "error"); }
}

function updateEditorStats() {
  const content = $("#editor-content").value;
  $("#editor-stats").textContent = `${content.split("\n").length} 行 · ${new Blob([content]).size} 字节`;
}

async function renameEntry(entry) {
  const name = await promptDialog({ title: "重命名", label: "新名称", value: entry.name, confirmText: "重命名" });
  if (!name || name === entry.name) return;
  try {
    await api("/api/move", { method: "POST", body: { from: entry.path, to: joinPath(parentPath(entry.path), name) } });
    toast("重命名完成");
    await loadFiles();
  } catch (error) { toast(error.message, "error"); }
}

function menuAction(iconName, label, handler, danger = false) {
  const button = document.createElement("button");
  button.type = "button";
  button.role = "menuitem";
  if (danger) button.className = "danger-ghost";
  const icon = document.createElement("i");
  icon.dataset.lucide = iconName;
  const text = document.createElement("span");
  text.textContent = label;
  button.append(icon, text);
  button.addEventListener("click", () => { closeEntryMenu(); handler(); });
  return button;
}

function closeEntryMenu() {
  $("#entry-menu").hidden = true;
}

function openEntryMenu(entry, anchor) {
  const menu = $("#entry-menu");
  menu.replaceChildren();
  const isArchive = /\.(zip|tar|tar\.gz|tgz)$/i.test(entry.name);
  menu.append(menuAction("pencil-line", "重命名", () => renameEntry(entry)));
  if (isArchive) menu.append(menuAction("package-open", "解压到当前目录", () => extractEntry(entry)));
  if (entry.type !== "directory") menu.append(menuAction("download", "下载", () => { location.href = `/api/download?path=${encodeURIComponent(entry.path)}`; }));
  const separator = document.createElement("div");
  separator.className = "menu-separator";
  menu.append(separator, menuAction("trash-2", "删除", () => { state.selected = new Set([entry.path]); renderFiles(); deleteSelected(); }, true));
  menu.hidden = false;
  refreshIcons(menu);
  const rect = anchor.getBoundingClientRect();
  const width = menu.offsetWidth;
  const height = menu.offsetHeight;
  menu.style.left = `${Math.max(8, Math.min(rect.right - width, innerWidth - width - 8))}px`;
  menu.style.top = `${Math.max(8, Math.min(rect.bottom + 4, innerHeight - height - 8))}px`;
}

async function uploadFiles(files) {
  for (const file of files) {
    try {
      await api(`/api/upload?dir=${encodeURIComponent(state.path)}&name=${encodeURIComponent(file.name)}`, { method: "POST", body: file });
      toast(`${file.name} 上传完成`);
    } catch (error) { toast(`${file.name}：${error.message}`, "error"); }
  }
  $("#upload-input").value = "";
  await loadFiles();
}

function switchView(view) {
  document.querySelectorAll("[data-view]").forEach((button) => button.classList.toggle("active", button.dataset.view === view));
  $("#files-view").hidden = view !== "files";
  $("#admin-view").hidden = view !== "admin";
  if (view === "admin") loadAdminTab(state.adminTab);
}

function fitTerminal() {
  if (!fitAddon || $("#admin-terminal").hidden || $("#admin-view").hidden) return;
  cancelAnimationFrame(terminalFitFrame);
  terminalFitFrame = requestAnimationFrame(() => {
    const container = $("#terminal");
    if (container.clientWidth > 0 && container.clientHeight > 0) fitAddon.fit();
  });
}

function initTerminal() {
  if (terminal) return;
  terminal = new window.Terminal({
    cursorBlink: true,
    fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", monospace',
    fontSize: 13,
    lineHeight: 1.25,
    scrollback: 5000,
    theme: { background: "#151918", foreground: "#d9e1de", cursor: "#49c99a", selectionBackground: "#34594c", black: "#202624", red: "#ef7777", green: "#49c99a", yellow: "#e8b35c", blue: "#79a9e8", magenta: "#ce8edb", cyan: "#71c7c7", white: "#d9e1de" }
  });
  fitAddon = new window.FitAddon.FitAddon();
  terminal.loadAddon(fitAddon);
  terminal.open($("#terminal"));
  fitTerminal();
  terminal.writeln("\x1b[38;5;72mHostDesk WebSSH\x1b[0m");
  terminal.writeln("\x1b[38;5;245m等待连接...\x1b[0m");
  terminal.onData((data) => {
    if (sshSocket?.readyState === WebSocket.OPEN) sshSocket.send(JSON.stringify({ type: "input", data }));
  });
  terminal.onResize(({ cols, rows }) => {
    if (sshSocket?.readyState === WebSocket.OPEN) sshSocket.send(JSON.stringify({ type: "resize", cols, rows }));
  });
  new ResizeObserver(fitTerminal).observe($("#terminal"));
}

function setTerminalConnected(connected, status) {
  $("#terminal-dot").classList.toggle("connected", connected);
  $("#terminal-status").textContent = status;
  $("#ssh-connect").hidden = connected;
  $("#ssh-disconnect").hidden = !connected;
}

function connectSsh(event) {
  event.preventDefault();
  initTerminal();
  sshSocket?.close();
  const protocol = location.protocol === "https:" ? "wss" : "ws";
  sshSocket = new WebSocket(`${protocol}://${location.host}/ws/ssh`);
  terminal.clear();
  terminal.writeln("\x1b[38;5;245m正在建立 SSH 连接...\x1b[0m");
  $("#terminal-status").textContent = "连接中";
  sshSocket.onopen = () => sshSocket.send(JSON.stringify({
    type: "connect",
    host: $("#ssh-host").value.trim(),
    port: Number($("#ssh-port").value),
    username: $("#ssh-user").value.trim(),
    password: $("#ssh-password").value,
    privateKey: sshPrivateKey
  }));
  sshSocket.onmessage = ({ data }) => {
    const message = JSON.parse(data);
    if (message.type === "data") terminal.write(message.data);
    if (message.type === "ready") { setTerminalConnected(true, "已连接"); fitTerminal(); terminal.focus(); }
    if (message.type === "error") { terminal.writeln(`\r\n\x1b[31m${message.message}\x1b[0m`); setTerminalConnected(false, "连接失败"); }
    if (message.type === "close") setTerminalConnected(false, "连接已关闭");
  };
  sshSocket.onerror = () => { terminal.writeln("\r\n\x1b[31mWebSocket 连接失败\x1b[0m"); setTerminalConnected(false, "连接失败"); };
  sshSocket.onclose = () => setTerminalConnected(false, "未连接");
}

const adminTabs = {
  overview: ["服务器概览", "软件安装与服务状态"],
  "server-settings": ["服务器设置", "主机、时区与交换空间"],
  terminal: ["终端", "WebSSH 连接"],
  sites: ["网站管理", "Nginx 虚拟主机"],
  certificates: ["证书管理", "申请与自动续期"],
  nginx: ["Nginx", "全局设置与配置检查"],
  php: ["PHP", "PHP-FPM 设置与扩展"],
  databases: ["数据库", "MariaDB/MySQL 数据与用户"],
  ftp: ["FTP", "vsftpd 服务与用户"],
  containers: ["容器管理", "Docker 容器与运行配置"],
  account: ["账号安全", "管理员凭据与登录保护"]
};

async function loadAdminTab(tab) {
  state.adminTab = tab;
  document.querySelectorAll("[data-admin-tab]").forEach((button) => button.classList.toggle("active", button.classList.contains("admin-link") && button.dataset.adminTab === tab));
  document.querySelectorAll(".admin-content").forEach((section) => { section.hidden = section.id !== `admin-${tab}`; });
  $("#admin-title").textContent = adminTabs[tab][0];
  $("#admin-subtitle").textContent = adminTabs[tab][1];
  try {
    if (tab === "overview") await loadAdminOverview();
    if (tab === "server-settings") await loadServerSettings();
    if (tab === "terminal") { initTerminal(); setTimeout(fitTerminal, 50); }
    if (tab === "sites") await loadSites();
    if (tab === "certificates") await loadCertificates();
    if (tab === "nginx") await loadNginxSettings();
    if (tab === "php") await loadPHP();
    if (tab === "databases") await loadDatabases();
    if (tab === "ftp") await loadFTP();
    if (tab === "containers") await loadContainers();
    if (tab === "account") await loadAccount();
  } catch (error) { toast(error.message, "error"); }
}

async function loadAccount() {
  const account = await api("/api/admin/account");
  $("#account-user").value = account.username;
  $("#account-current-password").value = "";
  $("#account-new-password").value = "";
  $("#account-confirm-password").value = "";
}

function displayServerTime(value) {
  return value ? value.replace("T", " ") : "-";
}

async function loadServerSettings() {
  const settings = await api("/api/admin/server-settings");
  $("#server-hostname").value = settings.hostname || "";
  const timezone = $("#server-timezone");
  timezone.replaceChildren();
  for (const name of settings.timezones || []) {
    const option = document.createElement("option");
    option.value = name;
    option.textContent = name;
    timezone.append(option);
  }
  timezone.value = settings.timezone;
  if (!timezone.value && settings.timezone) {
    const option = document.createElement("option");
    option.value = settings.timezone;
    option.textContent = settings.timezone;
    timezone.prepend(option);
    timezone.value = settings.timezone;
  }
  $("#server-os").textContent = settings.operatingSystem || "Linux";
  $("#server-kernel").textContent = `${settings.kernel || "-"} · ${settings.architecture || "-"}`;
  $("#server-current-time").textContent = displayServerTime(settings.currentTime);
  $("#server-ntp").textContent = settings.ntpRunning ? "运行中" : "未运行";
  $("#server-ntp").className = settings.ntpRunning ? "status-text running" : "status-text stopped";
  $("#swap-summary").textContent = `${formatSize(settings.swapTotalBytes || 0)} 总计 · ${formatSize(settings.swapUsedBytes || 0)} 已用`;

  const list = $("#swap-list");
  list.replaceChildren();
  for (const swap of settings.swaps || []) {
    const row = document.createElement("tr");
    const path = document.createElement("td");
    const pathCode = document.createElement("code");
    pathCode.textContent = swap.path;
    path.append(pathCode);
    const type = document.createElement("td");
    type.textContent = swap.type === "file" ? "交换文件" : "交换分区";
    const size = document.createElement("td");
    size.textContent = formatSize(swap.sizeBytes || 0);
    const used = document.createElement("td");
    used.textContent = swap.active ? formatSize(swap.usedBytes || 0) : "未启用";
    const priority = document.createElement("td");
    priority.textContent = swap.active ? swap.priority : "-";
    const management = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = `state-badge ${swap.active ? "running" : ""}`;
    badge.textContent = swap.managed ? (swap.active ? "HostDesk" : "HostDesk · 未启用") : "系统配置";
    management.append(badge);
    const actions = document.createElement("td");
    if (swap.managed) {
      const remove = iconButton("trash-2", "移除交换空间", "remove-swap");
      remove.className = "row-command danger-ghost";
      remove.addEventListener("click", async () => {
        const approved = await promptDialog({ title: "移除 HostDesk 交换空间", message: "交换文件将被停用并删除。若当前内存不足，系统会拒绝此操作。", confirmText: "移除", danger: true, input: false });
        if (approved) runAdminAction(remove, () => api("/api/admin/server-settings/swap", { method: "DELETE" }), "交换空间已移除");
      });
      actions.append(remove);
    }
    row.append(path, type, size, used, priority, management, actions);
    list.append(row);
  }
  const swaps = settings.swaps || [];
  $("#swaps-empty").hidden = swaps.length > 0;
  $("#new-swap-btn").disabled = swaps.some((swap) => swap.managed);
  $("#new-swap-btn").title = $("#new-swap-btn").disabled ? "请先移除现有的 HostDesk 交换文件" : "增加交换空间";
  refreshIcons($("#admin-server-settings"));
}

function serviceLabel(name) {
  return { nginx: "Nginx", php: "PHP-FPM", mysql: "MariaDB", ftp: "vsftpd" }[name] || name;
}

function serviceIcon(name) {
  return { nginx: "network", php: "file-code-2", mysql: "database", ftp: "folder-sync" }[name] || "box";
}

function serviceCommandButton(iconName, title, handler, className = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = className;
  const icon = document.createElement("i");
  icon.dataset.lucide = iconName;
  const text = document.createElement("span");
  text.textContent = title;
  button.append(icon, text);
  button.addEventListener("click", () => handler(button));
  return button;
}

function formatPercent(value) {
  const number = Number(value) || 0;
  return `${number < 10 ? number.toFixed(1) : number.toFixed(0)}%`;
}

function formatUptime(seconds) {
  let remaining = Math.max(0, Math.floor(Number(seconds) || 0));
  const days = Math.floor(remaining / 86400);
  remaining %= 86400;
  const hours = Math.floor(remaining / 3600);
  const minutes = Math.floor((remaining % 3600) / 60);
  if (days) return `${days} 天 ${hours} 小时`;
  if (hours) return `${hours} 小时 ${minutes} 分钟`;
  return `${minutes} 分钟`;
}

function usagePercent(resource) {
  return resource?.total ? Math.min(100, resource.used * 100 / resource.total) : 0;
}

function resourceMetric({ icon, label, value, detail, percent }) {
  const item = document.createElement("article");
  item.className = "resource-item";
  const heading = document.createElement("div");
  heading.className = "resource-item-heading";
  const iconElement = document.createElement("i");
  iconElement.dataset.lucide = icon;
  const labelElement = document.createElement("span");
  labelElement.textContent = label;
  heading.append(iconElement, labelElement);
  const valueElement = document.createElement("strong");
  valueElement.textContent = value;
  const detailElement = document.createElement("span");
  detailElement.className = "resource-detail";
  detailElement.textContent = detail;
  item.append(heading, valueElement, detailElement);
  if (percent !== undefined) {
    const progress = document.createElement("div");
    progress.className = "resource-progress";
    progress.setAttribute("role", "progressbar");
    progress.setAttribute("aria-valuemin", "0");
    progress.setAttribute("aria-valuemax", "100");
    progress.setAttribute("aria-valuenow", String(Math.round(percent)));
    const bar = document.createElement("span");
    bar.style.width = `${Math.min(100, Math.max(0, percent))}%`;
    progress.append(bar);
    item.append(progress);
  }
  return item;
}

function renderSystemOverview(system) {
  const summary = $("#server-summary");
  summary.replaceChildren();
  const identity = document.createElement("div");
  identity.className = "server-identity";
  const iconWrap = document.createElement("span");
  iconWrap.className = "server-icon";
  const icon = document.createElement("i");
  icon.dataset.lucide = "server";
  iconWrap.append(icon);
  const identityText = document.createElement("div");
  const hostname = document.createElement("strong");
  hostname.textContent = system.hostname || "本机服务器";
  const kernel = document.createElement("span");
  kernel.textContent = system.kernel ? `Linux ${system.kernel}` : "Linux";
  identityText.append(hostname, kernel);
  identity.append(iconWrap, identityText);

  const facts = document.createElement("dl");
  for (const [label, value] of [
    ["公网 IP", system.publicIpAddress || "暂未获取"],
    ["运行时间", formatUptime(system.uptimeSeconds)],
  ]) {
    const fact = document.createElement("div");
    const term = document.createElement("dt");
    term.textContent = label;
    const description = document.createElement("dd");
    description.textContent = value;
    fact.append(term, description);
    facts.append(fact);
  }
  summary.append(identity, facts);

  const memoryPercent = usagePercent(system.memory);
  const diskPercent = usagePercent(system.disk);
  const loads = system.cpu?.loadAverage?.length ? system.cpu.loadAverage.map((value) => Number(value).toFixed(2)).join(" / ") : "暂无";
  const resources = $("#resource-grid");
  resources.replaceChildren(
    resourceMetric({ icon: "cpu", label: "CPU", value: formatPercent(system.cpu?.usagePercent), detail: `${system.cpu?.cores || 0} 核 · 负载 ${loads}`, percent: Number(system.cpu?.usagePercent) || 0 }),
    resourceMetric({ icon: "memory-stick", label: "内存", value: formatPercent(memoryPercent), detail: `${formatSize(system.memory?.used || 0)} / ${formatSize(system.memory?.total || 0)}`, percent: memoryPercent }),
    resourceMetric({ icon: "hard-drive", label: "根分区", value: formatPercent(diskPercent), detail: `${formatSize(system.disk?.used || 0)} / ${formatSize(system.disk?.total || 0)}`, percent: diskPercent }),
    resourceMetric({ icon: "arrow-down-up", label: "累计流量", value: `↓ ${formatSize(system.network?.receivedBytes || 0)}`, detail: `上传 ↑ ${formatSize(system.network?.transmittedBytes || 0)}` }),
  );
}

async function runAdminAction(button, action, successMessage) {
  button.disabled = true;
  try {
    await action();
    toast(successMessage);
    await loadAdminTab(state.adminTab);
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
}

async function loadAdminOverview() {
  const [data, update] = await Promise.all([api("/api/admin/overview"), api("/api/admin/update")]);
  state.overview = data;
  $("#admin-subtitle").textContent = data.platform;
  renderSystemOverview(data.system || {});
  renderUpdateStatus(update);
  const grid = $("#service-grid");
  grid.replaceChildren();
  for (const service of data.services) {
    const card = document.createElement("article");
    card.className = "service-card";
    const head = document.createElement("div");
    head.className = "service-card-head";
    const identity = document.createElement("div");
    identity.className = "service-identity";
    const iconWrap = document.createElement("span");
    iconWrap.className = "service-icon";
    const icon = document.createElement("i");
    icon.dataset.lucide = serviceIcon(service.name);
    iconWrap.append(icon);
    const label = document.createElement("div");
    label.className = "service-label";
    const nameRow = document.createElement("div");
    nameRow.className = "service-name-row";
    const strong = document.createElement("strong");
    strong.textContent = serviceLabel(service.name);
    const status = document.createElement("span");
    const statusLabel = service.running ? "运行中" : service.installed ? "已停止" : "未安装";
    status.className = `service-status-dot ${service.running ? "running" : "stopped"}`;
    status.title = statusLabel;
    status.setAttribute("role", "img");
    status.setAttribute("aria-label", statusLabel);
    nameRow.append(strong, status);
    const version = document.createElement("span");
    version.textContent = service.version || (service.installed ? "已安装" : "尚未安装");
    label.append(nameRow, version);
    identity.append(iconWrap, label);
    head.append(identity);
    const actions = document.createElement("div");
    actions.className = "service-actions";
    if (!service.installed) {
      actions.append(serviceCommandButton("download", "安装", (button) => runAdminAction(button, () => api(`/api/admin/components/${service.name}/install`, { method: "POST" }), `${serviceLabel(service.name)} 安装完成`), "primary"));
    } else {
      const serviceName = service.service;
      const action = service.running ? "restart" : "start";
      actions.append(serviceCommandButton(service.running ? "rotate-cw" : "play", service.running ? "重启" : "启动", (button) => runAdminAction(button, () => api(`/api/admin/services/${serviceName}/${action}`, { method: "POST" }), "服务状态已更新")));
      actions.append(serviceCommandButton("package-minus", "卸载", async (button) => {
        const approved = await promptDialog({ title: `卸载 ${serviceLabel(service.name)}`, message: "程序包将被移除，网站和数据库数据目录会保留。", confirmText: "卸载", danger: true, input: false });
        if (approved) runAdminAction(button, () => api(`/api/admin/components/${service.name}/remove`, { method: "POST" }), "组件已卸载");
      }, "danger-ghost"));
    }
    card.append(head, actions);
    grid.append(card);
  }
  refreshIcons(grid);
}

function renderUpdateStatus(update) {
  const container = $("#update-status");
  container.replaceChildren();
  container.classList.toggle("update-available", Boolean(update.updateAvailable));
  const info = document.createElement("div");
  const icon = document.createElement("i");
  icon.dataset.lucide = update.updateAvailable ? "circle-arrow-up" : "badge-check";
  const text = document.createElement("div");
  const title = document.createElement("strong");
  const detail = document.createElement("span");
  title.textContent = update.updateAvailable ? `发现新版本 ${update.latestVersion}` : "HostDesk 已是最新版本";
  if (update.error) title.textContent = "暂时无法检查更新";
  const current = update.currentVersion === "dev" ? "开发构建" : update.currentVersion;
  detail.textContent = update.error || `当前 ${current}${update.latestVersion ? ` · 最新 ${update.latestVersion}` : ""}`;
  text.append(title, detail);
  info.append(icon, text);
  const actions = document.createElement("div");
  actions.className = "update-actions";
  const refresh = serviceCommandButton("refresh-cw", "检查更新", async (button) => {
    button.disabled = true;
    try { renderUpdateStatus(await api("/api/admin/update?refresh=1")); }
    catch (error) { toast(error.message, "error"); }
    finally { button.disabled = false; }
  });
  actions.append(refresh);
  if (update.releaseUrl) {
    const release = serviceCommandButton("external-link", "查看版本", () => window.open(update.releaseUrl, "_blank", "noopener"), update.updateAvailable ? "primary" : "");
    actions.append(release);
  }
  container.append(info, actions);
  refreshIcons(container);
}

async function loadNginxSettings() {
  const settings = await api("/api/admin/nginx/settings");
  $("#nginx-body-size").value = settings.clientMaxBodySize;
  $("#nginx-keepalive").value = settings.keepaliveTimeout;
  $("#nginx-gzip").checked = settings.gzip;
  $("#nginx-tokens").checked = settings.serverTokens;
}

function siteTypeLabel(type) { return { static: "静态", php: "PHP", proxy: "反代" }[type] || type; }

function managedRelativePath(absolutePath) {
  if (typeof absolutePath !== "string" || !absolutePath.startsWith("/")) return null;
  const root = state.root === "/" ? "/" : state.root.replace(/\/+$/, "");
  const normalized = absolutePath.replace(/\/+/g, "/").replace(/\/$/, "") || "/";
  if (root === "/") return normalized === "/" ? "" : normalized.slice(1);
  if (normalized === root) return "";
  return normalized.startsWith(`${root}/`) ? normalized.slice(root.length + 1) : null;
}

function labelCommand(button, label) {
  const text = document.createElement("span");
  text.textContent = label;
  button.append(text);
  button.className = "row-command labeled";
  return button;
}

async function openSiteFiles(pathValue) {
  switchView("files");
  await loadFiles(pathValue);
}

async function loadSites() {
  const data = await api("/api/admin/sites");
  state.sites = data.sites;
  const tbody = $("#site-list");
  tbody.replaceChildren();
  for (const site of state.sites) {
    const row = document.createElement("tr");
    const domain = document.createElement("td");
    const strong = document.createElement("strong");
    strong.textContent = site.domain;
    domain.append(strong);
    const type = document.createElement("td");
    type.className = "type-label";
    type.textContent = siteTypeLabel(site.type);
    const target = document.createElement("td");
    target.textContent = site.type === "proxy" ? site.upstream : site.root;
    const ssl = document.createElement("td");
    ssl.textContent = site.ssl ? "已启用" : "HTTP";
    const status = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = `state-badge ${site.enabled ? "running" : ""}`;
    badge.textContent = site.enabled ? "启用" : "停用";
    status.append(badge);
    const actions = document.createElement("td");
    actions.className = "site-actions-cell";
    const filePath = site.type === "proxy" ? null : managedRelativePath(site.root);
    if (filePath !== null) {
      const files = labelCommand(iconButton("folder-open", "管理网站文件", "site-files"), "文件");
      files.addEventListener("click", () => openSiteFiles(filePath));
      actions.append(files);
    }
    const edit = labelCommand(iconButton("settings-2", "修改网站配置", "edit-site"), "配置");
    edit.addEventListener("click", () => openSiteEditor(site));
    actions.append(edit);
    const toggle = iconButton(site.enabled ? "pause" : "play", site.enabled ? "停用" : "启用", "toggle-site");
    toggle.className = "row-command";
    toggle.addEventListener("click", (event) => runAdminAction(event.currentTarget, () => api(`/api/admin/sites/${site.id}/${site.enabled ? "disable" : "enable"}`, { method: "POST" }), "网站状态已更新"));
    const remove = iconButton("trash-2", "删除网站", "delete-site");
    remove.className = "row-command danger-ghost";
    remove.addEventListener("click", async () => {
      const approved = await promptDialog({ title: `删除 ${site.domain}`, message: "将删除 Nginx 站点配置，网站目录和文件会保留。", confirmText: "删除", danger: true, input: false });
      if (approved) runAdminAction(remove, () => api(`/api/admin/sites/${site.id}`, { method: "DELETE" }), "网站配置已删除");
    });
    actions.append(toggle, remove);
    row.append(domain, type, target, ssl, status, actions);
    tbody.append(row);
  }
  $("#sites-empty").hidden = state.sites.length > 0;
  refreshIcons(tbody);
}

function updateSiteFields() {
  const proxy = $("#site-type").value === "proxy";
  const php = $("#site-type").value === "php";
  $("#site-root-field").hidden = proxy;
  $("#site-upstream-field").hidden = !proxy;
  $("#site-rewrite-mode-field").hidden = !php;
  $("#site-rewrite-rules-field").hidden = !php || $("#site-rewrite-mode").value !== "custom";
  $("#site-cert-fields").hidden = !$("#site-ssl").checked;
}

function openSiteEditor(site) {
  editingSite = site;
  $("#site-form").reset();
  $("#site-dialog-title").textContent = `修改 ${site.domain}`;
  $("#site-submit").textContent = "保存配置";
  $("#site-domain").value = site.domain;
  $("#site-domain").disabled = true;
  $("#site-aliases").value = (site.aliases || []).join(", ");
  $("#site-type").value = site.type;
  $("#site-root").value = site.root || "";
  $("#site-upstream").value = site.upstream || "";
  $("#site-rewrite-mode").value = site.rewriteMode || (site.type === "php" ? "laravel" : "none");
  $("#site-rewrite-rules").value = site.rewriteRules || "";
  $("#site-ssl").checked = Boolean(site.ssl);
  $("#site-cert").value = site.certificate || "";
  $("#site-key").value = site.privateKey || "";
  updateSiteFields();
  $("#site-dialog").showModal();
}

function formatCertificateDate(value) {
  if (!value) return "—";
  return new Intl.DateTimeFormat("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit" }).format(new Date(value));
}

function certificateChallengeLabel(value) {
  return value === "dns-cloudflare" ? "DNS / Cloudflare" : "HTTP";
}

function renderDNSSettings(settings) {
  state.dnsSettings = settings || {};
  $("#dns-default-email").value = settings.defaultEmail || "";
  for (const [name, configured] of [["cloudflare", settings.cloudflareConfigured]]) {
    const status = $(`#${name}-status`);
    status.textContent = configured ? "已配置" : "未配置";
    status.className = `state-badge ${configured ? "running" : ""}`;
    $(`#clear-${name}-field`).hidden = !configured;
    $(`#clear-${name}`).checked = false;
  }
  $("#dns-cloudflare-token").value = "";
  $("#certificate-provider").options[0].textContent = `Cloudflare · ${settings.cloudflareConfigured ? "已配置" : "未配置"}`;
}

function populateCertificateSites() {
  const select = $("#certificate-site");
  const current = select.value;
  select.replaceChildren();
  for (const site of state.sites) {
    const option = document.createElement("option");
    option.value = site.id;
    option.textContent = site.domain;
    select.append(option);
  }
  if ([...select.options].some((option) => option.value === current)) select.value = current;
}

function updateCertificateDomains() {
  const site = state.sites.find((item) => item.id === $("#certificate-site").value);
  if (site) $("#certificate-domains").value = [site.domain, ...(site.aliases || [])].join(", ");
}

function updateCertificateChallenge() {
  const dns = document.querySelector('input[name="certificate-challenge"]:checked').value === "dns";
  $("#certificate-provider-field").hidden = !dns;
  $("#certificate-provider").required = dns;
}

async function loadCertificates() {
  const [data, sitesData, settings] = await Promise.all([api("/api/admin/certificates"), api("/api/admin/sites"), api("/api/admin/dns-settings")]);
  state.certificates = data.certificates;
  state.sites = sitesData.sites;
  renderDNSSettings(settings);
  populateCertificateSites();
  const tbody = $("#certificate-list");
  tbody.replaceChildren();
  for (const certificate of state.certificates) {
    const row = document.createElement("tr");
    const site = document.createElement("td"); site.textContent = state.sites.find((item) => item.id === certificate.siteId)?.domain || certificate.siteId;
    const domains = document.createElement("td"); domains.className = "certificate-domains"; domains.textContent = certificate.domains.join(", "); domains.title = domains.textContent;
    const challenge = document.createElement("td"); challenge.textContent = certificateChallengeLabel(certificate.challenge);
    const expiry = document.createElement("td"); expiry.className = "certificate-expiry"; expiry.textContent = formatCertificateDate(certificate.expiresAt);
    const automatic = document.createElement("td"); automatic.textContent = certificate.autoRenew ? "开启" : "关闭";
    const status = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = `state-badge ${!certificate.lastError && !certificate.renewalDue ? "running" : ""}`;
    badge.textContent = certificate.lastError ? "续期失败" : certificate.renewalDue ? "待续期" : "有效";
    if (certificate.lastError) badge.title = certificate.lastError;
    status.append(badge);
    const actions = document.createElement("td");
    const renew = iconButton("refresh-cw", "立即续期", "renew-certificate");
    renew.className = "row-command";
    renew.addEventListener("click", () => runAdminAction(renew, () => api(`/api/admin/certificates/${encodeURIComponent(certificate.id)}/renew`, { method: "POST" }), "证书续期完成"));
    actions.append(renew);
    row.append(site, domains, challenge, expiry, automatic, status, actions);
    tbody.append(row);
  }
  $("#certificates-empty").hidden = state.certificates.length > 0;
  refreshIcons($("#admin-certificates"));
}

async function loadPHP() {
  const data = await api("/api/admin/php");
  $("#php-version").textContent = data.version || "未安装";
  const settings = data.settings || {};
  $("#php-upload").value = settings.uploadMaxFilesize || "64m";
  $("#php-post").value = settings.postMaxSize || "64m";
  $("#php-memory").value = settings.memoryLimit || "256m";
  $("#php-time").value = settings.maxExecutionTime || 30;
  $("#php-errors").checked = Boolean(settings.displayErrors);
  $("#php-settings-form").querySelector('button[type="submit"]').disabled = !data.installed;
  const grid = $("#extension-grid");
  grid.replaceChildren();
  for (const extension of data.extensions) {
    const item = document.createElement("div");
    item.className = "extension-item";
    const name = document.createElement("span");
    name.textContent = extension.name;
    const button = iconButton(extension.installed ? "minus" : "plus", extension.installed ? "卸载" : "安装", "extension");
    button.disabled = !data.installed;
    button.addEventListener("click", () => runAdminAction(button, () => api(`/api/admin/php/extensions/${extension.name}/${extension.installed ? "remove" : "install"}`, { method: "POST" }), `扩展 ${extension.name} 已${extension.installed ? "卸载" : "安装"}`));
    item.append(name, button);
    grid.append(item);
  }
  refreshIcons(grid);
}

async function loadDatabases() {
  let databaseData;
  let userData;
  try {
    [databaseData, userData] = await Promise.all([api("/api/admin/databases"), api("/api/admin/database-users")]);
  } catch (error) {
    $("#database-list").replaceChildren();
    $("#db-user-list").replaceChildren();
    $("#databases-empty").hidden = false;
    $("#databases-empty").textContent = error.message;
    $("#db-users-empty").hidden = false;
    $("#db-users-empty").textContent = "MariaDB 启动后可管理用户";
    return;
  }
  state.databases = databaseData.databases;
  const databaseList = $("#database-list");
  databaseList.replaceChildren();
  for (const database of state.databases) {
    const row = document.createElement("tr");
    const name = document.createElement("td"); name.textContent = database.name;
    const tables = document.createElement("td"); tables.textContent = database.tables;
    const size = document.createElement("td"); size.textContent = formatSize(database.size, "file");
    const actions = document.createElement("td");
    const remove = iconButton("trash-2", "删除数据库", "delete-db"); remove.className = "row-command danger-ghost";
    remove.addEventListener("click", async () => {
      const confirm = await promptDialog({ title: `删除数据库 ${database.name}`, label: `输入数据库名称 ${database.name} 确认`, confirmText: "永久删除", danger: true });
      if (confirm === database.name) runAdminAction(remove, () => api(`/api/admin/databases/${encodeURIComponent(database.name)}`, { method: "DELETE" }), "数据库已删除");
    });
    actions.append(remove); row.append(name, tables, size, actions); databaseList.append(row);
  }
  $("#databases-empty").hidden = state.databases.length > 0;
  const select = $("#db-user-database"); select.replaceChildren();
  for (const database of state.databases) { const option = document.createElement("option"); option.value = database.name; option.textContent = database.name; select.append(option); }
  const userList = $("#db-user-list"); userList.replaceChildren();
  for (const user of userData.users) {
    const row = document.createElement("tr");
    const name = document.createElement("td"); name.textContent = user.user;
    const host = document.createElement("td"); host.textContent = user.host;
    const plugin = document.createElement("td"); plugin.textContent = user.plugin;
    const actions = document.createElement("td");
    const remove = iconButton("user-minus", "删除用户", "delete-user"); remove.className = "row-command danger-ghost";
    remove.addEventListener("click", async () => {
      const approved = await promptDialog({ title: `删除用户 ${user.user}@${user.host}`, message: "该用户的数据库不会被删除。", confirmText: "删除", danger: true, input: false });
      if (approved) runAdminAction(remove, () => api(`/api/admin/database-users/${encodeURIComponent(user.user)}?host=${encodeURIComponent(user.host)}`, { method: "DELETE" }), "数据库用户已删除");
    });
    actions.append(remove); row.append(name, host, plugin, actions); userList.append(row);
  }
  $("#db-users-empty").hidden = userData.users.length > 0;
  refreshIcons($("#admin-databases"));
}

function openFTPUserDialog(user = null) {
  editingFTPUser = user;
  $("#ftp-user-form").reset();
  $("#ftp-user-dialog-title").textContent = user ? `重置 ${user.username} 的密码` : "添加 FTP 用户";
  $("#ftp-user-name").disabled = Boolean(user);
  $("#ftp-user-name").value = user?.username || "";
  $("#ftp-user-home").textContent = user?.home || "/srv/ftp/<用户名>";
  $("#ftp-user-dialog").showModal();
  (user ? $("#ftp-user-password") : $("#ftp-user-name")).focus();
}

async function loadFTP() {
  const data = await api("/api/admin/ftp");
  const status = data.status || {};
  state.ftpUsers = data.users || [];
  $("#ftp-root").textContent = data.root || "/srv/ftp";
  $("#ftp-passive-ports").textContent = data.passivePorts || "40000-40100";
  $("#ftp-status-text").textContent = !status.installed ? "vsftpd 尚未安装" : status.running ? "vsftpd 运行中" : "vsftpd 已停止";
  $("#ftp-install-btn").hidden = Boolean(status.installed);
  $("#ftp-restart-btn").hidden = !status.installed;
  $("#ftp-restart-btn").dataset.action = status.running ? "restart" : "start";
  $("#ftp-restart-btn span").textContent = status.running ? "重启服务" : "启动服务";
  $("#new-ftp-user-btn").hidden = !status.installed;

  const list = $("#ftp-user-list");
  list.replaceChildren();
  for (const user of state.ftpUsers) {
    const row = document.createElement("tr");
    const name = document.createElement("td");
    const strong = document.createElement("strong");
    strong.textContent = user.username;
    name.append(strong);
    const home = document.createElement("td");
    const code = document.createElement("code");
    code.textContent = user.home;
    home.append(code);
    const created = document.createElement("td");
    created.textContent = user.createdAt ? new Date(user.createdAt).toLocaleString("zh-CN", { hour12: false }) : "-";
    const system = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = `state-badge ${user.systemPresent ? "running" : ""}`;
    badge.textContent = user.systemPresent ? "正常" : "系统用户缺失";
    system.append(badge);
    const actions = document.createElement("td");
    const password = iconButton("key-round", "重置密码", "ftp-password");
    password.className = "row-command";
    password.disabled = !user.systemPresent;
    password.addEventListener("click", () => openFTPUserDialog(user));
    const remove = iconButton("user-minus", "删除 FTP 用户", "ftp-remove");
    remove.className = "row-command danger-ghost";
    remove.addEventListener("click", async () => {
      const approved = await promptDialog({ title: `删除 FTP 用户 ${user.username}`, message: `系统账号将被删除，文件目录 ${user.home} 会保留。`, confirmText: "删除", danger: true, input: false });
      if (approved) runAdminAction(remove, () => api(`/api/admin/ftp/users/${encodeURIComponent(user.username)}`, { method: "DELETE" }), "FTP 用户已删除，文件目录已保留");
    });
    actions.append(password, remove);
    row.append(name, home, created, system, actions);
    list.append(row);
  }
  const empty = $("#ftp-users-empty");
  empty.hidden = state.ftpUsers.length > 0;
  empty.textContent = status.installed ? "暂无 FTP 用户" : "安装 FTP 服务后可添加用户";
  refreshIcons($("#admin-ftp"));
}

function containerStateLabel(container) {
  if (container.state === "running") return "运行中";
  if (container.state === "paused") return "已暂停";
  if (container.state === "restarting") return "重启中";
  if (container.state === "created") return "已创建";
  if (container.state === "exited") return "已停止";
  if (container.state === "dead") return "异常";
  return container.state || "未知";
}

function containerActionButton(iconName, title, className = "") {
  const button = iconButton(iconName, title, "container-action");
  button.className = `row-command ${className}`.trim();
  return button;
}

async function openContainerEditor(container, button) {
  if (button) button.disabled = true;
  try {
    editingContainer = await api(`/api/admin/containers/${encodeURIComponent(container.id)}`);
    $("#container-form").reset();
    $("#container-name").value = editingContainer.name;
    $("#container-restart-policy").value = editingContainer.restartPolicy || "no";
    $("#container-retry").value = editingContainer.maximumRetryCount || 0;
    $("#container-cpus").value = editingContainer.cpus || 0;
    $("#container-memory").value = editingContainer.memoryBytes ? Math.round(editingContainer.memoryBytes / 1024 / 1024) : 0;
    $("#container-image").textContent = editingContainer.image || "-";
    $("#container-command").textContent = editingContainer.command?.join(" ") || "使用镜像默认命令";
    $("#container-networks").textContent = editingContainer.networks?.join(", ") || editingContainer.networkMode || "-";
    $("#container-ports").textContent = editingContainer.ports?.length ? editingContainer.ports.map((port) => port.hostPort ? `${port.hostIp || "0.0.0.0"}:${port.hostPort} → ${port.containerPort}` : port.containerPort).join("\n") : "未映射端口";
    $("#container-mounts").textContent = editingContainer.mounts?.length ? editingContainer.mounts.map((mount) => `${mount.source} → ${mount.destination}${mount.readWrite ? "" : " (只读)"}`).join("\n") : "无数据挂载";
    $("#container-environment").textContent = editingContainer.environment?.join("\n") || "无环境变量";
    const warning = $("#container-compose-warning");
    warning.hidden = editingContainer.managedBy !== "Docker Compose";
    warning.querySelector("span").textContent = editingContainer.managedBy === "Docker Compose" ? `该容器由 Compose 项目 ${editingContainer.composeProject || "未知"} 管理，重新部署项目时可能覆盖这里的设置。` : "";
    updateContainerFields();
    $("#container-dialog").showModal();
    refreshIcons($("#container-dialog"));
  } catch (error) { toast(error.message, "error"); }
  finally { if (button) button.disabled = false; }
}

async function openContainerLogs(container, button) {
  button.disabled = true;
  try {
    const data = await api(`/api/admin/containers/${encodeURIComponent(container.id)}/logs`);
    $("#container-logs-title").textContent = `${container.name} 日志`;
    $("#container-logs").textContent = data.logs || "暂无日志";
    $("#container-logs-dialog").showModal();
    $("#container-logs").scrollTop = $("#container-logs").scrollHeight;
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
}

async function loadContainers() {
  const data = await api("/api/admin/containers");
  state.containers = data.containers || [];
  const list = $("#container-list");
  list.replaceChildren();
  for (const container of state.containers) {
    const row = document.createElement("tr");
    const identity = document.createElement("td");
    identity.className = "container-identity";
    const name = document.createElement("strong");
    name.textContent = container.name;
    const metadata = document.createElement("span");
    metadata.textContent = `${container.id.slice(0, 12)}${container.managedBy ? ` · ${container.managedBy}` : ""}`;
    identity.append(name, metadata);
    const image = document.createElement("td");
    const imageName = document.createElement("code");
    imageName.textContent = container.image;
    image.append(imageName);
    const status = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = `state-badge ${container.state === "running" ? "running" : ""}`;
    badge.textContent = containerStateLabel(container);
    badge.title = container.status;
    status.append(badge);
    const resources = document.createElement("td");
    resources.className = "container-resource";
    const cpu = document.createElement("strong");
    cpu.textContent = container.cpuPercent || "-";
    const memory = document.createElement("span");
    memory.textContent = container.memoryUsage || "无实时数据";
    resources.append(cpu, memory);
    const network = document.createElement("td");
    network.textContent = container.networkIO || "-";
    const ports = document.createElement("td");
    ports.className = "container-ports";
    ports.textContent = container.ports || "-";
    const actions = document.createElement("td");
    actions.className = "container-actions-cell";
    const logs = containerActionButton("scroll-text", "查看日志");
    logs.addEventListener("click", () => openContainerLogs(container, logs));
    const edit = containerActionButton("settings-2", "修改配置");
    edit.addEventListener("click", (event) => openContainerEditor(container, event.currentTarget));
    const actionName = container.state === "running" ? "stop" : container.state === "paused" ? "unpause" : "start";
    const toggle = containerActionButton(actionName === "stop" ? "square" : "play", actionName === "stop" ? "停止" : actionName === "unpause" ? "继续" : "启动");
    toggle.addEventListener("click", () => runAdminAction(toggle, () => api(`/api/admin/containers/${encodeURIComponent(container.id)}/${actionName}`, { method: "POST" }), "容器状态已更新"));
    actions.append(logs, edit, toggle);
    if (container.state === "running") {
      const restart = containerActionButton("rotate-cw", "重启");
      restart.addEventListener("click", () => runAdminAction(restart, () => api(`/api/admin/containers/${encodeURIComponent(container.id)}/restart`, { method: "POST" }), "容器已重启"));
      actions.append(restart);
    }
    const remove = containerActionButton("trash-2", "删除容器", "danger-ghost");
    remove.addEventListener("click", async () => {
      const approved = await promptDialog({ title: `删除容器 ${container.name}`, message: "容器将被强制停止并永久删除，镜像和数据卷会保留。", confirmText: "删除", danger: true, input: false });
      if (approved) runAdminAction(remove, () => api(`/api/admin/containers/${encodeURIComponent(container.id)}`, { method: "DELETE" }), "容器已删除");
    });
    actions.append(remove);
    row.append(identity, image, status, resources, network, ports, actions);
    list.append(row);
  }
  const empty = $("#containers-empty");
  empty.hidden = state.containers.length > 0;
  empty.textContent = data.available === false ? "Docker 尚未安装" : "暂无容器";
  refreshIcons($("#admin-containers"));
}

function updateContainerFields() {
  $("#container-retry-field").hidden = $("#container-restart-policy").value !== "on-failure";
}

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#login-error").textContent = "";
  const submit = event.submitter;
  submit.disabled = true;
  try {
    const session = await api("/api/login", { method: "POST", body: { username: $("#login-user").value, password: $("#login-password").value } });
    $("#login-password").value = "";
    await enterApp(session);
  } catch (error) { $("#login-error").textContent = error.message; }
  finally { submit.disabled = false; }
});

$("#setup-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#setup-error").textContent = "";
  const submit = event.submitter;
  const password = $("#setup-password").value;
  if (password !== $("#setup-confirm").value) {
    $("#setup-error").textContent = "两次输入的密码不一致";
    return;
  }
  submit.disabled = true;
  try {
    const session = await api("/api/setup", { method: "POST", body: { username: $("#setup-user").value.trim(), password } });
    $("#setup-password").value = "";
    $("#setup-confirm").value = "";
    await enterApp(session);
  } catch (error) { $("#setup-error").textContent = error.message; }
  finally { submit.disabled = false; }
});

$("#logout-btn").addEventListener("click", async () => { try { await api("/api/logout", { method: "POST" }); } finally { showLogin(); } });
$("#account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  const newPassword = $("#account-new-password").value;
  if (newPassword !== $("#account-confirm-password").value) {
    toast("两次输入的新密码不一致", "error");
    return;
  }
  button.disabled = true;
  try {
    const session = await api("/api/admin/account", { method: "PUT", body: {
      username: $("#account-user").value.trim(),
      currentPassword: $("#account-current-password").value,
      newPassword
    } });
    state.csrf = session.csrf;
    state.user = session.user;
    $("#account-name").textContent = state.user;
    $("#login-user").value = state.user;
    $("#account-current-password").value = "";
    $("#account-new-password").value = "";
    $("#account-confirm-password").value = "";
    toast("管理员凭据已更新");
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});
$("#refresh-btn").addEventListener("click", () => loadFiles());
$("#up-btn").addEventListener("click", () => loadFiles(parentPath(state.path)));
$("#new-file-btn").addEventListener("click", () => createItem("file"));
$("#new-folder-btn").addEventListener("click", () => createItem("directory"));
$("#upload-btn").addEventListener("click", () => $("#upload-input").click());
$("#upload-input").addEventListener("change", (event) => uploadFiles([...event.target.files]));
$("#delete-btn").addEventListener("click", deleteSelected);
$("#copy-btn").addEventListener("click", () => transferSelected("copy"));
$("#move-btn").addEventListener("click", () => transferSelected("move"));
$("#archive-btn").addEventListener("click", archiveSelected);
$("#select-all").addEventListener("change", (event) => { state.selected = event.target.checked ? new Set(state.entries.map((entry) => entry.path)) : new Set(); renderFiles(); });
$("#breadcrumbs").addEventListener("click", (event) => { const button = event.target.closest("button[data-path]"); if (button) loadFiles(button.dataset.path); });
document.addEventListener("click", (event) => { const button = event.target.closest("[data-jump]"); if (button) { loadFiles(button.dataset.jump); $(".sidebar").classList.remove("open"); } });
$("#sidebar-toggle").addEventListener("click", () => $(".sidebar").classList.toggle("open"));
document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => switchView(button.dataset.view)));
document.querySelectorAll("[data-admin-tab]").forEach((button) => button.addEventListener("click", () => loadAdminTab(button.dataset.adminTab)));
$("#admin-refresh").addEventListener("click", () => loadAdminTab(state.adminTab));

$("#file-list").addEventListener("change", (event) => {
  if (event.target.matches('input[type="checkbox"]')) setSelected(event.target.closest("tr").dataset.path, event.target.checked);
});
$("#file-list").addEventListener("dblclick", (event) => {
  const row = event.target.closest("tr");
  if (!row || event.target.matches("input, button, svg")) return;
  const entry = state.entries.find((item) => item.path === row.dataset.path);
  if (entry.type === "directory") loadFiles(entry.path); else openEditor(entry);
});
$("#file-list").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  const entry = state.entries.find((item) => item.path === button.closest("tr").dataset.path);
  if (button.dataset.action === "open") loadFiles(entry.path);
  if (button.dataset.action === "edit") openEditor(entry);
  if (button.dataset.action === "download") location.href = `/api/download?path=${encodeURIComponent(entry.path)}`;
  if (button.dataset.action === "more") { event.stopPropagation(); openEntryMenu(entry, button); }
});

document.addEventListener("click", (event) => { if (!event.target.closest("#entry-menu")) closeEntryMenu(); });
window.addEventListener("resize", closeEntryMenu);
document.addEventListener("scroll", closeEntryMenu, true);

$("#editor-close").addEventListener("click", () => $("#editor-dialog").close());
$("#editor-save").addEventListener("click", saveEditor);
$("#editor-content").addEventListener("input", updateEditorStats);
$("#ssh-form").addEventListener("submit", connectSsh);
$("#ssh-key-btn").addEventListener("click", () => $("#ssh-key-file").click());
$("#ssh-key-file").addEventListener("change", async (event) => {
  const file = event.target.files[0];
  sshPrivateKey = file ? await file.text() : "";
  $("#ssh-key-btn").classList.toggle("has-key", Boolean(sshPrivateKey));
  $("#ssh-key-btn span").textContent = file ? file.name : "私钥";
});
$("#ssh-disconnect").addEventListener("click", () => sshSocket?.close());
$("#terminal-clear").addEventListener("click", () => terminal?.clear());

$("#nginx-settings-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  runAdminAction(button, () => api("/api/admin/nginx/settings", { method: "PUT", body: {
    clientMaxBodySize: $("#nginx-body-size").value.trim(),
    keepaliveTimeout: Number($("#nginx-keepalive").value),
    gzip: $("#nginx-gzip").checked,
    serverTokens: $("#nginx-tokens").checked
  } }), "Nginx 设置已保存");
});

$("#new-site-btn").addEventListener("click", () => {
  editingSite = null;
  $("#site-form").reset();
  $("#site-dialog-title").textContent = "添加网站";
  $("#site-submit").textContent = "创建网站";
  $("#site-domain").disabled = false;
  $("#site-type").value = "static";
  $("#site-rewrite-mode").value = "laravel";
  updateSiteFields();
  $("#site-dialog").showModal();
  $("#site-domain").focus();
});
$("#site-type").addEventListener("change", updateSiteFields);
$("#site-rewrite-mode").addEventListener("change", updateSiteFields);
$("#site-ssl").addEventListener("change", updateSiteFields);
$("#site-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const submit = event.submitter;
  submit.disabled = true;
  try {
    const type = $("#site-type").value;
    const body = {
      domain: $("#site-domain").value.trim(),
      aliases: $("#site-aliases").value.split(",").map((value) => value.trim()).filter(Boolean),
      type,
      root: $("#site-root").value.trim(),
      upstream: $("#site-upstream").value.trim(),
      rewriteMode: type === "php" ? $("#site-rewrite-mode").value : "",
      rewriteRules: type === "php" && $("#site-rewrite-mode").value === "custom" ? $("#site-rewrite-rules").value.trim() : "",
      enabled: editingSite ? editingSite.enabled : true,
      ssl: $("#site-ssl").checked,
      certificate: $("#site-cert").value.trim(),
      privateKey: $("#site-key").value.trim()
    };
    await api(editingSite ? `/api/admin/sites/${editingSite.id}` : "/api/admin/sites", { method: editingSite ? "PUT" : "POST", body });
    $("#site-dialog").close();
    toast(editingSite ? "网站配置已更新" : "网站已创建");
    editingSite = null;
    await loadSites();
  } catch (error) { toast(error.message, "error"); }
  finally { submit.disabled = false; }
});

$("#new-certificate-btn").addEventListener("click", () => {
  if (!state.sites.length) { toast("请先添加网站", "error"); return; }
  $("#certificate-form").reset();
  $("#certificate-auto-renew").checked = true;
  $("#certificate-email").value = state.dnsSettings.defaultEmail || "";
  populateCertificateSites();
  updateCertificateDomains();
  updateCertificateChallenge();
  $("#certificate-dialog").showModal();
});
$("#certificate-site").addEventListener("change", updateCertificateDomains);
document.querySelectorAll('input[name="certificate-challenge"]').forEach((input) => input.addEventListener("change", updateCertificateChallenge));
$("#certificate-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  const challengeType = document.querySelector('input[name="certificate-challenge"]:checked').value;
  const challenge = challengeType === "dns" ? `dns-${$("#certificate-provider").value}` : "http";
  button.disabled = true;
  try {
    await api("/api/admin/certificates", { method: "POST", body: {
      siteId: $("#certificate-site").value,
      domains: $("#certificate-domains").value.split(",").map((value) => value.trim()).filter(Boolean),
      email: $("#certificate-email").value.trim(),
      challenge,
      autoRenew: $("#certificate-auto-renew").checked
    } });
    $("#certificate-dialog").close();
    toast("证书申请完成");
    await loadCertificates();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

$("#dns-settings-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  button.disabled = true;
  try {
    const settings = await api("/api/admin/dns-settings", { method: "PUT", body: {
      defaultEmail: $("#dns-default-email").value.trim(),
      cloudflareToken: $("#dns-cloudflare-token").value.trim(),
      clearCloudflare: $("#clear-cloudflare").checked
    } });
    renderDNSSettings(settings);
    toast("DNS 设置已保存");
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

for (const [input, clear] of [
  ["#dns-cloudflare-token", "#clear-cloudflare"],
]) {
  $(input).addEventListener("input", () => {
    if ($(input).value) $(clear).checked = false;
  });
}

$("#php-settings-form").addEventListener("submit", (event) => {
  event.preventDefault();
  runAdminAction(event.submitter, () => api("/api/admin/php/settings", { method: "PUT", body: {
    uploadMaxFilesize: $("#php-upload").value.trim(),
    postMaxSize: $("#php-post").value.trim(),
    memoryLimit: $("#php-memory").value.trim(),
    maxExecutionTime: Number($("#php-time").value),
    displayErrors: $("#php-errors").checked
  } }), "PHP 设置已保存");
});

$("#new-db-btn").addEventListener("click", () => { $("#database-form").reset(); $("#database-dialog").showModal(); $("#database-name").focus(); });
$("#database-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter; button.disabled = true;
  try {
    await api("/api/admin/databases", { method: "POST", body: { name: $("#database-name").value.trim(), charset: $("#database-charset").value } });
    $("#database-dialog").close(); toast("数据库已创建"); await loadDatabases();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

$("#new-db-user-btn").addEventListener("click", () => {
  if (!state.databases.length) { toast("请先创建数据库", "error"); return; }
  $("#db-user-form").reset(); $("#db-user-host").value = "localhost"; $("#db-user-dialog").showModal(); $("#db-user-name").focus();
});
$("#db-user-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter; button.disabled = true;
  const privileges = [...$("#db-user-privileges").selectedOptions].map((option) => option.value);
  try {
    await api("/api/admin/database-users", { method: "POST", body: {
      user: $("#db-user-name").value.trim(), host: $("#db-user-host").value.trim(), password: $("#db-user-password").value,
      database: $("#db-user-database").value, privileges
    } });
    $("#db-user-dialog").close(); toast("数据库用户已创建"); await loadDatabases();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

$("#ftp-install-btn").addEventListener("click", (event) => runAdminAction(event.currentTarget, () => api("/api/admin/components/ftp/install", { method: "POST" }), "FTP 服务安装完成"));
$("#ftp-restart-btn").addEventListener("click", (event) => {
  const action = event.currentTarget.dataset.action || "restart";
  runAdminAction(event.currentTarget, () => api(`/api/admin/services/vsftpd/${action}`, { method: "POST" }), "FTP 服务状态已更新");
});
$("#new-ftp-user-btn").addEventListener("click", () => openFTPUserDialog());
$("#ftp-user-name").addEventListener("input", () => {
  if (!editingFTPUser) $("#ftp-user-home").textContent = `/srv/ftp/${$("#ftp-user-name").value.trim() || "<用户名>"}`;
});
$("#ftp-user-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  const password = $("#ftp-user-password").value;
  if (password !== $("#ftp-user-confirm-password").value) {
    toast("两次输入的 FTP 密码不一致", "error");
    return;
  }
  button.disabled = true;
  try {
    const username = editingFTPUser?.username || $("#ftp-user-name").value.trim();
    await api(editingFTPUser ? `/api/admin/ftp/users/${encodeURIComponent(username)}` : "/api/admin/ftp/users", {
      method: editingFTPUser ? "PUT" : "POST",
      body: editingFTPUser ? { password } : { username, password }
    });
    $("#ftp-user-dialog").close();
    toast(editingFTPUser ? "FTP 密码已更新" : "FTP 用户已创建");
    editingFTPUser = null;
    await loadFTP();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

$("#container-restart-policy").addEventListener("change", updateContainerFields);
$("#container-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!editingContainer) return;
  const button = event.submitter;
  button.disabled = true;
  try {
    await api(`/api/admin/containers/${encodeURIComponent(editingContainer.id)}`, { method: "PUT", body: {
      name: $("#container-name").value.trim(),
      restartPolicy: $("#container-restart-policy").value,
      maximumRetryCount: Number($("#container-retry").value) || 0,
      cpus: Number($("#container-cpus").value) || 0,
      memoryMb: Number($("#container-memory").value) || 0
    } });
    $("#container-dialog").close();
    editingContainer = null;
    toast("容器配置已更新");
    await loadContainers();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

$("#server-settings-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  button.disabled = true;
  try {
    await api("/api/admin/server-settings", { method: "PUT", body: {
      hostname: $("#server-hostname").value.trim(),
      timezone: $("#server-timezone").value
    } });
    toast("服务器设置已保存");
    await loadServerSettings();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

$("#new-swap-btn").addEventListener("click", () => {
  $("#swap-form").reset();
  $("#swap-size").value = 2048;
  $("#swap-dialog").showModal();
  $("#swap-size").focus();
});

$("#swap-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.submitter;
  button.disabled = true;
  try {
    await api("/api/admin/server-settings/swap", { method: "POST", body: { sizeMb: Number($("#swap-size").value) } });
    $("#swap-dialog").close();
    toast("交换空间已创建并启用");
    await loadServerSettings();
  } catch (error) { toast(error.message, "error"); }
  finally { button.disabled = false; }
});

document.querySelectorAll(".dialog-x, .dialog-cancel").forEach((button) => button.addEventListener("click", () => button.closest("dialog").close()));

refreshIcons();
api("/api/setup")
  .then((setup) => setup.required ? showSetup() : api("/api/session").then(enterApp).catch(showLogin))
  .catch(showLogin);
