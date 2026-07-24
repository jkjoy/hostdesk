<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from "vue";
import { CornerDownLeft, Eraser, Plug, RotateCw, Unplug } from "@lucide/vue";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { getCSRFToken } from "../api";
import PageHeader from "../components/PageHeader.vue";

const container = ref<HTMLElement>();
const connected = ref(false);
const status = ref("未连接");
const command = ref("");
let terminal: Terminal | null = null;
let fit: FitAddon | null = null;
let socket: WebSocket | null = null;
let observer: ResizeObserver | null = null;
let resizeFrame = 0;

function sendSize() {
  if (socket?.readyState === WebSocket.OPEN && terminal && terminal.cols > 0 && terminal.rows > 0) {
    socket.send(JSON.stringify({ type: "resize", cols: terminal.cols, rows: terminal.rows }));
  }
}

function resize(focus = false) {
  if (!container.value || !fit || !terminal) return;
  window.cancelAnimationFrame(resizeFrame);
  resizeFrame = window.requestAnimationFrame(() => {
    try {
      fit?.fit();
      sendSize();
      if (focus) terminal?.focus();
    } catch {
      // The terminal may be unmounting while a resize frame is pending.
    }
  });
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
  instance.writeln("\x1b[38;5;72mHostDesk 本机终端\x1b[0m\r\n\x1b[38;5;245m正在连接...\x1b[0m");
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
  if (!terminal || !fit) return;
  try { fit.fit(); } catch { /* A resize frame will retry after layout. */ }
  socket?.close();
  const protocol = location.protocol === "https:" ? "wss" : "ws";
  const connection = new WebSocket(`${protocol}://${location.host}/ws/terminal`);
  socket = connection;
  connected.value = false;
  status.value = "连接中";
  terminal.reset();
  terminal.writeln("\x1b[38;5;245m正在启动本机终端...\x1b[0m");

  connection.onopen = () => connection.send(JSON.stringify({
    type: "connect",
    csrf: getCSRFToken(),
    cols: terminal?.cols || 100,
    rows: terminal?.rows || 30,
  }));
  connection.onmessage = ({ data }) => {
    if (connection !== socket) return;
    let message: { type: string; data?: string; message?: string };
    try { message = JSON.parse(data); } catch { return; }
    if (message.type === "data" && message.data) terminal?.write(message.data);
    if (message.type === "ready") {
      connected.value = true;
      status.value = "已连接本机";
      resize();
    }
    if (message.type === "error") {
      terminal?.writeln(`\r\n\x1b[31m${message.message || "终端连接失败"}\x1b[0m`);
      connected.value = false;
      status.value = "连接失败";
      resize(true);
    }
    if (message.type === "close") disconnect(false);
  };
  connection.onerror = () => {
    if (connection !== socket) return;
    terminal?.writeln("\r\n\x1b[31mWebSocket 连接失败\x1b[0m");
    connected.value = false;
    status.value = "连接失败";
  };
  connection.onclose = () => {
    if (connection !== socket) return;
    socket = null;
    connected.value = false;
    if (status.value === "已连接本机" || status.value === "连接中") status.value = "连接已关闭";
  };
}

function disconnect(close = true) {
  const connection = socket;
  socket = null;
  if (close) connection?.close();
  connected.value = false;
  status.value = "未连接";
}

function submitCommand() {
  if (!connected.value || socket?.readyState !== WebSocket.OPEN) return;
  socket.send(JSON.stringify({ type: "input", data: `${command.value}\r` }));
  command.value = "";
}

onMounted(async () => {
  await nextTick();
  init();
  connect();
});
onBeforeUnmount(() => {
  window.cancelAnimationFrame(resizeFrame);
  socket?.close();
  observer?.disconnect();
  terminal?.dispose();
});
</script>

<template>
  <PageHeader title="终端" subtitle="服务器本机 Shell" kicker="TERMINAL">
    <span class="connection-state"><i :class="{ connected }"></i>{{ status }}</span>
    <button v-if="!connected" class="button primary" type="button" @click="connect"><RotateCw :size="16" />重新连接</button>
    <button v-else class="button" type="button" @click="disconnect()"><Unplug :size="16" />断开</button>
  </PageHeader>
  <div class="terminal-shell local-terminal" :class="{ 'has-command': connected }">
    <div class="terminal-head">
      <span>{{ status }}</span>
      <div class="terminal-head-actions">
        <button v-if="!connected" class="icon-button dark" type="button" title="连接" @click="connect"><Plug :size="16" /></button>
        <button class="icon-button dark" type="button" title="清屏" @click="terminal?.clear()"><Eraser :size="16" /></button>
      </div>
    </div>
    <div class="terminal-stage">
      <div ref="container" class="terminal" role="application" aria-label="本机终端" @pointerdown="terminal?.focus()"></div>
    </div>
    <form v-if="connected" class="terminal-command" @submit.prevent="submitCommand">
      <span>#</span><input v-model="command" autocomplete="off" spellcheck="false" aria-label="终端命令"><button class="icon-button dark" type="submit" title="执行命令"><CornerDownLeft :size="17" /></button>
    </form>
  </div>
</template>
