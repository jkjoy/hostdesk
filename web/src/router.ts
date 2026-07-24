import { defineComponent, type Component } from "vue";
import { createRouter, createWebHistory } from "vue-router";
import FilesView from "./views/FilesView.vue";
import SitesView from "./views/SitesView.vue";
import CertificatesView from "./views/CertificatesView.vue";
import NginxView from "./views/NginxView.vue";
import PHPView from "./views/PHPView.vue";
import DatabasesView from "./views/DatabasesView.vue";
import FTPView from "./views/FTPView.vue";
import ContainersView from "./views/ContainersView.vue";
import TerminalView from "./views/TerminalView.vue";
import AccountView from "./views/AccountView.vue";
import ServerSettingsView from "./views/ServerSettingsView.vue";

export type ViewID = "overview" | "files" | "sites" | "certificates" | "nginx" | "php" | "databases" | "ftp" | "containers" | "terminal" | "account" | "server-settings";

const OverviewRoute = defineComponent({ name: "OverviewRoute", render: () => null });

const viewRoutes: Array<[ViewID, string, Component]> = [
  ["overview", "/overview", OverviewRoute],
  ["files", "/files", FilesView],
  ["sites", "/sites", SitesView],
  ["certificates", "/certificates", CertificatesView],
  ["nginx", "/nginx", NginxView],
  ["php", "/php", PHPView],
  ["databases", "/databases", DatabasesView],
  ["ftp", "/ftp", FTPView],
  ["containers", "/containers", ContainersView],
  ["terminal", "/terminal", TerminalView],
  ["account", "/account", AccountView],
  ["server-settings", "/server-settings", ServerSettingsView],
];

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", redirect: { name: "overview" } },
    ...viewRoutes.map(([name, path, component]) => ({ name, path, component, meta: { view: name } })),
    { path: "/:pathMatch(.*)*", redirect: { name: "overview" } },
  ],
});
