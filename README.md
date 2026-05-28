# video_dl

一个基于 `yt-dlp` 的视频下载服务：提供新建下载 HTTP API 和简单 Web 页面，提交网页或视频链接后自动按最高质量下载并用 `ffmpeg` 合并。提交普通网页链接时，会先探测页面里的候选视频，并优先下载估算体积最大的那个。

## 本地运行

需要本机已安装：

- Go 1.24+
- yt-dlp
- ffmpeg

```bash
API_TOKEN=your-secret-token go run .
```

打开 `http://localhost:8080`。

## Docker 运行

```bash
API_TOKEN=your-secret-token \
docker compose up --build
```

下载文件默认保存到 `./downloads`。任务状态只保存在内存中，服务重启后任务列表会清空。
下载过程中产生的音频、视频分片等临时文件默认放在 `/dev/shm/video_dl`，完成后只把最终视频移到下载目录。
Web 页面支持删除单条任务记录、删除选中记录和清空全部任务记录；删除运行中任务会先取消下载。文件名会按跨平台兼容规则过滤非法字符。
已完成任务提供“预览”和“下载文件”两个操作；预览会返回明确的媒体 MIME 类型，下载按钮会强制浏览器保存文件。
Web 新建任务表单支持粘贴浏览器 DevTools 的 `Copy request headers` 原文，后端会解析 Cookie、User-Agent、Referer 等头并传给 yt-dlp，并过滤 `Accept-Encoding`、`Sec-Fetch-*`、`Connection` 等不适合转发的浏览器传输层头。

## 油猴脚本

安装 `video-dl-userscript.user.js` 后，在任意网页的视频/音频元素上会出现下载按钮：

- “选项”：展开页面/直链、代理、CK 三个开关；默认工具条只显示“下载”和“选项”，减少遮挡
- “页面/直链”：控制下载按钮提交当前页面 URL，还是提交当前媒体直链；`blob:` 链接会自动回退到当前页面 URL
- 下载按钮：按当前“页面/直链”模式提交；按住 Shift 可临时反向使用另一种模式
- “代理开/关”：控制该站点提交任务时的 `proxy` 参数，实际代理地址由后端 `PROXY_URL` 环境变量提供
- “CK开/关”：控制该站点提交任务时是否携带 `document.cookie`
- 脚本会随任务提交当前页面可读取的 Cookie、User-Agent、Referer、Accept-Language 等上下文，方便后端访问需要登录态或防盗链的网站

首次使用需在油猴菜单里打开 `Video DL 设置`，填写后端地址和 `API_TOKEN`。

## API

公开 API 只提供新建下载任务，并要求 token：

```bash
curl -X POST http://localhost:8080/api/downloads \
  -H 'Authorization: Bearer your-secret-token' \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/video","proxy":false,"cookie":"sid=...","user_agent":"Mozilla/5.0","referer":"https://example.com/page","raw_headers":"User-Agent: Mozilla/5.0\nCookie: sid=..."}'
```

兼容 `X-API-Token: your-secret-token` 请求头。

如果需要让某个任务走代理，启动服务时配置 `PROXY_URL`，创建任务时传 `proxy: true`：

```bash
curl -X POST http://localhost:8080/api/downloads \
  -H 'Authorization: Bearer your-secret-token' \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/video","proxy":true}'
```

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | HTTP 服务端口 |
| `API_TOKEN` | 无 | 公开 API token，必填 |
| `PROXY_URL` | 无 | yt-dlp 代理地址，例如 `http://127.0.0.1:7890` 或 `socks5://127.0.0.1:1080` |
| `DOWNLOAD_DIR` | `downloads` | 下载目录 |
| `TEMP_DIR` | `/dev/shm/video_dl` | 临时下载目录，默认使用内存文件系统 |
| `WORKERS` | `1` 或 `2` | 并发下载 worker 数 |
| `YT_DLP_BIN` | `yt-dlp` | yt-dlp 可执行文件 |
| `FFMPEG_BIN` | `ffmpeg` | ffmpeg 可执行文件 |

## GitHub Packages

`.github/workflows/docker.yml` 会在推送到 `main`/`master` 或推送 `v*.*.*` tag 时构建并推送镜像：

```text
ghcr.io/dream10201/video_dl
```
