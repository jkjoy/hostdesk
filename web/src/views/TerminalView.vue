<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from "vue";
import { ClipboardPaste, Copy, Eraser, Plug, RotateCw, TextSelect, Unplug } from "@lucide/vue";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { getCSRFToken } from "../api";
import { useUI } from "../ui";
import PageHeader from "../components/PageHeader.vue";

const { notify } = useUI();
const container = ref<HTMLElement>();
const connected = ref(false);
const status = ref("未连接");
const hasSelection = ref(false);
const contextMenu = ref<{ x: number; y: number } | null>(null);
const contextMenuElement = ref<HTMLElement>();
let terminal: Terminal | null = null;
let fit: FitAddon | null = null;
let socket: WebSocket | null = null;
let observer: ResizeObserver | null = null;
let ansiObserver: MutationObserver | null = null;
let resizeFrame = 0;

const ansiColors = [
  "#4b5550", "#ef7777", "#49c99a", "#e4bd65", "#71a7ec", "#c792ea", "#56c7d9", "#d9e1de",
  "#75817b", "#ff9292", "#67dfae", "#f1d37f", "#91bcf3", "#d7a8f3", "#7adce8", "#f4f7f6",
];
const colorSteps = [0x00, 0x5f, 0x87, 0xaf, 0xd7, 0xff];
for (let index = 0; index < 216; index++) {
  const red = colorSteps[Math.floor(index / 36) % 6];
  const green = colorSteps[Math.floor(index / 6) % 6];
  const blue = colorSteps[index % 6];
  ansiColors.push(`#${red.toString(16).padStart(2, "0")}${green.toString(16).padStart(2, "0")}${blue.toString(16).padStart(2, "0")}`);
}
for (let index = 0; index < 24; index++) {
  const value = (8 + index * 10).toString(16).padStart(2, "0");
  ansiColors.push(`#${value}${value}${value}`);
}

function applyAnsiColors(element: Element) {
  if (!(element instanceof HTMLElement)) return;
  let foreground = "";
  let background = "";
  for (const name of element.classList) {
    const match = /^xterm-(fg|bg)-(\d+)$/.exec(name);
    if (!match) continue;
    const index = Number(match[2]);
    const value = index === 257 ? (match[1] === "fg" ? "#151918" : "#d9e1de") : ansiColors[index];
    if (match[1] === "fg") foreground = value || "";
    if (match[1] === "bg") background = value || "";
  }
  foreground ? element.style.setProperty("--terminal-ansi-fg", foreground) : element.style.removeProperty("--terminal-ansi-fg");
  background ? element.style.setProperty("--terminal-ansi-bg", background) : element.style.removeProperty("--terminal-ansi-bg");
}

function syncAnsiColors(node: Node) {
  if (!(node instanceof Element)) return;
  applyAnsiColors(node);
  node.querySelectorAll(".xterm-rows span").forEach(applyAnsiColors);
}

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

function closeContextMenu() {
  contextMenu.value = null;
}

function fallbackCopy(text: string) {
  const helper = document.createElement("textarea");
  helper.value = text;
  helper.setAttribute("readonly", "");
  helper.style.position = "fixed";
  helper.style.inset = "-9999px auto auto -9999px";
  document.body.appendChild(helper);
  helper.select();
  let copied = false;
  try { copied = document.execCommand("copy"); } catch { copied = false; }
  helper.remove();
  return copied;
}

async function copySelection(showNotice = true) {
  const text = terminal?.getSelection() || "";
  closeContextMenu();
  if (!text) {
    notify("请先选择终端内容", "error");
    terminal?.focus();
    return;
  }
  let copied = false;
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      copied = true;
    }
  } catch {
    copied = false;
  }
  if (!copied) copied = fallbackCopy(text);
  if (copied) {
    if (showNotice) notify("终端内容已复制");
  } else {
    notify("浏览器未允许写入剪贴板", "error");
  }
  terminal?.focus();
}

async function pasteClipboard() {
  closeContextMenu();
  try {
    const text = await navigator.clipboard.readText();
    if (text) terminal?.paste(text);
    else notify("剪贴板中没有文本", "error");
  } catch {
    notify("浏览器未允许读取剪贴板，请使用 Ctrl+V", "error");
  }
  terminal?.focus();
}

function selectAll() {
  closeContextMenu();
  terminal?.selectAll();
  terminal?.focus();
}

async function openContextMenu(event: MouseEvent) {
  contextMenu.value = { x: event.clientX, y: event.clientY };
  await nextTick();
  const menu = contextMenuElement.value;
  if (!menu || !contextMenu.value) return;
  const bounds = menu.getBoundingClientRect();
  contextMenu.value.x = Math.max(8, Math.min(event.clientX, window.innerWidth - bounds.width - 8));
  contextMenu.value.y = Math.max(8, Math.min(event.clientY, window.innerHeight - bounds.height - 8));
}

function init() {
  if (terminal || !container.value) return;
  const instance = new Terminal({
    cursorBlink: true,
    fontFamily: '"SFMono-Regular", Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.25,
    scrollback: 5000,
    theme: {
      background: "#151918", foreground: "#d9e1de", cursor: "#49c99a", cursorAccent: "#151918",
      selectionBackground: "#315f50", selectionForeground: "#ffffff", black: ansiColors[0], red: ansiColors[1], green: ansiColors[2],
      yellow: ansiColors[3], blue: ansiColors[4], magenta: ansiColors[5], cyan: ansiColors[6], white: ansiColors[7],
      brightBlack: ansiColors[8], brightRed: ansiColors[9], brightGreen: ansiColors[10], brightYellow: ansiColors[11],
      brightBlue: ansiColors[12], brightMagenta: ansiColors[13], brightCyan: ansiColors[14], brightWhite: ansiColors[15],
      extendedAnsi: ansiColors.slice(16),
    },
  });
  const addon = new FitAddon();
  terminal = instance;
  fit = addon;
  instance.loadAddon(addon);
  instance.open(container.value);
  instance.attachCustomKeyEventHandler(event => {
    if (event.type !== "keydown") return true;
    const modifier = event.ctrlKey || event.metaKey;
    const key = event.key.toLowerCase();
    if (modifier && key === "c" && (instance.hasSelection() || event.shiftKey)) {
      event.preventDefault();
      void copySelection(false);
      return false;
    }
    if ((modifier && key === "v") || (event.shiftKey && event.key === "Insert")) {
      closeContextMenu();
      return false;
    }
    return true;
  });
  instance.onSelectionChange(() => { hasSelection.value = instance.hasSelection(); });
  ansiObserver = new MutationObserver(records => {
    for (const record of records) {
      if (record.type === "attributes") applyAnsiColors(record.target as Element);
      record.addedNodes.forEach(syncAnsiColors);
    }
  });
  ansiObserver.observe(container.value, { subtree: true, childList: true, attributes: true, attributeFilter: ["class"] });
  syncAnsiColors(container.value);
  instance.writeln("\x1b[1;38;5;10mHostDesk 本机终端\x1b[0m\r\n\x1b[38;5;8m正在连接...\x1b[0m");
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
  terminal.writeln("\x1b[38;5;8m正在启动本机终端...\x1b[0m");

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
      resize(true);
    }
    if (message.type === "error") {
      terminal?.writeln(`\r\n\x1b[1;38;5;9m${message.message || "终端连接失败"}\x1b[0m`);
      connected.value = false;
      status.value = "连接失败";
      resize(true);
    }
    if (message.type === "close") disconnect(false);
  };
  connection.onerror = () => {
    if (connection !== socket) return;
    terminal?.writeln("\r\n\x1b[1;38;5;9mWebSocket 连接失败\x1b[0m");
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

onMounted(async () => {
  window.addEventListener("click", closeContextMenu);
  window.addEventListener("resize", closeContextMenu);
  window.addEventListener("scroll", closeContextMenu, true);
  await nextTick();
  init();
  connect();
});
onBeforeUnmount(() => {
  window.removeEventListener("click", closeContextMenu);
  window.removeEventListener("resize", closeContextMenu);
  window.removeEventListener("scroll", closeContextMenu, true);
  window.cancelAnimationFrame(resizeFrame);
  socket?.close();
  observer?.disconnect();
  ansiObserver?.disconnect();
  terminal?.dispose();
});
</script>

<template>
  <div class="terminal-page">
    <PageHeader title="终端" subtitle="服务器本机 Shell" kicker="TERMINAL">
      <span class="connection-state"><i :class="{ connected }"></i>{{ status }}</span>
      <button v-if="!connected" class="button primary" type="button" @click="connect"><RotateCw :size="16" />重新连接</button>
      <button v-else class="button" type="button" @click="disconnect()"><Unplug :size="16" />断开</button>
    </PageHeader>
    <div class="terminal-shell local-terminal">
      <div class="terminal-head">
        <span>{{ status }}</span>
        <div class="terminal-head-actions">
          <button v-if="!connected" class="icon-button dark" type="button" title="连接" @click="connect"><Plug :size="16" /></button>
          <button class="icon-button dark" type="button" title="复制选中内容" :disabled="!hasSelection" @pointerdown.prevent @click="copySelection()"><Copy :size="16" /></button>
          <button class="icon-button dark" type="button" title="粘贴" :disabled="!connected" @pointerdown.prevent @click="pasteClipboard"><ClipboardPaste :size="16" /></button>
          <button class="icon-button dark" type="button" title="清屏" @click="terminal?.clear()"><Eraser :size="16" /></button>
        </div>
      </div>
      <div class="terminal-stage">
        <div ref="container" class="terminal" role="application" aria-label="本机终端" @pointerdown="terminal?.focus()" @contextmenu.stop.prevent="openContextMenu"></div>
      </div>
    </div>
    <Teleport to="body"><div v-if="contextMenu" ref="contextMenuElement" class="terminal-context-menu" :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }" role="menu" @click.stop @contextmenu.prevent>
      <button type="button" role="menuitem" :disabled="!hasSelection" @click="copySelection()"><Copy :size="16" /><span>复制</span><kbd>Ctrl+C</kbd></button>
      <button type="button" role="menuitem" :disabled="!connected" @click="pasteClipboard"><ClipboardPaste :size="16" /><span>粘贴</span><kbd>Ctrl+V</kbd></button>
      <div></div>
      <button type="button" role="menuitem" @click="selectAll"><TextSelect :size="16" /><span>全选</span></button>
    </div></Teleport>
  </div>
</template>
