<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { ExternalLink, FileCode2, FolderOpen, Pause, Play, Plus, RotateCcw, Save, Settings2, Trash2 } from "@lucide/vue";
import { api } from "../api";
import { errorMessage, useUI } from "../ui";
import AppModal from "../components/AppModal.vue";
import EmptyState from "../components/EmptyState.vue";
import PageHeader from "../components/PageHeader.vue";

interface Site {
  id: string; domain: string; aliases: string[]; type: string; root: string; upstream: string;
  runDirectory?: string; rewriteMode?: string; rewriteRules?: string; enabled: boolean; ssl: boolean;
  certificateMode?: "managed" | "custom"; certificateId?: string; certificateConfigured: boolean;
}
interface CertificateOption { id: string; domains: string[]; expiresAt: string }

const emit = defineEmits<{ openFiles: [path: string] }>();
const { notify, confirm } = useUI();
const sites = ref<Site[]>([]);
const certificates = ref<CertificateOption[]>([]);
const loading = ref(false);
const directoriesLoading = ref(false);
const runDirectories = ref<string[]>([]);
const modal = ref(false);
const editing = ref<Site | null>(null);
const nginxModal = ref(false);
const nginxSite = ref<Site | null>(null);
const nginxConfig = ref("");
const nginxConfigPath = ref("");
const nginxCustomized = ref(false);
const nginxLoading = ref(false);
const form = reactive({
  domain: "", aliases: "", type: "static", root: "", runDirectory: "", upstream: "", rewriteMode: "laravel", rewriteRules: "",
  ssl: false, certificateMode: "managed" as "managed" | "custom", certificateId: "", certificatePem: "", privateKeyPem: "",
});
const customCertificateRequired = computed(() => form.ssl && form.certificateMode === "custom" && !(editing.value?.certificateMode === "custom" && editing.value.certificateConfigured));
const availableRunDirectories = computed(() => {
  const directories = runDirectories.value.filter(Boolean);
  if (!form.runDirectory || directories.includes(form.runDirectory)) return directories;
  return [...directories, form.runDirectory];
});

async function load() {
  loading.value = true;
  try {
    const data = await api<{ sites: Site[]; certificates: CertificateOption[] }>("/api/admin/sites");
    sites.value = data.sites;
    certificates.value = data.certificates || [];
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    loading.value = false;
  }
}

async function loadRunDirectories() {
  runDirectories.value = [];
  if (form.type !== "php" || !form.root.trim()) return;
  directoriesLoading.value = true;
  try {
    const query = new URLSearchParams({ root: form.root.trim() });
    const data = await api<{ directories: string[] }>(`/api/admin/site-directories?${query}`);
    runDirectories.value = data.directories;
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    directoriesLoading.value = false;
  }
}

function open(site?: Site) {
  editing.value = site || null;
  const defaultMode = site?.certificateMode || (certificates.value.length ? "managed" : "custom");
  Object.assign(form, site ? {
    domain: site.domain, aliases: site.aliases.join(", "), type: site.type, root: site.root, upstream: site.upstream,
    runDirectory: site.runDirectory || "",
    rewriteMode: site.rewriteMode || "laravel", rewriteRules: site.rewriteRules || "", ssl: site.ssl,
    certificateMode: defaultMode, certificateId: site.certificateId || certificates.value[0]?.id || "", certificatePem: "", privateKeyPem: "",
  } : {
    domain: "", aliases: "", type: "static", root: "", runDirectory: "", upstream: "", rewriteMode: "laravel", rewriteRules: "", ssl: false,
    certificateMode: defaultMode, certificateId: certificates.value[0]?.id || "", certificatePem: "", privateKeyPem: "",
  });
  modal.value = true;
  void loadRunDirectories();
}

async function save() {
  loading.value = true;
  try {
    const body = {
      domain: form.domain,
      aliases: form.aliases.split(",").map(value => value.trim()).filter(Boolean),
      type: form.type,
      root: form.root,
      runDirectory: form.type === "php" ? form.runDirectory : "",
      upstream: form.upstream,
      rewriteMode: form.rewriteMode,
      rewriteRules: form.rewriteRules,
      ssl: form.ssl,
      certificateMode: form.ssl ? form.certificateMode : "",
      certificateId: form.ssl && form.certificateMode === "managed" ? form.certificateId : "",
      certificatePem: form.ssl && form.certificateMode === "custom" ? form.certificatePem : "",
      privateKeyPem: form.ssl && form.certificateMode === "custom" ? form.privateKeyPem : "",
    };
    await api(editing.value ? `/api/admin/sites/${editing.value.id}` : "/api/admin/sites", { method: editing.value ? "PUT" : "POST", body });
    notify(editing.value ? "网站配置已保存" : "网站已创建");
    modal.value = false;
    await load();
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    loading.value = false;
  }
}

async function toggle(site: Site) {
  try {
    await api(`/api/admin/sites/${site.id}/${site.enabled ? "disable" : "enable"}`, { method: "POST" });
    notify("网站状态已更新");
    await load();
  } catch (e) {
    notify(errorMessage(e), "error");
  }
}

async function remove(site: Site) {
  if (!await confirm(`删除 ${site.domain}`, "将删除 Nginx 站点配置，网站目录和文件会保留。", "删除", true)) return;
  try {
    await api(`/api/admin/sites/${site.id}`, { method: "DELETE" });
    notify("网站配置已删除");
    await load();
  } catch (e) {
    notify(errorMessage(e), "error");
  }
}

async function openNginx(site: Site) {
  nginxSite.value = site;
  nginxConfig.value = "";
  nginxConfigPath.value = "";
  nginxCustomized.value = false;
  nginxModal.value = true;
  nginxLoading.value = true;
  try {
    const data = await api<{ config: string; path: string; customized: boolean }>(`/api/admin/sites/${site.id}/nginx`);
    nginxConfig.value = data.config;
    nginxConfigPath.value = data.path;
    nginxCustomized.value = data.customized;
  } catch (e) {
    nginxModal.value = false;
    notify(errorMessage(e), "error");
  } finally {
    nginxLoading.value = false;
  }
}

async function saveNginx() {
  if (!nginxSite.value) return;
  nginxLoading.value = true;
  try {
    await api(`/api/admin/sites/${nginxSite.value.id}/nginx`, { method: "PUT", body: { config: nginxConfig.value } });
    nginxCustomized.value = true;
    notify(nginxSite.value.enabled ? "Nginx 配置已校验、保存并重新加载" : "Nginx 配置已校验并保存");
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    nginxLoading.value = false;
  }
}

async function resetNginx() {
  if (!nginxSite.value || !await confirm("恢复托管配置", `将重新生成 ${nginxSite.value.domain} 的 Nginx 配置，当前手工修改会被替换。`, "恢复")) return;
  nginxLoading.value = true;
  try {
    await api(`/api/admin/sites/${nginxSite.value.id}/nginx`, { method: "DELETE" });
    notify("已恢复 HostDesk 托管配置");
    await openNginx(nginxSite.value);
  } catch (e) {
    notify(errorMessage(e), "error");
  } finally {
    nginxLoading.value = false;
  }
}

function openFiles(site: Site) { emit("openFiles", site.root); }
function siteURL(site: Site) { return `${site.ssl ? "https" : "http"}://${site.domain}`; }
function siteDocumentRoot(site: Site) {
  if (site.type !== "php" || !site.runDirectory) return site.root;
  return `${site.root.replace(/\/+$/, "")}/${site.runDirectory.replace(/^\/+/, "")}`;
}
onMounted(load);
</script>

<template>
  <PageHeader title="网站管理" subtitle="Nginx 虚拟主机与网站目录" kicker="SITES"><button class="button primary" @click="open()"><Plus :size="16" />添加网站</button></PageHeader>
  <div class="data-surface"><div class="table-scroll"><table class="data-table"><thead><tr><th>域名</th><th>类型</th><th>目录 / 上游</th><th>HTTPS</th><th>状态</th><th class="actions-column">操作</th></tr></thead><tbody><tr v-for="site in sites" :key="site.id"><td><a class="site-domain-link" :href="siteURL(site)" target="_blank" rel="noopener noreferrer" :title="`新窗口打开 ${site.domain}`"><strong>{{ site.domain }}</strong><ExternalLink :size="13" /></a><small v-if="site.aliases.length">{{ site.aliases.join(', ') }}</small></td><td>{{ { static: '静态', php: 'PHP', proxy: '反代' }[site.type] || site.type }}</td><td class="truncate-cell" :title="site.type === 'proxy' ? site.upstream : siteDocumentRoot(site)">{{ site.type === 'proxy' ? site.upstream : siteDocumentRoot(site) }}<small v-if="site.type === 'php' && site.runDirectory">项目：{{ site.root }}</small></td><td>{{ site.ssl ? '已启用' : 'HTTP' }}</td><td><span class="status-pill" :class="{ running: site.enabled, stopped: !site.enabled }">{{ site.enabled ? '启用' : '停用' }}</span></td><td><div class="row-actions"><button v-if="site.type !== 'proxy'" class="icon-button" title="打开网站文件" aria-label="打开网站文件" @click="openFiles(site)"><FolderOpen :size="16" /></button><button class="icon-button" title="编辑 Nginx 配置" @click="openNginx(site)"><FileCode2 :size="16" /></button><button class="icon-button" title="网站设置" @click="open(site)"><Settings2 :size="16" /></button><button class="icon-button" :title="site.enabled ? '停用' : '启用'" @click="toggle(site)"><Pause v-if="site.enabled" :size="16" /><Play v-else :size="16" /></button><button class="icon-button danger" title="删除" @click="remove(site)"><Trash2 :size="16" /></button></div></td></tr></tbody></table></div><EmptyState v-if="!loading && !sites.length" message="还没有网站" /></div>
  <AppModal v-if="modal" :title="editing ? `修改 ${editing.domain}` : '添加网站'" wide :busy="loading" :submit-label="editing ? '保存配置' : '创建网站'" @close="modal = false" @submit="save">
    <div class="form-grid">
      <label class="field">主域名<input v-model="form.domain" :disabled="!!editing" placeholder="example.com" required></label>
      <label class="field">别名域名<input v-model="form.aliases" placeholder="www.example.com, api.example.com"></label>
      <label class="field">网站类型<select v-model="form.type" @change="loadRunDirectories"><option value="static">静态网站</option><option value="php">PHP 网站</option><option value="proxy">反向代理</option></select></label>
      <label v-if="form.type !== 'proxy'" class="field">网站目录<input v-model="form.root" placeholder="自动生成到 /var/www" @change="loadRunDirectories"></label>
      <label v-else class="field">上游地址<input v-model="form.upstream" placeholder="http://127.0.0.1:3000"></label>
      <label v-if="form.type === 'php'" class="field">运行目录<select v-model="form.runDirectory" :disabled="directoriesLoading"><option value="">网站根目录</option><option v-for="directory in availableRunDirectories" :key="directory" :value="directory">/{{ directory }}</option></select></label>
      <label v-if="form.type === 'php'" class="field">伪静态<select v-model="form.rewriteMode"><option value="none">关闭</option><option value="wordpress">WordPress</option><option value="laravel">通用 PHP / Laravel</option><option value="thinkphp">ThinkPHP</option><option value="custom">自定义</option></select></label>
      <label v-if="form.type === 'php' && form.rewriteMode === 'custom'" class="field full">自定义规则<textarea v-model="form.rewriteRules" rows="5" spellcheck="false"></textarea></label>
      <label class="check-field full"><input v-model="form.ssl" type="checkbox">启用 HTTPS</label>
      <template v-if="form.ssl">
        <fieldset class="option-field full"><legend>证书来源</legend><label><input v-model="form.certificateMode" type="radio" value="managed">已有证书</label><label><input v-model="form.certificateMode" type="radio" value="custom">填写证书</label></fieldset>
        <label v-if="form.certificateMode === 'managed'" class="field full">已有证书<select v-model="form.certificateId" required><option value="" disabled>请选择证书</option><option v-for="certificate in certificates" :key="certificate.id" :value="certificate.id">{{ certificate.domains.join(', ') }}</option></select></label>
        <template v-else>
          <label class="field full">证书内容{{ editing?.certificateMode === 'custom' && editing.certificateConfigured ? '（已保存）' : '' }}<textarea v-model="form.certificatePem" :required="customCertificateRequired" rows="7" spellcheck="false" placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
          <label class="field full">私钥内容{{ editing?.certificateMode === 'custom' && editing.certificateConfigured ? '（已保存）' : '' }}<textarea v-model="form.privateKeyPem" :required="customCertificateRequired" rows="7" spellcheck="false" placeholder="-----BEGIN PRIVATE KEY-----"></textarea></label>
        </template>
      </template>
    </div>
  </AppModal>
  <AppModal v-if="nginxModal" :title="`Nginx 配置 · ${nginxSite?.domain || ''}`" wide :busy="nginxLoading" @close="nginxModal = false" @submit="saveNginx">
    <textarea v-model="nginxConfig" class="code-editor" spellcheck="false" :disabled="nginxLoading" aria-label="Nginx 配置"></textarea>
    <div class="editor-meta"><code>{{ nginxConfigPath }}</code><span>{{ nginxCustomized ? '手工配置' : 'HostDesk 托管' }}</span></div>
    <template #actions>
      <button v-if="nginxCustomized" class="button quiet" type="button" :disabled="nginxLoading" @click="resetNginx"><RotateCcw :size="16" />恢复托管配置</button>
      <button class="button quiet" type="button" :disabled="nginxLoading" @click="nginxModal = false">取消</button>
      <button class="button primary" type="submit" :disabled="nginxLoading"><Save :size="16" />保存并重载</button>
    </template>
  </AppModal>
</template>
