import { inject, type InjectionKey } from "vue";

export interface UIContext {
  notify: (message: string, kind?: "success" | "error") => void;
  confirm: (title: string, message: string, confirmText?: string, danger?: boolean) => Promise<boolean>;
}

export const uiKey: InjectionKey<UIContext> = Symbol("hostdesk-ui");

export function useUI() {
  const value = inject(uiKey);
  if (!value) throw new Error("HostDesk UI context is unavailable");
  return value;
}

export function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : "操作失败";
}

export function formatBytes(bytes = 0, directory = false) {
  if (directory) return "-";
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) { value /= 1024; index += 1; }
  return `${value >= 10 || index === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

export function displayDate(value?: string, dateOnly = false) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return value;
  return dateOnly ? date.toLocaleDateString("zh-CN") : date.toLocaleString("zh-CN", { hour12: false });
}
