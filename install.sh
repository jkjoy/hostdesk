#!/usr/bin/env bash

set -Eeuo pipefail

readonly REPOSITORY="jkjoy/hostdesk"
readonly INSTALL_PATH="/usr/local/bin/hostdesk"
readonly CONFIG_PATH="/etc/conf.d/hostdesk"
readonly SERVICE_PATH="/etc/init.d/hostdesk"

ASSUME_YES=false
RELEASE_VERSION="latest"
TEMP_DIR=""

usage() {
  cat <<'EOF'
HostDesk Alpine 安装器

用法:
  install.sh [--version TAG] [--yes]

选项:
  --version TAG  安装指定 Release，例如 v1.0.0；默认安装 latest
  --yes          使用默认配置，不进行交互询问
  -h, --help     显示帮助
EOF
}

log() {
  printf '[hostdesk] %s\n' "$*"
}

die() {
  printf '[hostdesk] 错误: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]]; then
    rm -rf -- "$TEMP_DIR"
  fi
}

trap cleanup EXIT

while (($# > 0)); do
  case "$1" in
    --version)
      (($# >= 2)) || die "--version 缺少参数"
      RELEASE_VERSION="$2"
      shift 2
      ;;
    --yes)
      ASSUME_YES=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "未知参数: $1"
      ;;
  esac
done

[[ "$RELEASE_VERSION" == "latest" || "$RELEASE_VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || \
  die "Release 版本只能包含字母、数字、点、下划线和连字符"
[[ ${EUID:-$(id -u)} -eq 0 ]] || die "请以 root 用户运行此脚本"
[[ -f /etc/alpine-release ]] || die "此安装器仅支持 Alpine Linux"
command -v apk >/dev/null 2>&1 || die "未找到 apk"
command -v rc-service >/dev/null 2>&1 || die "未找到 OpenRC，请确认系统使用 OpenRC"
command -v rc-update >/dev/null 2>&1 || die "未找到 rc-update"

if [[ "$ASSUME_YES" != true && ! -r /dev/tty ]]; then
  die "交互模式需要终端；自动安装请使用 --yes"
fi

prompt() {
  local message="$1"
  local default_value="$2"
  local answer=""

  if [[ "$ASSUME_YES" == true ]]; then
    printf '%s' "$default_value"
    return
  fi

  read -r -p "$message [$default_value]: " answer </dev/tty
  printf '%s' "${answer:-$default_value}"
}

confirm() {
  local message="$1"
  local default_answer="${2:-yes}"
  local hint="[Y/n]"
  local answer=""

  if [[ "$ASSUME_YES" == true ]]; then
    [[ "$default_answer" == "yes" ]]
    return
  fi

  [[ "$default_answer" == "yes" ]] || hint="[y/N]"
  read -r -p "$message $hint: " answer </dev/tty
  answer="${answer:-$default_answer}"
  [[ "$answer" =~ ^[Yy]([Ee][Ss])?$ ]]
}

prompt_port() {
  local value
  while true; do
    value="$(prompt "监听端口" "8787")"
    if [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 1 && value <= 65535)); then
      printf '%s' "$value"
      return
    fi
    log "端口必须是 1 到 65535 之间的整数" >&2
  done
}

prompt_absolute_directory() {
  local message="$1"
  local default_value="$2"
  local must_exist="$3"
  local value

  while true; do
    value="$(prompt "$message" "$default_value")"
    if [[ "$value" != /* || "$value" == *$'\n'* ]]; then
      log "请输入不含换行符的绝对路径" >&2
      continue
    fi
    if [[ "$must_exist" == true && ! -d "$value" ]]; then
      log "目录不存在: $value" >&2
      continue
    fi
    printf '%s' "$value"
    return
  done
}

shell_quote() {
  local value="${1//\'/\'\\\'\'}"
  printf "'%s'" "$value"
}

detect_architecture() {
  case "$(uname -m)" in
    i386|i486|i586|i686|x86)
      printf '386'
      ;;
    x86_64|amd64)
      printf 'amd64'
      ;;
    aarch64|arm64)
      printf 'arm64'
      ;;
    armv7l|armv7)
      printf 'armv7'
      ;;
    *)
      die "不支持的 CPU 架构: $(uname -m)"
      ;;
  esac
}

download_release_asset() {
  local url="$1"
  local output="$2"
  local attempt
  local max_attempts=24

  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    if curl --fail --location --silent --show-error \
      --output "$output" "$url"; then
      return
    fi
    if ((attempt < max_attempts)); then
      log "Release 产物尚未就绪，5 秒后重试 (${attempt}/${max_attempts})"
      sleep 5
    fi
  done

  return 1
}

detect_public_ipv4() {
  local endpoint
  local address

  for endpoint in "https://api.ipify.org" "https://ipv4.icanhazip.com"; do
    if address="$(curl --ipv4 --fail --silent --max-time 5 "$endpoint")"; then
      address="${address//$'\r'/}"
      address="${address//$'\n'/}"
      if [[ "$address" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
        printf '%s' "$address"
        return
      fi
    fi
  done

  return 1
}

write_config() {
  local host="$1"
  local port="$2"
  local file_root="$3"
  local data_dir="$4"
  local cookie_secure="$5"
  local config_temp="$TEMP_DIR/hostdesk.conf"

  {
    printf 'HOST=%s\n' "$(shell_quote "$host")"
    printf 'PORT=%s\n' "$(shell_quote "$port")"
    printf 'FILE_ROOT=%s\n' "$(shell_quote "$file_root")"
    printf 'DATA_DIR=%s\n' "$(shell_quote "$data_dir")"
    printf 'COOKIE_SECURE=%s\n' "$(shell_quote "$cookie_secure")"
    printf 'SSH_HOSTS=%s\n' "$(shell_quote '127.0.0.1,localhost,::1')"
    printf 'export HOST PORT FILE_ROOT DATA_DIR COOKIE_SECURE SSH_HOSTS\n'
  } >"$config_temp"

  chmod 600 "$config_temp"
  mv -f "$config_temp" "$CONFIG_PATH"
}

write_service() {
  local service_temp="$TEMP_DIR/hostdesk.initd"

  cat >"$service_temp" <<'EOF'
#!/sbin/openrc-run

name="HostDesk"
description="HostDesk file manager and server administration panel"
command="/usr/local/bin/hostdesk"
command_background="yes"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/hostdesk.log"
error_log="/var/log/hostdesk.log"

depend() {
    need net
    after firewall
}

start_pre() {
    checkpath --file --mode 0640 "$output_log"
    checkpath --directory --mode 0700 "$DATA_DIR"
}
EOF

  chmod 755 "$service_temp"
  mv -f "$service_temp" "$SERVICE_PATH"
}

log "安装运行依赖"
apk add --no-cache ca-certificates curl >/dev/null

architecture="$(detect_architecture)"
asset="hostdesk-linux-${architecture}"
if [[ "$RELEASE_VERSION" == "latest" ]]; then
  download_base="https://github.com/${REPOSITORY}/releases/latest/download"
else
  download_base="https://github.com/${REPOSITORY}/releases/download/${RELEASE_VERSION}"
fi

TEMP_DIR="$(mktemp -d /tmp/hostdesk-install.XXXXXX)"
log "下载 ${asset} (${RELEASE_VERSION})"
download_release_asset "$download_base/$asset" "$TEMP_DIR/$asset" || \
  die "无法下载 ${asset}，请确认 Release 已完成构建"
download_release_asset "$download_base/checksums.txt" "$TEMP_DIR/checksums.txt" || \
  die "无法下载 checksums.txt，请确认 Release 已完成构建"

awk -v asset="$asset" '$2 == asset { print; found = 1 } END { if (!found) exit 1 }' \
  "$TEMP_DIR/checksums.txt" >"$TEMP_DIR/checksum.selected" || \
  die "checksums.txt 中缺少 ${asset}"
(
  cd "$TEMP_DIR"
  sha256sum -c checksum.selected >/dev/null
) || die "二进制校验失败"

keep_config=false
if [[ -f "$CONFIG_PATH" ]] && confirm "检测到现有配置，是否保留" yes; then
  keep_config=true
fi

if [[ "$keep_config" != true ]]; then
  host="127.0.0.1"
  port="$(prompt_port)"
  file_root="$(prompt_absolute_directory "文件管理根目录" "/" true)"
  data_dir="$(prompt_absolute_directory "数据目录" "/var/lib/hostdesk" false)"
  cookie_secure=false
  if confirm "是否通过本机 HTTPS 反向代理访问" yes; then
    cookie_secure=true
  elif confirm "是否允许公网直接通过明文 HTTP 访问（不推荐）" no; then
    host="0.0.0.0"
    log "警告：公网明文 HTTP 会暴露登录凭据，请配置防火墙和 HTTPS"
  fi
  mkdir -p "$data_dir"
  chmod 700 "$data_dir"
  write_config "$host" "$port" "$file_root" "$data_dir" "$cookie_secure"
fi

had_binary=false
if [[ -f "$INSTALL_PATH" ]]; then
  had_binary=true
  cp -p "$INSTALL_PATH" "${INSTALL_PATH}.previous"
fi

mkdir -p "$(dirname "$INSTALL_PATH")"
cp "$TEMP_DIR/$asset" "${INSTALL_PATH}.new"
chmod 755 "${INSTALL_PATH}.new"
mv -f "${INSTALL_PATH}.new" "$INSTALL_PATH"
write_service
rc-update add hostdesk default >/dev/null

if rc-service hostdesk status >/dev/null 2>&1; then
  service_action=restart
else
  service_action=start
fi

if ! rc-service hostdesk "$service_action"; then
  if [[ "$had_binary" == true && -f "${INSTALL_PATH}.previous" ]]; then
    log "启动失败，恢复上一版本"
    mv -f "${INSTALL_PATH}.previous" "$INSTALL_PATH"
    rc-service hostdesk restart || true
  fi
  die "HostDesk 服务启动失败，请查看 /var/log/hostdesk.log"
fi

log "安装完成"
log "服务状态: rc-service hostdesk status"
log "运行日志: /var/log/hostdesk.log"
if [[ "$keep_config" == true ]]; then
  log "已保留现有配置: $CONFIG_PATH"
else
  if [[ "$host" == "127.0.0.1" ]]; then
    log "本机地址: http://127.0.0.1:${port}"
    if [[ "$cookie_secure" == true ]]; then
      log "请通过本机 HTTPS 反向代理访问，HostDesk 端口不会暴露到公网"
    else
      log "请使用 SSH 端口转发访问，HostDesk 端口不会暴露到公网"
    fi
  elif public_ip="$(detect_public_ipv4)"; then
    log "访问地址: http://${public_ip}:${port}"
    log "警告：当前为公网明文 HTTP，请尽快迁移到 HTTPS 反向代理"
  else
    log "访问地址: http://<服务器公网IP>:${port}"
    log "未能自动获取公网 IP，请使用服务器的实际公网 IP 访问"
    log "警告：当前为公网明文 HTTP，请尽快迁移到 HTTPS 反向代理"
  fi
fi
