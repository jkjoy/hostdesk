<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { Archive, ChevronRight, Copy, Download, Ellipsis, File, FileCode, FilePlus, Folder, FolderClock, FolderOpen, FolderPlus, Move, Pencil, RefreshCw, Save, Trash2, Upload } from "@lucide/vue";
import { api } from "../api";
import { errorMessage, formatBytes, displayDate, useUI } from "../ui";
import AppModal from "../components/AppModal.vue";
import EmptyState from "../components/EmptyState.vue";
import PageHeader from "../components/PageHeader.vue";

interface Entry { name: string; path: string; type: "directory" | "file" | "link"; size: number; modified: string; mode: string }
const props = defineProps<{ initialPath?: string }>();
const { notify, confirm } = useUI();
const path = ref(props.initialPath || "");
const entries = ref<Entry[]>([]);
const selected = ref(new Set<string>());
const loading = ref(false);
const upload = ref<HTMLInputElement>();
const recent = ref<string[]>([]);
const editor = ref<{ path: string; name: string; content: string } | null>(null);
const prompt = ref<{ title: string; label: string; value: string; action: (value: string) => Promise<void> } | null>(null);
const promptBusy = ref(false);
const breadcrumbs = computed(() => ["", ...path.value.split("/").filter(Boolean)].map((part, index, all) => ({ label: index ? part : "根目录", path: all.slice(1, index + 1).join("/") })));

function join(...parts: string[]) { return parts.filter(Boolean).join("/").replace(/\/+/g, "/"); }
function parent(value: string) { const parts = value.split("/").filter(Boolean); parts.pop(); return parts.join("/"); }
function selectedEntry(entry: Entry) { return selected.value.has(entry.path); }
function toggle(entry: Entry) { const next = new Set(selected.value); next.has(entry.path) ? next.delete(entry.path) : next.add(entry.path); selected.value = next; }

async function load(next = path.value) {
  loading.value = true;
  try { const data = await api<{ path: string; entries: Entry[] }>(`/api/files?path=${encodeURIComponent(next)}`); path.value = data.path; entries.value = data.entries; selected.value = new Set(); if (data.path) { recent.value = [data.path, ...recent.value.filter(item => item !== data.path)].slice(0, 6); localStorage.setItem("hostdesk_recent", JSON.stringify(recent.value)); } }
  catch (error) { notify(errorMessage(error), "error"); }
  finally { loading.value = false; }
}
function create(type: "file" | "directory") {
  prompt.value = { title: type === "directory" ? "新建目录" : "新建文件", label: "名称", value: type === "directory" ? "新目录" : "未命名.txt", action: async (name) => { await api("/api/create", { method: "POST", body: { path: join(path.value, name), type } }); notify("创建成功"); await load(); } };
}
function rename(entry: Entry) {
  prompt.value = { title: "重命名", label: "新名称", value: entry.name, action: async (name) => { await api("/api/move", { method: "POST", body: { from: entry.path, to: join(parent(entry.path), name) } }); notify("重命名完成"); await load(); } };
}
function transfer(kind: "copy" | "move") {
  prompt.value = { title: kind === "copy" ? "复制到" : "移动到", label: "目标目录（相对管理根目录）", value: path.value, action: async (destination) => { for (const from of selected.value) await api(`/api/${kind}`, { method: "POST", body: { from, to: join(destination, from.split("/").pop() || "") } }); notify(kind === "copy" ? "复制完成" : "移动完成"); await load(); } };
}
function archive() {
  prompt.value = { title: "打包所选内容", label: "压缩包名称", value: `archive-${new Date().toISOString().slice(0, 10)}.tar.gz`, action: async (name) => { await api("/api/archive", { method: "POST", body: { paths: [...selected.value], name, destination: path.value } }); notify("打包完成"); await load(); } };
}
async function submitPrompt() { if (!prompt.value?.value.trim()) return; promptBusy.value = true; try { await prompt.value.action(prompt.value.value.trim()); prompt.value = null; } catch (error) { notify(errorMessage(error), "error"); } finally { promptBusy.value = false; } }
async function remove(paths = [...selected.value]) { if (!await confirm("删除内容", `将永久删除 ${paths.length} 项内容，此操作无法撤销。`, "删除", true)) return; try { for (const item of paths) await api("/api/delete", { method: "POST", body: { path: item } }); notify("删除完成"); await load(); } catch (error) { notify(errorMessage(error), "error"); } }
async function extract(entry: Entry) { if (!await confirm("解压文件", `将 ${entry.name} 解压到当前目录。`, "解压")) return; try { await api("/api/extract", { method: "POST", body: { path: entry.path, destination: path.value } }); notify("解压完成"); await load(); } catch (error) { notify(errorMessage(error), "error"); } }
async function edit(entry: Entry) { try { const data = await api<{ content: string }>(`/api/file?path=${encodeURIComponent(entry.path)}`); editor.value = { path: entry.path, name: entry.name, content: data.content }; } catch (error) { notify(errorMessage(error), "error"); } }
async function saveEditor() { if (!editor.value) return; try { await api("/api/file", { method: "PUT", body: { path: editor.value.path, content: editor.value.content } }); notify("文件已保存"); editor.value = null; await load(); } catch (error) { notify(errorMessage(error), "error"); } }
async function uploadFiles(files: FileList | null) { if (!files) return; for (const file of files) { try { await api(`/api/upload?dir=${encodeURIComponent(path.value)}&name=${encodeURIComponent(file.name)}`, { method: "POST", body: file }); notify(`${file.name} 上传完成`); } catch (error) { notify(`${file.name}: ${errorMessage(error)}`, "error"); } } if (upload.value) upload.value.value = ""; await load(); }
function isArchive(entry: Entry) { return /\.(zip|tar|tar\.gz|tgz)$/i.test(entry.name); }
function toggleAll() { selected.value = selected.value.size === entries.value.length ? new Set() : new Set(entries.value.map(item => item.path)); }
onMounted(() => { try { recent.value = JSON.parse(localStorage.getItem("hostdesk_recent") || "[]"); } catch { recent.value = []; } void load(); });
</script>

<template>
  <PageHeader title="文件管理" :subtitle="path ? `/${path}` : '/'" kicker="FILE MANAGER">
    <button class="button" type="button" @click="create('directory')"><FolderPlus :size="16" />目录</button>
    <button class="button" type="button" @click="create('file')"><FilePlus :size="16" />文件</button>
    <button class="button primary" type="button" @click="upload?.click()"><Upload :size="16" />上传</button><input ref="upload" hidden type="file" multiple @change="uploadFiles(($event.target as HTMLInputElement).files)">
  </PageHeader>
  <div class="file-toolbar">
    <div class="path-bar"><template v-for="crumb in breadcrumbs" :key="crumb.path"><ChevronRight v-if="crumb.path" :size="14" /><button type="button" @click="load(crumb.path)">{{ crumb.label }}</button></template></div>
    <label v-if="recent.length" class="recent-jump" title="最近路径"><FolderClock :size="16" /><select :value="path" @change="load(($event.target as HTMLSelectElement).value)"><option disabled value="">最近路径</option><option v-for="item in recent" :key="item" :value="item">/{{ item }}</option></select></label>
    <button class="icon-button" title="刷新" type="button" :disabled="loading" @click="load()"><RefreshCw :class="{ spin: loading }" :size="17" /></button>
  </div>
  <div v-if="selected.size" class="selection-bar"><strong>已选 {{ selected.size }} 项</strong><button class="button" @click="transfer('copy')"><Copy :size="15" />复制</button><button class="button" @click="transfer('move')"><Move :size="15" />移动</button><button class="button" @click="archive"><Archive :size="15" />打包</button><button class="button danger-text" @click="remove()"><Trash2 :size="15" />删除</button></div>
  <div class="data-surface"><div class="table-scroll"><table class="data-table"><thead><tr><th class="check-column"><input type="checkbox" :checked="entries.length > 0 && selected.size === entries.length" :aria-label="selected.size === entries.length ? '取消全选' : '全选'" @change="toggleAll"></th><th>名称</th><th>大小</th><th>修改时间</th><th>权限</th><th class="actions-column"></th></tr></thead><tbody>
    <tr v-for="entry in entries" :key="entry.path" :class="{ selected: selectedEntry(entry) }"><td><input type="checkbox" :checked="selectedEntry(entry)" :aria-label="`选择 ${entry.name}`" @change="toggle(entry)"></td><td><button class="file-entry" type="button" @dblclick="entry.type === 'directory' ? load(entry.path) : edit(entry)" @click="entry.type === 'directory' ? load(entry.path) : edit(entry)"><Folder v-if="entry.type === 'directory'" :size="18" /><FileCode v-else-if="/\.(js|ts|go|php|py|sh|json|html|css|md|ya?ml)$/i.test(entry.name)" :size="18" /><File v-else :size="18" /><span>{{ entry.name }}</span></button></td><td>{{ formatBytes(entry.size, entry.type === 'directory') }}</td><td>{{ displayDate(entry.modified) }}</td><td><code>{{ entry.mode }}</code></td><td><div class="row-actions"><button v-if="entry.type === 'directory'" class="icon-button" title="打开" @click="load(entry.path)"><FolderOpen :size="16" /></button><button v-else class="icon-button" title="编辑" @click="edit(entry)"><Pencil :size="16" /></button><a v-if="entry.type !== 'directory'" class="icon-button" title="下载" :href="`/api/download?path=${encodeURIComponent(entry.path)}`"><Download :size="16" /></a><button v-if="isArchive(entry)" class="icon-button" title="解压" @click="extract(entry)"><Archive :size="16" /></button><button class="icon-button" title="重命名" @click="rename(entry)"><Ellipsis :size="16" /></button><button class="icon-button danger" title="删除" @click="remove([entry.path])"><Trash2 :size="16" /></button></div></td></tr>
  </tbody></table></div><EmptyState v-if="!loading && !entries.length" message="当前目录为空" /></div>
  <AppModal v-if="prompt" :title="prompt.title" :busy="promptBusy" @close="prompt = null" @submit="submitPrompt"><label class="field">{{ prompt.label }}<input v-model="prompt.value" required autofocus></label></AppModal>
  <AppModal v-if="editor" :title="editor.name" wide submit-label="保存文件" @close="editor = null" @submit="saveEditor"><textarea v-model="editor.content" class="code-editor" spellcheck="false"></textarea><div class="editor-meta"><code>/{{ editor.path }}</code><span>{{ editor.content.split('\n').length }} 行</span></div><template #actions><button class="button quiet" type="button" @click="editor = null">关闭</button><button class="button primary" type="submit"><Save :size="16" />保存</button></template></AppModal>
</template>
