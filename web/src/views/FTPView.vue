<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { Download, Pencil, Play, Plus, RefreshCw, Trash2 } from "@lucide/vue";
import { api } from "../api";
import { displayDate, errorMessage, useUI } from "../ui";
import AppModal from "../components/AppModal.vue";
import EmptyState from "../components/EmptyState.vue";
import PageHeader from "../components/PageHeader.vue";

interface User { username: string; home: string; siteId: string; siteDomain: string; createdAt: string; systemPresent: boolean }
interface Site { id: string; domain: string; root: string }
interface Status { installed: boolean; running: boolean; service: string }

const { notify, confirm } = useUI();
const users = ref<User[]>([]);
const sites = ref<Site[]>([]);
const status = ref<Status>({ installed: false, running: false, service: "vsftpd" });
const root = ref("/var/www");
const passive = ref("40000-40100");
const loading = ref(false);
const modal = ref(false);
const editing = ref<User | null>(null);
const form = reactive({ username: "", password: "", confirm: "", siteId: "" });
const selectedSite = computed(() => sites.value.find((site) => site.id === form.siteId));

async function load() {
  loading.value = true;
  try {
    const data = await api<{ status: Status; users: User[]; sites: Site[]; root: string; passivePorts: string }>("/api/admin/ftp");
    status.value = data.status;
    users.value = data.users;
    sites.value = data.sites;
    root.value = data.root;
    passive.value = data.passivePorts;
  } catch (error) {
    notify(errorMessage(error), "error");
  } finally {
    loading.value = false;
  }
}

function open(item?: User) {
  if (!sites.value.length) {
    notify("请先创建静态或 PHP 网站", "error");
    return;
  }
  editing.value = item || null;
  const currentSite = item && sites.value.some((site) => site.id === item.siteId) ? item.siteId : sites.value[0].id;
  Object.assign(form, { username: item?.username || "", password: "", confirm: "", siteId: currentSite });
  modal.value = true;
}

async function save() {
  if (!editing.value && !form.password) return notify("请输入 FTP 密码", "error");
  if (form.password !== form.confirm) return notify("两次输入的密码不一致", "error");
  loading.value = true;
  try {
    await api(editing.value ? `/api/admin/ftp/users/${encodeURIComponent(editing.value.username)}` : "/api/admin/ftp/users", {
      method: editing.value ? "PUT" : "POST",
      body: { username: form.username, password: form.password, siteId: form.siteId },
    });
    notify(editing.value ? "FTP 用户已更新" : "FTP 用户已创建");
    modal.value = false;
    await load();
  } catch (error) {
    notify(errorMessage(error), "error");
  } finally {
    loading.value = false;
  }
}

async function install() {
  loading.value = true;
  try {
    await api("/api/admin/components/ftp/install", { method: "POST" });
    notify("FTP 服务安装完成");
    await load();
  } catch (error) {
    notify(errorMessage(error), "error");
  } finally {
    loading.value = false;
  }
}

async function service() {
  loading.value = true;
  try {
    await api(`/api/admin/services/${status.value.service || "vsftpd"}/${status.value.running ? "restart" : "start"}`, { method: "POST" });
    notify("FTP 服务状态已更新");
    await load();
  } catch (error) {
    notify(errorMessage(error), "error");
  } finally {
    loading.value = false;
  }
}

async function remove(item: User) {
  if (!await confirm(`删除 FTP 用户 ${item.username}`, `系统账号会被删除，网站文件 ${item.home} 将保留。`, "删除", true)) return;
  try {
    await api(`/api/admin/ftp/users/${encodeURIComponent(item.username)}`, { method: "DELETE" });
    notify("FTP 用户已删除，网站文件已保留");
    await load();
  } catch (error) {
    notify(errorMessage(error), "error");
  }
}

onMounted(load);
</script>

<template>
  <PageHeader title="FTP" :subtitle="!status.installed ? 'vsftpd 尚未安装' : status.running ? 'vsftpd 运行中' : 'vsftpd 已停止'" kicker="FILE TRANSFER">
    <button v-if="!status.installed" class="button primary" :disabled="loading" @click="install"><Download :size="16" />安装 FTP</button>
    <template v-else>
      <button class="button" :disabled="loading" @click="service"><RefreshCw v-if="status.running" :size="16" /><Play v-else :size="16" />{{ status.running ? '重启服务' : '启动服务' }}</button>
      <button class="button primary" :disabled="!sites.length" :title="sites.length ? '添加 FTP 用户' : '请先创建网站'" @click="open()"><Plus :size="16" />添加用户</button>
    </template>
  </PageHeader>
  <div class="summary-strip"><div><span>网站目录</span><strong>{{ root }}</strong></div><div><span>被动端口</span><strong>{{ passive }}</strong></div></div>
  <section class="section-block">
    <div class="data-surface">
      <div class="table-scroll">
        <table class="data-table wide-table">
          <thead><tr><th>用户</th><th>绑定网站</th><th>登录根目录</th><th>创建时间</th><th>系统状态</th><th></th></tr></thead>
          <tbody><tr v-for="item in users" :key="item.username"><td><strong>{{ item.username }}</strong></td><td>{{ item.siteDomain || '未绑定' }}</td><td>{{ item.home }}</td><td>{{ displayDate(item.createdAt) }}</td><td><span class="status-pill" :class="{ running: item.systemPresent, stopped: !item.systemPresent }">{{ item.systemPresent ? '正常' : '系统用户缺失' }}</span></td><td><div class="row-actions"><button class="icon-button" title="编辑 FTP 用户" @click="open(item)"><Pencil :size="16" /></button><button class="icon-button danger" title="删除用户" @click="remove(item)"><Trash2 :size="16" /></button></div></td></tr></tbody>
        </table>
      </div>
      <EmptyState v-if="!loading && !users.length" message="暂无 FTP 用户" />
    </div>
  </section>
  <AppModal v-if="modal" :title="editing ? `编辑 ${editing.username}` : '添加 FTP 用户'" :busy="loading" @close="modal = false" @submit="save">
    <div class="form-grid one-column">
      <label class="field">用户名<input v-model="form.username" :disabled="!!editing" pattern="[a-z_][a-z0-9_-]{2,31}" required></label>
      <label class="field">绑定网站<select v-model="form.siteId" required><option v-for="site in sites" :key="site.id" :value="site.id">{{ site.domain }}</option></select></label>
      <label class="field">密码<input v-model="form.password" type="password" minlength="12" :required="!editing" autocomplete="new-password" :placeholder="editing ? '留空表示不修改' : ''"></label>
      <label class="field">确认密码<input v-model="form.confirm" type="password" minlength="12" :required="!editing"></label>
      <p class="field-note">登录后根目录：{{ selectedSite?.root || '-' }}</p>
    </div>
  </AppModal>
</template>
