# HostDesk

HostDesk 是一个单机自托管的文件管理器和 WebSSH 工作台。前端资源全部嵌入 Go 二进制，部署时只需要复制 `hostdesk` 一个文件。

## 功能

- 文件浏览、上传、下载、新建、在线编辑、删除、复制、移动和重命名
- 创建 `.tar.gz`，解压 `.zip`、`.tar`、`.tar.gz` 和 `.tgz`
- 浏览器 SSH 终端，支持密码和后端私钥认证
- 安装和管理 Alpine/OpenRC 上的 Nginx、PHP-FPM 与 MariaDB
- 管理静态、PHP 和反向代理网站，保存前执行 `nginx -t` 并在失败时回滚
- 通过 HTTP-01 或 Cloudflare DNS-01 申请 Let's Encrypt 证书并自动绑定网站
- 每 12 小时检查证书，剩余 30 天时自动续期并安全重载 Nginx
- 管理 PHP 设置与扩展、MariaDB 数据库、用户和授权
- 使用原生 Nginx 直接提供 `80/443` 服务
- 首次使用时创建管理员，SQLite 持久化账号与 Scrypt 密码哈希
- 后台修改管理员用户名和密码，修改后自动撤销其他会话
- 持久化登录限流、递增锁定、HttpOnly 会话、CSRF 校验
- 管理根目录限制、符号链接边界检查、压缩包路径穿越与解压大小防护

## 运行

```sh
chmod +x hostdesk
./hostdesk
```

默认监听 `127.0.0.1:8787`，管理当前用户的主目录。首次打开页面时创建管理员账号和密码，凭据保存在用户配置目录下权限为 `0600` 的 SQLite 数据库中。旧版本的 `config.json` 会在首次启动新版本时自动迁移，原账号和密码保持不变。

常用环境变量：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `HOST` | `127.0.0.1` | 监听地址 |
| `PORT` | `8787` | 监听端口 |
| `FILE_ROOT` | 当前用户主目录 | 可管理的根目录 |
| `DATA_DIR` | 用户配置目录下的 `hostdesk` | SQLite 数据库与服务配置目录 |
| `MAX_UPLOAD_MB` | `256` | 单文件上传上限 |
| `MAX_EXTRACT_MB` | `2048` | 单个压缩包解压后总大小上限 |
| `SSH_HOSTS` | `127.0.0.1,localhost,::1` | 允许连接的 SSH 主机列表 |
| `SSH_HOST_KEY_SHA256` | 空 | 非本机 SSH 必须设置的主机指纹 |
| `COOKIE_SECURE` | `false` | HTTPS 部署时应设为 `true` |

远程访问建议由 Caddy 或 Nginx 提供 HTTPS，并代理到 `127.0.0.1:8787`。不要直接把明文 HTTP 暴露到公网。

服务器管理功能面向 Alpine Linux + OpenRC。Nginx 配置由 HostDesk 直接管理，保存设置或网站前会执行配置检查，失败时自动恢复原配置。

DNS 验证支持 Cloudflare API Token，Token 需要 `Zone:Read` 和 `DNS:Edit` 权限。凭据使用本机随机密钥加密，并保存在权限为 `0600` 的 HostDesk 数据文件中。HTTP 验证要求域名的 80 端口能够访问当前主机；通配符证书必须使用 DNS 验证。

## 构建

```sh
go test ./...
go build -trimpath -ldflags='-s -w' -o hostdesk .
```

构建需要 Go 1.26.3 或更高版本。运行二进制不需要 Go、Node.js、Python、`tar` 或 `unzip`。
