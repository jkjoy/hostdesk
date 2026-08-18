package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const defaultStaticIndexTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light">
  <title>{{DOMAIN}}</title>
  <style>
    :root { color-scheme: light; font-family: Inter, "Noto Sans SC", system-ui, sans-serif; color: #17201d; background: #f3f6f4; }
    * { box-sizing: border-box; }
    body { min-height: 100vh; margin: 0; display: grid; grid-template-rows: auto 1fr auto; }
    header, footer { width: min(1120px, calc(100% - 40px)); margin: 0 auto; display: flex; align-items: center; justify-content: space-between; }
    header { height: 72px; border-bottom: 1px solid #d9e1dd; }
    header strong { font-size: 13px; letter-spacing: 0; }
    header span, footer { color: #66716c; font-size: 12px; }
    main { width: min(1120px, calc(100% - 40px)); margin: 0 auto; display: grid; align-items: center; }
    .hero { padding: 64px 0; display: grid; grid-template-columns: minmax(0, 1.35fr) minmax(280px, .65fr); gap: 56px; align-items: end; }
    .status { display: inline-flex; align-items: center; gap: 8px; margin: 0 0 22px; color: #237456; font-size: 13px; font-weight: 700; }
    .status i { width: 9px; height: 9px; border-radius: 50%; background: #2ba676; box-shadow: 0 0 0 5px #dceee6; }
    h1 { max-width: 760px; margin: 0; overflow-wrap: anywhere; font-size: clamp(42px, 5.5vw, 72px); line-height: 1.02; letter-spacing: 0; }
    .intro { max-width: 640px; margin: 28px 0 0; color: #59645f; font-size: 18px; line-height: 1.8; }
    .facts { border-top: 3px solid #17201d; }
    .facts div { min-height: 76px; display: grid; grid-template-columns: 100px 1fr; align-items: center; border-bottom: 1px solid #cfd8d3; }
    .facts span { color: #74807a; font-size: 12px; }
    .facts strong { font-size: 14px; text-align: right; overflow-wrap: anywhere; }
    footer { min-height: 64px; border-top: 1px solid #d9e1dd; }
    footer a { color: #237456; text-decoration: none; }
    @media (max-width: 760px) { .hero { grid-template-columns: 1fr; gap: 48px; padding: 44px 0; } h1 { font-size: 44px; } .intro { font-size: 16px; } }
  </style>
</head>
<body>
  <header><strong>HOSTDESK</strong><span>STATIC SITE</span></header>
  <main>
    <section class="hero">
      <div>
        <p class="status"><i></i>网站运行正常</p>
        <h1>{{DOMAIN}}</h1>
        <p class="intro">站点已经创建完成。上传网站文件后，这个默认页面将由你的内容替代。</p>
      </div>
      <div class="facts">
        <div><span>DOMAIN</span><strong>{{DOMAIN}}</strong></div>
        <div><span>SERVER</span><strong>HostDesk</strong></div>
        <div><span>STATUS</span><strong>Ready</strong></div>
      </div>
    </section>
  </main>
  <footer><span>Managed by HostDesk</span><a href="/">刷新页面</a></footer>
</body>
</html>
`

const default404Template = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light">
  <title>404 - {{DOMAIN}}</title>
  <style>
    :root { color-scheme: light; font-family: Inter, "Noto Sans SC", system-ui, sans-serif; color: #18201d; background: #f3f6f4; }
    * { box-sizing: border-box; }
    body { min-height: 100vh; margin: 0; display: grid; place-items: center; padding: 28px; }
    main { width: min(760px, 100%); border-top: 4px solid #e06d55; padding-top: 28px; }
    .code { margin: 0; color: #e06d55; font-size: clamp(88px, 22vw, 180px); font-weight: 800; line-height: .86; letter-spacing: 0; }
    h1 { margin: 38px 0 12px; font-size: clamp(28px, 5vw, 48px); letter-spacing: 0; }
    p { max-width: 560px; margin: 0; color: #65706b; font-size: 16px; line-height: 1.7; }
    .actions { margin-top: 32px; display: flex; align-items: center; gap: 22px; }
    a { min-height: 42px; padding: 0 18px; display: inline-flex; align-items: center; border: 1px solid #18201d; color: #18201d; font-size: 14px; font-weight: 700; text-decoration: none; }
    span { color: #7b8580; font-size: 12px; overflow-wrap: anywhere; }
  </style>
</head>
<body>
  <main>
    <p class="code">404</p>
    <h1>没有找到这个页面</h1>
    <p>地址可能已经改变，或者页面尚未发布。你可以返回网站首页继续访问。</p>
    <div class="actions"><a href="/">返回首页</a><span>{{DOMAIN}}</span></div>
  </main>
</body>
</html>
`

const defaultPHPProbeTemplate = `<?php
declare(strict_types=1);

$requestPath = parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH);
if ($requestPath !== '/' && $requestPath !== '/index.php') {
    http_response_code(404);
    $notFoundPage = __DIR__ . '/404.html';
    if (is_file($notFoundPage)) {
        readfile($notFoundPage);
        exit;
    }
}

function probe_escape(mixed $value): string
{
    if ($value === false || $value === null || $value === '') {
        return 'Not set';
    }
    return htmlspecialchars((string) $value, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8');
}

$domain = '{{DOMAIN}}';
$extensions = get_loaded_extensions();
sort($extensions, SORT_NATURAL | SORT_FLAG_CASE);
$rows = [
    'PHP version' => PHP_VERSION,
    'SAPI' => PHP_SAPI,
    'Operating system' => PHP_OS_FAMILY . ' / ' . php_uname('m'),
    'Loaded php.ini' => php_ini_loaded_file() ?: 'Not set',
    'Document root' => $_SERVER['DOCUMENT_ROOT'] ?? __DIR__,
    'Memory limit' => ini_get('memory_limit'),
    'Upload limit' => ini_get('upload_max_filesize'),
    'POST limit' => ini_get('post_max_size'),
    'Max execution time' => ini_get('max_execution_time') . 's',
    'Timezone' => date_default_timezone_get(),
    'OPcache' => extension_loaded('Zend OPcache') && ini_get('opcache.enable') ? 'Enabled' : 'Disabled',
];
?>
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light">
  <title><?= probe_escape($domain) ?> - PHP Probe</title>
  <style>
    :root { color-scheme: light; font-family: Inter, "Noto Sans SC", system-ui, sans-serif; color: #17201d; background: #f3f6f4; }
    * { box-sizing: border-box; }
    body { min-height: 100vh; margin: 0; }
    header { background: #17201d; color: #f4f7f5; }
    .bar, main { width: min(1080px, calc(100% - 40px)); margin: 0 auto; }
    .bar { min-height: 76px; display: flex; align-items: center; justify-content: space-between; gap: 20px; }
    .brand { font-size: 13px; font-weight: 800; }
    .online { display: flex; align-items: center; gap: 8px; color: #86ddb8; font-size: 12px; }
    .online i { width: 8px; height: 8px; border-radius: 50%; background: #49c990; }
    main { padding: 54px 0 70px; }
    .heading { display: flex; align-items: end; justify-content: space-between; gap: 28px; padding-bottom: 30px; border-bottom: 1px solid #ccd5d0; }
    .eyebrow { margin: 0 0 10px; color: #247657; font-size: 12px; font-weight: 800; }
    h1 { margin: 0; overflow-wrap: anywhere; font-size: clamp(34px, 6vw, 64px); line-height: 1.05; letter-spacing: 0; }
    .version { color: #e06d55; font: 700 18px ui-monospace, monospace; white-space: nowrap; }
    h2 { margin: 0 0 16px; font-size: 16px; letter-spacing: 0; }
    section { padding-top: 38px; }
    .parameters { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); border-top: 2px solid #17201d; }
    .row { min-height: 60px; padding: 12px 14px; display: grid; grid-template-columns: minmax(120px, .8fr) minmax(0, 1.2fr); align-items: center; gap: 16px; border-bottom: 1px solid #d4dcd8; }
    .row:nth-child(odd) { border-right: 1px solid #d4dcd8; }
    .row span { color: #6e7973; font-size: 12px; }
    .row strong { overflow-wrap: anywhere; font: 600 12px/1.5 ui-monospace, monospace; text-align: right; }
    .extensions { display: flex; flex-wrap: wrap; gap: 7px; }
    .extensions span { padding: 6px 9px; border: 1px solid #c9d3ce; background: #fff; font: 11px ui-monospace, monospace; }
    footer { margin-top: 44px; padding-top: 20px; display: flex; justify-content: space-between; border-top: 1px solid #ccd5d0; color: #77827c; font-size: 11px; }
    @media (max-width: 720px) { .heading { display: block; } .version { display: block; margin-top: 18px; } .parameters { grid-template-columns: 1fr; } .row:nth-child(odd) { border-right: 0; } .row { grid-template-columns: 1fr; gap: 5px; } .row strong { text-align: left; } }
  </style>
</head>
<body>
  <header><div class="bar"><span class="brand">HOSTDESK PHP PROBE</span><span class="online"><i></i>PHP-FPM Online</span></div></header>
  <main>
    <div class="heading"><div><p class="eyebrow">RUNTIME STATUS</p><h1><?= probe_escape($domain) ?></h1></div><span class="version">PHP <?= probe_escape(PHP_VERSION) ?></span></div>
    <section><h2>运行参数</h2><div class="parameters"><?php foreach ($rows as $label => $value): ?><div class="row"><span><?= probe_escape($label) ?></span><strong><?= probe_escape($value) ?></strong></div><?php endforeach; ?></div></section>
    <section><h2>已加载扩展（<?= count($extensions) ?>）</h2><div class="extensions"><?php foreach ($extensions as $extension): ?><span><?= probe_escape($extension) ?></span><?php endforeach; ?></div></section>
    <footer><span>Generated by HostDesk</span><span><?= probe_escape(date('Y-m-d H:i:s T')) ?></span></footer>
  </main>
</body>
</html>
`

func defaultSiteFile(template, domain string) []byte {
	return []byte(strings.ReplaceAll(template, "{{DOMAIN}}", domain))
}

func defaultStaticIndex(domain string) []byte {
	return defaultSiteFile(defaultStaticIndexTemplate, domain)
}
func defaultNotFoundPage(domain string) []byte { return defaultSiteFile(default404Template, domain) }
func defaultPHPProbe(domain string) []byte     { return defaultSiteFile(defaultPHPProbeTemplate, domain) }

func ensureDefaultSiteFiles(site siteDefinition) error {
	files := map[string][]byte{"404.html": defaultNotFoundPage(site.Domain)}
	if site.Type == "php" {
		files["index.php"] = defaultPHPProbe(site.Domain)
	} else {
		files["index.html"] = defaultStaticIndex(site.Domain)
	}
	documentRoot := siteDocumentRoot(site)
	for name, content := range files {
		filename := filepath.Join(documentRoot, name)
		if _, err := os.Stat(filename); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(filename, content, 0644); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}
