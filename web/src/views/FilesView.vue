<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { Archive, ChevronRight, ClipboardPaste, CloudDownload, Copy, Download, Ellipsis, File, FileCode, FilePlus, Folder, FolderClock, FolderOpen, FolderPlus, Pencil, RefreshCw, Save, Scissors, Shield, Trash2, Upload } from "@lucide/vue";
import { api } from "../api";
import { errorMessage, formatBytes, displayDate, useUI } from "../ui";
import AppModal from "../components/AppModal.vue";
import EmptyState from "../components/EmptyState.vue";
import PageHeader from "../components/PageHeader.vue";

interface Entry { name: string; path: string; type: "directory" | "file" | "link"; size: number; modified: string; mode: string; owner: string; group: string }
const props = defineProps<{ initialPath?: string }>();
const emit = defineEmits<{ pathChanged: [path: string] }>();
const { notify, confirm } = useUI();
const path = ref(props.initialPath || "");
const entries = ref<Entry[]>([]);
const selected = ref(new Set<string>());
const loading = ref(false);
const uploading = ref(false);
const upload = ref<HTMLInputElement>();
const draggingFiles = ref(false);
const recent = ref<string[]>([]);
const addressMode = ref(false);
const addressValue = ref("/");
const addressInput = ref<HTMLInputElement>();
const fileClipboard = ref<{ mode: "copy" | "move"; paths: string[] } | null>(null);
const contextMenu = ref<{ x: number; y: number; entry: Entry | null } | null>(null);
const contextMenuElement = ref<HTMLElement>();
const remoteModal = ref(false);
const remoteBusy = ref(false);
const remoteForm = ref({ url: "", name: "" });
const permissionModal = ref(false);
const permissionBusy = ref(false);
const permissionEntry = ref<Entry | null>(null);
const permissionForm = ref({ owner: "", group: "", mode: "", recursive: false });
const identityOptions = ref<{ users: string[]; groups: string[] }>({ users: [], groups: [] });
const editor = ref<{ path: string; name: string; content: string } | null>(null);
const prompt = ref<{ title: string; label: string; value: string; action: (value: string) => Promise<void> } | null>(null);
const promptBusy = ref(false);
const breadcrumbs = computed(() => ["", ...path.value.split("/").filter(Boolean)].map((part, index, all) => ({ label: index ? part : "根目录", path: all.slice(1, index + 1).join("/") })));
let dragDepth = 0;

function join(...parts: string[]) { return parts.filter(Boolean).join("/").replace(/\/+/g, "/"); }
function parent(value: string) { const parts = value.split("/").filter(Boolean); parts.pop(); return parts.join("/"); }
function selectedEntry(entry: Entry) { return selected.value.has(entry.path); }
function toggle(entry: Entry) { const next = new Set(selected.value); next.has(entry.path) ? next.delete(entry.path) : next.add(entry.path); selected.value = next; }
function fullPath(value = path.value) { return value ? `/${value}` : "/"; }
function isCut(entry: Entry) { return fileClipboard.value?.mode === "move" && fileClipboard.value.paths.includes(entry.path); }
function contextPaths(entry: Entry) { return selected.value.has(entry.path) ? [...selected.value] : [entry.path]; }

async function showAddress() {
  addressValue.value = fullPath();
  addressMode.value = true;
  await nextTick();
  addressInput.value?.focus();
  addressInput.value?.select();
}

async function openAddress() {
  const next = addressValue.value.trim().replace(/\\/g, "/").replace(/^\/+|\/+$/g, "");
  await load(next);
  addressMode.value = false;
}

function leaveAddress(event: FocusEvent) {
  const next = event.relatedTarget;
  if (!(next instanceof Node) || !(event.currentTarget as HTMLElement).contains(next)) addressMode.value = false;
}

async function copyAddress() {
  try {
    await navigator.clipboard.writeText(fullPath());
    notify("路径已复制");
    addressInput.value?.focus();
    addressInput.value?.select();
  } catch {
    notify("无法复制路径", "error");
  }
}

async function load(next = path.value) {
  loading.value = true;
  try { const data = await api<{ path: string; entries: Entry[] }>(`/api/files?path=${encodeURIComponent(next)}`); path.value = data.path; emit("pathChanged", data.path); entries.value = data.entries; selected.value = new Set(); if (data.path) { recent.value = [data.path, ...recent.value.filter(item => item !== data.path)].slice(0, 6); localStorage.setItem("hostdesk_recent", JSON.stringify(recent.value)); } }
  catch (error) { notify(errorMessage(error), "error"); }
  finally { loading.value = false; }
}
function create(type: "file" | "directory") {
  prompt.value = { title: type === "directory" ? "新建目录" : "新建文件", label: "名称", value: type === "directory" ? "新目录" : "未命名.txt", action: async (name) => { await api("/api/create", { method: "POST", body: { path: join(path.value, name), type } }); notify("创建成功"); await load(); } };
}
function rename(entry: Entry) {
  prompt.value = { title: "重命名", label: "新名称", value: entry.name, action: async (name) => { await api("/api/move", { method: "POST", body: { from: entry.path, to: join(parent(entry.path), name) } }); notify("重命名完成"); await load(); } };
}
function setFileClipboard(mode: "copy" | "move", paths = [...selected.value]) {
  if (!paths.length) return;
  fileClipboard.value = { mode, paths };
  notify(mode === "copy" ? `已复制 ${paths.length} 项` : `已剪切 ${paths.length} 项`);
}
function duplicateName(name: string, used: Set<string>) {
  const dot = name.lastIndexOf(".");
  const hasExtension = dot > 0;
  const stem = hasExtension ? name.slice(0, dot) : name;
  const extension = hasExtension ? name.slice(dot) : "";
  let candidate = `${stem} - 副本${extension}`;
  let index = 2;
  while (used.has(candidate)) candidate = `${stem} - 副本 (${index++})${extension}`;
  return candidate;
}
async function pasteClipboard(destination = path.value) {
  const clipboard = fileClipboard.value;
  if (!clipboard?.paths.length) return;
  closeContextMenu();
  let completed = 0;
  const failed: { path: string; message: string }[] = [];
  let listing: { entries: Entry[] };
  try {
    listing = await api<{ entries: Entry[] }>(`/api/files?path=${encodeURIComponent(destination)}`);
  } catch (error) {
    notify(errorMessage(error), "error");
    return;
  }
  const used = new Set(listing.entries.map(item => item.name));
  for (const from of clipboard.paths) {
    const originalName = from.split("/").pop() || "";
    let name = originalName;
    if (clipboard.mode === "copy" && used.has(name)) name = duplicateName(name, used);
    const to = join(destination, name);
    if (clipboard.mode === "move" && to === from) { completed += 1; continue; }
    try {
      await api(`/api/${clipboard.mode}`, { method: "POST", body: { from, to } });
      used.add(name);
      completed += 1;
    } catch (error) {
      failed.push({ path: from, message: errorMessage(error) });
    }
  }
  if (clipboard.mode === "move") {
    fileClipboard.value = failed.length ? { mode: "move", paths: failed.map(item => item.path) } : null;
  }
  if (completed) notify(`已粘贴 ${completed} 项`);
  if (failed.length) notify(`${failed.length} 项粘贴失败：${failed[0].message}`, "error");
  await load();
}
function archive(paths = [...selected.value]) {
  if (!paths.length) return;
  const firstName = paths[0].split("/").pop() || "archive";
  const suggested = paths.length === 1 ? `${firstName}.tar.gz` : `archive-${new Date().toISOString().slice(0, 10)}.tar.gz`;
  prompt.value = { title: "压缩所选内容", label: "压缩包名称", value: suggested, action: async (name) => { await api("/api/archive", { method: "POST", body: { paths, name, destination: path.value } }); notify("压缩完成"); await load(); } };
}
async function submitPrompt() { if (!prompt.value?.value.trim()) return; promptBusy.value = true; try { await prompt.value.action(prompt.value.value.trim()); prompt.value = null; } catch (error) { notify(errorMessage(error), "error"); } finally { promptBusy.value = false; } }
async function remove(paths = [...selected.value]) { if (!await confirm("删除内容", `将永久删除 ${paths.length} 项内容，此操作无法撤销。`, "删除", true)) return; try { for (const item of paths) await api("/api/delete", { method: "POST", body: { path: item } }); notify("删除完成"); await load(); } catch (error) { notify(errorMessage(error), "error"); } }
async function extract(entry: Entry) { if (!await confirm("解压文件", `将 ${entry.name} 解压到当前目录。`, "解压")) return; try { await api("/api/extract", { method: "POST", body: { path: entry.path, destination: path.value } }); notify("解压完成"); await load(); } catch (error) { notify(errorMessage(error), "error"); } }
async function edit(entry: Entry) { try { const data = await api<{ content: string }>(`/api/file?path=${encodeURIComponent(entry.path)}`); editor.value = { path: entry.path, name: entry.name, content: data.content }; } catch (error) { notify(errorMessage(error), "error"); } }
async function saveEditor() { if (!editor.value) return; try { await api("/api/file", { method: "PUT", body: { path: editor.value.path, content: editor.value.content } }); notify("文件已保存"); editor.value = null; await load(); } catch (error) { notify(errorMessage(error), "error"); } }
async function uploadFiles(files: FileList | readonly File[] | null) {
  const queue = Array.from(files || []);
  if (!queue.length || uploading.value) return;
  uploading.value = true;
  try {
    for (const file of queue) {
      try { await api(`/api/upload?dir=${encodeURIComponent(path.value)}&name=${encodeURIComponent(file.name)}`, { method: "POST", body: file }); notify(`${file.name} 上传完成`); }
      catch (error) { notify(`${file.name}: ${errorMessage(error)}`, "error"); }
    }
    await load();
  } finally {
    uploading.value = false;
    if (upload.value) upload.value.value = "";
  }
}
function containsDraggedFiles(event: DragEvent) { return Array.from(event.dataTransfer?.types || []).includes("Files"); }
function handleDragEnter(event: DragEvent) {
  if (!containsDraggedFiles(event)) return;
  dragDepth += 1;
  draggingFiles.value = true;
}
function handleDragOver(event: DragEvent) {
  if (!containsDraggedFiles(event)) return;
  if (event.dataTransfer) event.dataTransfer.dropEffect = "copy";
}
function handleDragLeave(event: DragEvent) {
  if (!draggingFiles.value) return;
  dragDepth = Math.max(0, dragDepth - 1);
  if (!dragDepth) draggingFiles.value = false;
}
function resetDragState() { dragDepth = 0; draggingFiles.value = false; }
function handleDrop(event: DragEvent) {
  const files = event.dataTransfer?.files;
  resetDragState();
  if (files?.length) void uploadFiles(files);
}
function openRemoteDownload() {
  remoteForm.value = { url: "", name: "" };
  remoteModal.value = true;
}
async function submitRemoteDownload() {
  if (!remoteForm.value.url.trim()) return;
  remoteBusy.value = true;
  try {
    const data = await api<{ name: string }>("/api/remote-download", { method: "POST", body: { url: remoteForm.value.url.trim(), destination: path.value, name: remoteForm.value.name.trim() } });
    notify(`${data.name} 下载完成`);
    remoteModal.value = false;
    await load();
  } catch (error) {
    notify(errorMessage(error), "error");
  } finally {
    remoteBusy.value = false;
  }
}
async function openPermissions(entry: Entry) {
  permissionEntry.value = entry;
  permissionForm.value = { owner: entry.owner, group: entry.group, mode: entry.mode, recursive: false };
  permissionModal.value = true;
  try {
    identityOptions.value = await api<{ users: string[]; groups: string[] }>("/api/file-identities");
  } catch (error) {
    permissionModal.value = false;
    notify(errorMessage(error), "error");
  }
}
async function submitPermissions() {
  if (!permissionEntry.value) return;
  permissionBusy.value = true;
  try {
    await api("/api/file-permissions", { method: "POST", body: { path: permissionEntry.value.path, ...permissionForm.value } });
    notify("权限与属组已更新");
    permissionModal.value = false;
    await load();
  } catch (error) {
    notify(errorMessage(error), "error");
  } finally {
    permissionBusy.value = false;
  }
}
function isArchive(entry: Entry) { return /\.(zip|tar|tar\.gz|tgz)$/i.test(entry.name); }
function toggleAll() { selected.value = selected.value.size === entries.value.length ? new Set() : new Set(entries.value.map(item => item.path)); }
function closeContextMenu() { contextMenu.value = null; }
async function openContextMenu(event: MouseEvent, entry: Entry | null) {
  if (entry && !selected.value.has(entry.path)) selected.value = new Set([entry.path]);
  contextMenu.value = { x: event.clientX, y: event.clientY, entry };
  await nextTick();
  const menu = contextMenuElement.value;
  if (!menu || !contextMenu.value) return;
  const bounds = menu.getBoundingClientRect();
  contextMenu.value.x = Math.max(8, Math.min(event.clientX, window.innerWidth - bounds.width - 8));
  contextMenu.value.y = Math.max(8, Math.min(event.clientY, window.innerHeight - bounds.height - 8));
}
function shortcutBlocked(event: KeyboardEvent) {
  if (event.defaultPrevented || prompt.value || editor.value || addressMode.value || document.querySelector(".modal-layer")) return true;
  const target = event.target;
  if (!(target instanceof Element)) return false;
  if (target.closest("textarea, select, [contenteditable='true']")) return true;
  if (target instanceof HTMLInputElement) return !["button", "checkbox", "radio", "reset", "submit"].includes(target.type);
  return false;
}
function handleShortcut(event: KeyboardEvent) {
  if (event.key === "Escape" && contextMenu.value) { event.preventDefault(); closeContextMenu(); return; }
  if (shortcutBlocked(event)) return;
  const modifier = event.ctrlKey || event.metaKey;
  const key = event.key.toLowerCase();
  if (modifier && key === "a" && entries.value.length) {
    event.preventDefault();
    selected.value = new Set(entries.value.map(item => item.path));
  } else if (modifier && key === "c" && selected.value.size) {
    event.preventDefault();
    setFileClipboard("copy");
  } else if (modifier && key === "x" && selected.value.size) {
    event.preventDefault();
    setFileClipboard("move");
  } else if (modifier && key === "v" && fileClipboard.value) {
    event.preventDefault();
    void pasteClipboard();
  } else if (event.key === "Delete" && selected.value.size && !event.repeat) {
    event.preventDefault();
    void remove();
  } else if (event.key === "F2" && selected.value.size === 1 && !event.repeat) {
    event.preventDefault();
    const entry = entries.value.find(item => selected.value.has(item.path));
    if (entry) rename(entry);
  }
}
function closeContextOnScroll() { closeContextMenu(); }
onMounted(() => {
  try { recent.value = JSON.parse(localStorage.getItem("hostdesk_recent") || "[]"); } catch { recent.value = []; }
  window.addEventListener("keydown", handleShortcut);
  window.addEventListener("click", closeContextMenu);
  window.addEventListener("resize", closeContextMenu);
  window.addEventListener("scroll", closeContextOnScroll, true);
  window.addEventListener("dragend", resetDragState);
  void load();
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", handleShortcut);
  window.removeEventListener("click", closeContextMenu);
  window.removeEventListener("resize", closeContextMenu);
  window.removeEventListener("scroll", closeContextOnScroll, true);
  window.removeEventListener("dragend", resetDragState);
});
watch(() => props.initialPath, (next) => { if ((next || "") !== path.value) void load(next || ""); });
</script>

<template>
  <PageHeader title="文件管理" :subtitle="path ? `/${path}` : '/'" kicker="FILE MANAGER">
    <button class="button" type="button" @click="create('directory')"><FolderPlus :size="16" />目录</button>
    <button class="button" type="button" @click="create('file')"><FilePlus :size="16" />文件</button>
    <button class="button" type="button" @click="openRemoteDownload"><CloudDownload :size="16" />远程下载</button>
    <button class="button primary" type="button" :disabled="uploading" @click="upload?.click()"><Upload :size="16" />上传</button><input ref="upload" hidden type="file" multiple @change="uploadFiles(($event.target as HTMLInputElement).files)">
  </PageHeader>
  <div class="file-navigation">
    <div v-if="recent.length" class="recent-paths"><span><FolderClock :size="15" />最近路径</span><div><button v-for="item in recent" :key="item" type="button" :class="{ active: item === path }" :title="fullPath(item)" @click="load(item)">{{ fullPath(item) }}</button></div></div>
    <div class="file-toolbar">
      <form v-if="addressMode" class="path-address" @submit.prevent="openAddress" @focusout="leaveAddress"><input ref="addressInput" v-model="addressValue" aria-label="完整路径" autocomplete="off" spellcheck="false" @keydown.esc.prevent="addressMode = false"><button class="icon-button" title="复制完整路径" type="button" @mousedown.prevent @click="copyAddress"><Copy :size="16" /></button></form>
      <div v-else class="path-bar" title="显示完整路径" @click.self="showAddress"><template v-for="(crumb, index) in breadcrumbs" :key="crumb.path"><ChevronRight v-if="crumb.path" :size="14" /><button type="button" :class="{ current: index === breadcrumbs.length - 1 }" @click="index === breadcrumbs.length - 1 ? showAddress() : load(crumb.path)">{{ crumb.label }}</button></template></div>
      <button class="icon-button" title="粘贴 (Ctrl+V)" type="button" :disabled="!fileClipboard" @click="pasteClipboard()"><ClipboardPaste :size="17" /></button><button class="icon-button" title="刷新" type="button" :disabled="loading" @click="load()"><RefreshCw :class="{ spin: loading }" :size="17" /></button>
    </div>
  </div>
  <div v-if="selected.size" class="selection-bar"><strong>已选 {{ selected.size }} 项</strong><button class="button" title="Ctrl+C" @click="setFileClipboard('copy')"><Copy :size="15" />复制</button><button class="button" title="Ctrl+X" @click="setFileClipboard('move')"><Scissors :size="15" />剪切</button><button class="button" @click="archive()"><Archive :size="15" />压缩</button><button class="button danger-text" title="Delete" @click="remove()"><Trash2 :size="15" />删除</button></div>
  <div class="data-surface file-manager-surface" :class="{ 'drag-active': draggingFiles }" @contextmenu.prevent="openContextMenu($event, null)" @dragenter.prevent="handleDragEnter" @dragover.prevent="handleDragOver" @dragleave.prevent="handleDragLeave" @drop.prevent="handleDrop"><div class="table-scroll"><table class="data-table"><thead><tr><th class="check-column"><input type="checkbox" :checked="entries.length > 0 && selected.size === entries.length" :aria-label="selected.size === entries.length ? '取消全选' : '全选'" @change="toggleAll"></th><th>名称</th><th>大小</th><th>修改时间</th><th>属主 / 属组</th><th>权限</th><th class="actions-column"></th></tr></thead><tbody>
    <tr v-for="entry in entries" :key="entry.path" :class="{ selected: selectedEntry(entry), cut: isCut(entry) }" @contextmenu.prevent.stop="openContextMenu($event, entry)"><td><input type="checkbox" :checked="selectedEntry(entry)" :aria-label="`选择 ${entry.name}`" @change="toggle(entry)"></td><td><button class="file-entry" type="button" @dblclick="entry.type === 'directory' ? load(entry.path) : edit(entry)" @click="entry.type === 'directory' ? load(entry.path) : edit(entry)"><Folder v-if="entry.type === 'directory'" :size="18" /><FileCode v-else-if="/\.(js|ts|go|php|py|sh|json|html|css|md|ya?ml)$/i.test(entry.name)" :size="18" /><File v-else :size="18" /><span>{{ entry.name }}</span></button></td><td>{{ formatBytes(entry.size, entry.type === 'directory') }}</td><td>{{ displayDate(entry.modified) }}</td><td class="file-owner-cell"><code>{{ entry.owner }}</code><span>:</span><code>{{ entry.group }}</code></td><td><code>{{ entry.mode }}</code></td><td><div class="row-actions"><button v-if="entry.type === 'directory'" class="icon-button" title="打开" @click="load(entry.path)"><FolderOpen :size="16" /></button><button v-else class="icon-button" title="编辑" @click="edit(entry)"><Pencil :size="16" /></button><a v-if="entry.type !== 'directory'" class="icon-button" title="下载" :href="`/api/download?path=${encodeURIComponent(entry.path)}`"><Download :size="16" /></a><button v-if="isArchive(entry)" class="icon-button" title="解压" @click="extract(entry)"><Archive :size="16" /></button><button class="icon-button" title="权限与属主" @click="openPermissions(entry)"><Shield :size="16" /></button><button class="icon-button" title="重命名" @click="rename(entry)"><Ellipsis :size="16" /></button><button class="icon-button danger" title="删除" @click="remove([entry.path])"><Trash2 :size="16" /></button></div></td></tr>
  </tbody></table></div><EmptyState v-if="!loading && !entries.length" message="当前目录为空" /><div v-if="draggingFiles" class="file-drop-overlay"><Upload :size="30" /><strong>释放以上传</strong></div></div>
  <Teleport to="body"><div v-if="contextMenu" ref="contextMenuElement" class="file-context-menu" :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }" role="menu" @click.stop @contextmenu.prevent>
    <div v-if="contextMenu.entry" class="file-context-title" :title="contextMenu.entry.name">{{ contextMenu.entry.name }}</div>
    <button v-if="contextMenu.entry?.type === 'directory'" type="button" role="menuitem" @click="load(contextMenu.entry.path); closeContextMenu()"><FolderOpen :size="16" /><span>打开</span></button>
    <button v-else-if="contextMenu.entry" type="button" role="menuitem" @click="edit(contextMenu.entry); closeContextMenu()"><Pencil :size="16" /><span>编辑</span></button>
    <a v-if="contextMenu.entry && contextMenu.entry.type !== 'directory'" role="menuitem" :href="`/api/download?path=${encodeURIComponent(contextMenu.entry.path)}`" @click="closeContextMenu"><Download :size="16" /><span>下载</span></a>
    <div v-if="contextMenu.entry" class="file-context-separator"></div>
    <button v-if="contextMenu.entry" type="button" role="menuitem" @click="setFileClipboard('copy', contextPaths(contextMenu.entry)); closeContextMenu()"><Copy :size="16" /><span>复制</span><kbd>Ctrl+C</kbd></button>
    <button v-if="contextMenu.entry" type="button" role="menuitem" @click="setFileClipboard('move', contextPaths(contextMenu.entry)); closeContextMenu()"><Scissors :size="16" /><span>剪切</span><kbd>Ctrl+X</kbd></button>
    <button v-if="fileClipboard" type="button" role="menuitem" @click="pasteClipboard(contextMenu.entry?.type === 'directory' ? contextMenu.entry.path : path)"><ClipboardPaste :size="16" /><span>{{ contextMenu.entry?.type === 'directory' ? '粘贴到此文件夹' : '粘贴到此处' }}</span><kbd>Ctrl+V</kbd></button>
    <button v-if="contextMenu.entry" type="button" role="menuitem" @click="archive(contextPaths(contextMenu.entry)); closeContextMenu()"><Archive :size="16" /><span>压缩</span></button>
    <button v-if="contextMenu.entry && isArchive(contextMenu.entry)" type="button" role="menuitem" @click="extract(contextMenu.entry); closeContextMenu()"><Archive :size="16" /><span>解压到当前目录</span></button>
    <template v-if="contextMenu.entry"><div class="file-context-separator"></div><button type="button" role="menuitem" @click="openPermissions(contextMenu.entry); closeContextMenu()"><Shield :size="16" /><span>权限与属主</span></button><button type="button" role="menuitem" @click="rename(contextMenu.entry); closeContextMenu()"><Ellipsis :size="16" /><span>重命名</span><kbd>F2</kbd></button><button class="danger" type="button" role="menuitem" @click="remove(contextPaths(contextMenu.entry)); closeContextMenu()"><Trash2 :size="16" /><span>删除</span><kbd>Del</kbd></button></template>
    <template v-else><button type="button" role="menuitem" @click="create('directory'); closeContextMenu()"><FolderPlus :size="16" /><span>新建目录</span></button><button type="button" role="menuitem" @click="create('file'); closeContextMenu()"><FilePlus :size="16" /><span>新建文件</span></button><button type="button" role="menuitem" @click="openRemoteDownload(); closeContextMenu()"><CloudDownload :size="16" /><span>远程下载</span></button><div class="file-context-separator"></div><button type="button" role="menuitem" :disabled="!entries.length" @click="selected = new Set(entries.map(item => item.path)); closeContextMenu()"><Copy :size="16" /><span>全选</span><kbd>Ctrl+A</kbd></button><button type="button" role="menuitem" @click="load(); closeContextMenu()"><RefreshCw :size="16" /><span>刷新</span></button></template>
  </div></Teleport>
  <AppModal v-if="remoteModal" title="远程下载" submit-label="开始下载" :busy="remoteBusy" @close="remoteModal = false" @submit="submitRemoteDownload"><div class="form-grid"><label class="field full">下载地址<input v-model="remoteForm.url" type="url" inputmode="url" autocomplete="off" spellcheck="false" placeholder="https://example.com/file.zip" required autofocus></label><label class="field full">保存文件名（可选）<input v-model="remoteForm.name" autocomplete="off" placeholder="自动识别"></label></div><div class="editor-meta"><span>保存到</span><code>{{ fullPath() }}</code></div></AppModal>
  <AppModal v-if="permissionModal && permissionEntry" :title="`权限 · ${permissionEntry.name}`" submit-label="应用设置" :busy="permissionBusy" @close="permissionModal = false" @submit="submitPermissions"><div class="form-grid"><label class="field">属主<input v-model="permissionForm.owner" list="file-owner-options" autocomplete="off" required><datalist id="file-owner-options"><option v-for="item in identityOptions.users" :key="item" :value="item"></option></datalist></label><label class="field">用户组<input v-model="permissionForm.group" list="file-group-options" autocomplete="off" required><datalist id="file-group-options"><option v-for="item in identityOptions.groups" :key="item" :value="item"></option></datalist></label><label class="field full">权限模式<input v-model="permissionForm.mode" inputmode="numeric" pattern="0?[0-7]{3}" maxlength="4" placeholder="755" required></label><label v-if="permissionEntry.type === 'directory'" class="check-field full"><input v-model="permissionForm.recursive" type="checkbox">递归应用到目录内容</label></div><div class="editor-meta"><code>/{{ permissionEntry.path }}</code><span>{{ permissionEntry.owner }}:{{ permissionEntry.group }} · {{ permissionEntry.mode }}</span></div></AppModal>
  <AppModal v-if="prompt" :title="prompt.title" :busy="promptBusy" @close="prompt = null" @submit="submitPrompt"><label class="field">{{ prompt.label }}<input v-model="prompt.value" required autofocus></label></AppModal>
  <AppModal v-if="editor" :title="editor.name" wide submit-label="保存文件" @close="editor = null" @submit="saveEditor"><textarea v-model="editor.content" class="code-editor" spellcheck="false"></textarea><div class="editor-meta"><code>/{{ editor.path }}</code><span>{{ editor.content.split('\n').length }} 行</span></div><template #actions><button class="button quiet" type="button" @click="editor = null">关闭</button><button class="button primary" type="submit"><Save :size="16" />保存</button></template></AppModal>
</template>
