<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from "vue";
import { CornerDownLeft, Eraser, KeyRound, Plug, Trash2, Unplug } from "@lucide/vue";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { api } from "../api";
import { errorMessage, useUI } from "../ui";
import PageHeader from "../components/PageHeader.vue";

interface SSHSettings { host: string; port: number; username: string; passwordConfigured: boolean }

const { notify, confirm } = useUI();
const container = ref<HTMLElement>();
const keyInput = ref<HTMLInputElement>();
const commandInput = ref<HTMLInputElement>();
const connected = ref(false);
const status = ref("未连接");
const form = reactive({ host: "127.0.0.1", port: 22, username: "", password: "", privateKey: "" });
const command = ref("");
const remember = ref(true);
const settings = ref<SSHSettings | null>(null);
const canUseSavedCredential = computed(() => !!settings.value?.passwordConfigured && !form.password && !form.privateKey && settings.value.host === form.host.trim() && settings.value.port === form.port && settings.value.username === form.username.trim());
let terminal: Terminal | null = null;
let fit: FitAddon | null = null;
let socket: WebSocket | null = null;
let observer: ResizeObserver | null = null;
let resizeFrame = 0;

function sendSize() {
  const connection = socket;
  const currentTerminal = terminal;
  if (connection?.readyState === WebSocket.OPEN && currentTerminal && currentTerminal.cols > 0 && currentTerminal.rows > 0) {
    connection.send(JSON.stringify({ type: "resize", cols: currentTerminal.cols, rows: currentTerminal.rows }));
  }
}

function resize(focus = false) {
  const currentFit = fit;
  const currentTerminal = terminal;
  if (!container.value || !currentFit || !currentTerminal) return;
  window.cancelAnimationFrame(resizeFrame);
  resizeFrame = window.requestAnimationFrame(() => {
    try {
      currentFit.fit();
      sendSize();
      if (focus) currentTerminal.focus();
    } catch {
      // The terminal may be unmounting while a resize frame is pending.
    }
  });
}

function focusTerminal() {
  terminal?.focus();
}

function init() {
  if (terminal || !container.value) return;
  const instance = new Terminal({
    cursorBlink: true,
    fontFamily: '"SFMono-Regular", Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.25,
    scrollback: 5000,
    theme: { background: "#151918", foreground: "#d9e1de", cursor: "#49c99a", green: "#49c99a", red: "#ef7777" },
  });
  const addon = new FitAddon();
  terminal = instance;
  fit = addon;
  instance.loadAddon(addon);
  instance.open(container.value);
  instance.writeln("\x1b[38;5;72mHostDesk WebSSH\x1b[0m\r\n\x1b[38;5;245m等待连接...\x1b[0m");
  instance.onData((data: string) => {
    if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: "input", data }));
  });
  instance.onResize(sendSize);
  observer = new ResizeObserver(() => resize());
  observer.observe(container.value);
  resize();
  void document.fonts?.ready.then(() => resize());
}

function connect() {
  init();
  const currentTerminal = terminal;
  const currentFit = fit;
  if (!currentTerminal || !currentFit) return;
  try { currentFit.fit(); } catch { /* A resize frame will retry after layout. */ }
  socket?.close();
  const protocol = location.protocol === "https:" ? "wss" : "ws";
  const connection = new WebSocket(`${protocol}://${location.host}/ws/ssh`);
  socket = connection;
  connected.value = false;
  status.value = "连接中";
  currentTerminal.reset();
  currentTerminal.writeln("\x1b[38;5;245m正在建立 SSH 连接...\x1b[0m");

  connection.onopen = () => connection.send(JSON.stringify({
    type: "connect",
    ...form,
    cols: currentTerminal.cols,
    rows: currentTerminal.rows,
    useSavedCredential: canUseSavedCredential.value,
  }));
  connection.onmessage = ({ data }) => {
    if (connection !== socket) return;
    let message: { type: string; data?: string; message?: string };
    try { message = JSON.parse(data); } catch { return; }
    if (message.type === "data" && message.data) {
      currentTerminal.write(message.data);
    }
    if (message.type === "ready") {
      connected.value = true;
      status.value = "已连接";
      currentTerminal.writeln("\r\n\x1b[32mSSH 连接成功\x1b[0m");
      connection.send(JSON.stringify({ type: "input", data: "\r" }));
      currentTerminal.scrollToBottom();
      resize();
      void persistConnection();
      void nextTick(() => commandInput.value?.focus());
    }
    if (message.type === "error") {
      currentTerminal.writeln(`\r\n\x1b[31m${message.message || "SSH 连接失败"}\x1b[0m`);
      connected.value = false;
      status.value = "连接失败";
      resize(true);
    }
    if (message.type === "close") disconnect(false);
  };
  connection.onerror = () => {
    if (connection !== socket) return;
    currentTerminal.writeln("\r\n\x1b[31mWebSocket 连接失败\x1b[0m");
    connected.value = false;
    status.value = "连接失败";
  };
  connection.onclose = () => {
    if (connection !== socket) return;
    socket = null;
    connected.value = false;
    if (status.value === "已连接" || status.value === "连接中") status.value = "连接已关闭";
  };
}

function submitCommand() {
  if (!connected.value || socket?.readyState !== WebSocket.OPEN) return;
  socket.send(JSON.stringify({ type: "input", data: `${command.value}\r` }));
  command.value = "";
}

async function loadSettings() {
  try {
    const saved = await api<SSHSettings>("/api/admin/ssh-settings");
    settings.value = saved.host ? saved : null;
    if (saved.host) Object.assign(form, { host: saved.host, port: saved.port || 22, username: saved.username, password: "", privateKey: "" });
  } catch (error) {
    notify(errorMessage(error), "error");
  }
}

async function persistConnection() {
  if (!remember.value) return;
  try {
    settings.value = await api<SSHSettings>("/api/admin/ssh-settings", {
      method: "PUT",
      body: { host: form.host, port: form.port, username: form.username, password: form.password },
    });
    form.password = "";
    notify("SSH 连接已加密保存");
  } catch (error) {
    notify(errorMessage(error), "error");
  }
}

async function removeSavedConnection() {
  if (!await confirm("删除已保存连接", "将清除数据库中的 SSH 主机、用户名和加密密码。", "删除", true)) return;
  try {
    await api<SSHSettings>("/api/admin/ssh-settings", { method: "DELETE" });
    settings.value = null;
    form.password = "";
    notify("已保存的 SSH 连接已删除");
  } catch (error) {
    notify(errorMessage(error), "error");
  }
}

function disconnect(close = true) {
  const connection = socket;
  socket = null;
  if (close) connection?.close();
  connected.value = false;
  status.value = "未连接";
}

function readKey(files: FileList | null) {
  const file = files?.[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = () => {
    form.privateKey = String(reader.result || "");
    notify("SSH 私钥已载入");
  };
  reader.readAsText(file);
}

onMounted(async () => {
  await nextTick();
  init();
  await loadSettings();
});
onBeforeUnmount(() => {
  window.cancelAnimationFrame(resizeFrame);
  socket?.close();
  observer?.disconnect();
  terminal?.dispose();
});
</script>

<template>
  <PageHeader title="终端" subtitle="WebSSH 连接" kicker="TERMINAL">
    <span class="connection-state"><i :class="{ connected }"></i>{{ status }}</span>
  </PageHeader>
  <form class="connection-bar" @submit.prevent="connect">
    <label class="field"><span>主机</span><input v-model="form.host" required></label>
    <label class="field port"><span>端口</span><input v-model.number="form.port" type="number" min="1" max="65535" required></label>
    <label class="field"><span>用户</span><input v-model="form.username" required autocomplete="username"></label>
    <label class="field"><span>密码</span><input v-model="form.password" type="password" autocomplete="current-password"></label>
    <button class="button" type="button" @click="keyInput?.click()"><KeyRound :size="16" />私钥</button>
    <input ref="keyInput" hidden type="file" @change="readKey(($event.target as HTMLInputElement).files)">
    <button v-if="!connected" class="button primary" type="submit"><Plug :size="16" />连接</button>
    <button v-else class="button" type="button" @click="disconnect()"><Unplug :size="16" />断开</button>
    <div class="connection-persist">
      <label><input v-model="remember" type="checkbox">保存用户名和加密密码</label>
      <span v-if="settings?.passwordConfigured"><KeyRound :size="14" />已保存 {{ settings.username }}@{{ settings.host }}:{{ settings.port }}</span>
      <button v-if="settings" class="icon-button danger" type="button" title="删除已保存连接" @click="removeSavedConnection"><Trash2 :size="15" /></button>
    </div>
  </form>
  <div class="terminal-shell" :class="{ 'has-command': connected }">
    <div class="terminal-head">
      <span>{{ status }}</span>
      <div class="terminal-head-actions">
        <button class="icon-button dark" type="button" title="清屏" @click="terminal?.clear()"><Eraser :size="16" /></button>
      </div>
    </div>
    <div class="terminal-stage">
      <div ref="container" class="terminal" role="application" aria-label="SSH 终端" @pointerdown="focusTerminal"></div>
    </div>
    <form v-if="connected" class="terminal-command" @submit.prevent="submitCommand">
      <span>$</span><input ref="commandInput" v-model="command" autocomplete="off" spellcheck="false" aria-label="终端命令"><button class="icon-button dark" type="submit" title="执行命令"><CornerDownLeft :size="17" /></button>
    </form>
  </div>
</template>
