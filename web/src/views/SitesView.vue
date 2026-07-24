<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { FolderOpen, Pause, Play, Plus, Settings2, Trash2 } from "@lucide/vue";
import { api } from "../api";
import { errorMessage, useUI } from "../ui";
import AppModal from "../components/AppModal.vue";
import EmptyState from "../components/EmptyState.vue";
import PageHeader from "../components/PageHeader.vue";

interface Site {
  id: string; domain: string; aliases: string[]; type: string; root: string; upstream: string;
  rewriteMode?: string; rewriteRules?: string; enabled: boolean; ssl: boolean;
  certificateMode?: "managed" | "custom"; certificateId?: string; certificateConfigured: boolean;
}
interface CertificateOption { id: string; domains: string[]; expiresAt: string }

const emit = defineEmits<{ openFiles: [path: string] }>();
const { notify, confirm } = useUI();
const sites = ref<Site[]>([]);
const certificates = ref<CertificateOption[]>([]);
const loading = ref(false);
const modal = ref(false);
const editing = ref<Site | null>(null);
const form = reactive({
  domain: "", aliases: "", type: "static", root: "", upstream: "", rewriteMode: "laravel", rewriteRules: "",
  ssl: false, certificateMode: "managed" as "managed" | "custom", certificateId: "", certificatePem: "", privateKeyPem: "",
});
const customCertificateRequired = computed(() => form.ssl && form.certificateMode === "custom" && !(editing.value?.certificateMode === "custom" && editing.value.certificateConfigured));

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

function open(site?: Site) {
  editing.value = site || null;
  const defaultMode = site?.certificateMode || (certificates.value.length ? "managed" : "custom");
  Object.assign(form, site ? {
    domain: site.domain, aliases: site.aliases.join(", "), type: site.type, root: site.root, upstream: site.upstream,
    rewriteMode: site.rewriteMode || "laravel", rewriteRules: site.rewriteRules || "", ssl: site.ssl,
    certificateMode: defaultMode, certificateId: site.certificateId || certificates.value[0]?.id || "", certificatePem: "", privateKeyPem: "",
  } : {
    domain: "", aliases: "", type: "static", root: "", upstream: "", rewriteMode: "laravel", rewriteRules: "", ssl: false,
    certificateMode: defaultMode, certificateId: certificates.value[0]?.id || "", certificatePem: "", privateKeyPem: "",
  });
  modal.value = true;
}

async function save() {
  loading.value = true;
  try {
    const body = {
      domain: form.domain,
      aliases: form.aliases.split(",").map(value => value.trim()).filter(Boolean),
      type: form.type,
      root: form.root,
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

function openFiles(site: Site) { emit("openFiles", site.root); }
onMounted(load);
</script>

<template>
  <PageHeader title="网站管理" subtitle="Nginx 虚拟主机与网站目录" kicker="SITES"><button class="button primary" @click="open()"><Plus :size="16" />添加网站</button></PageHeader>
  <div class="data-surface"><div class="table-scroll"><table class="data-table"><thead><tr><th>域名</th><th>类型</th><th>目录 / 上游</th><th>HTTPS</th><th>状态</th><th class="actions-column">操作</th></tr></thead><tbody><tr v-for="site in sites" :key="site.id"><td><strong>{{ site.domain }}</strong><small v-if="site.aliases.length">{{ site.aliases.join(', ') }}</small></td><td>{{ { static: '静态', php: 'PHP', proxy: '反代' }[site.type] || site.type }}</td><td class="truncate-cell" :title="site.type === 'proxy' ? site.upstream : site.root">{{ site.type === 'proxy' ? site.upstream : site.root }}</td><td>{{ site.ssl ? '已启用' : 'HTTP' }}</td><td><span class="status-pill" :class="{ running: site.enabled, stopped: !site.enabled }">{{ site.enabled ? '启用' : '停用' }}</span></td><td><div class="row-actions"><button v-if="site.type !== 'proxy'" class="button compact" @click="openFiles(site)"><FolderOpen :size="15" />文件</button><button class="icon-button" title="配置" @click="open(site)"><Settings2 :size="16" /></button><button class="icon-button" :title="site.enabled ? '停用' : '启用'" @click="toggle(site)"><Pause v-if="site.enabled" :size="16" /><Play v-else :size="16" /></button><button class="icon-button danger" title="删除" @click="remove(site)"><Trash2 :size="16" /></button></div></td></tr></tbody></table></div><EmptyState v-if="!loading && !sites.length" message="还没有网站" /></div>
  <AppModal v-if="modal" :title="editing ? `修改 ${editing.domain}` : '添加网站'" wide :busy="loading" :submit-label="editing ? '保存配置' : '创建网站'" @close="modal = false" @submit="save">
    <div class="form-grid">
      <label class="field">主域名<input v-model="form.domain" :disabled="!!editing" placeholder="example.com" required></label>
      <label class="field">别名域名<input v-model="form.aliases" placeholder="www.example.com, api.example.com"></label>
      <label class="field">网站类型<select v-model="form.type"><option value="static">静态网站</option><option value="php">PHP 网站</option><option value="proxy">反向代理</option></select></label>
      <label v-if="form.type !== 'proxy'" class="field">网站目录<input v-model="form.root" placeholder="自动生成到 /var/www"></label>
      <label v-else class="field">上游地址<input v-model="form.upstream" placeholder="http://127.0.0.1:3000"></label>
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
</template>
