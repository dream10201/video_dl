// ==UserScript==
// @name         Video DL 页面下载助手
// @namespace    https://github.com/dream10201/video_dl
// @version      2.3.0
// @description  在网页视频/音频上显示下载按钮，将当前页面或媒体直链连同浏览器上下文提交到 video_dl 服务。
// @author       dream10201
// @match        *://*/*
// @connect      *
// @grant        GM.xmlHttpRequest
// @grant        GM.setValue
// @grant        GM.getValue
// @grant        GM.registerMenuCommand
// @grant        GM.addStyle
// @run-at       document-end
// @inject-into  content
// ==/UserScript==

(async function () {
    'use strict';

    const CONFIG_KEYS = {
        BACKEND: 'VIDEO_DL_BACKEND_URL',
        TOKEN: 'VIDEO_DL_API_TOKEN'
    };
    const PROXY_KEY_PREFIX = 'VIDEO_DL_PROXY_';
    const COOKIE_KEY_PREFIX = 'VIDEO_DL_COOKIE_';
    const MODE_KEY_PREFIX = 'VIDEO_DL_MODE_';
    const mediaMap = new WeakMap();

    GM.addStyle(`
        .video-dl-btn-group {
            position: fixed;
            z-index: 2147483647;
            display: flex;
            gap: 6px;
            font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            pointer-events: auto;
            opacity: 0;
            transition: opacity .2s ease;
        }
        .video-dl-btn-group:hover,
        .video-dl-btn-group.visible { opacity: 1; }
        .video-dl-btn {
            height: 28px;
            border: 0;
            border-radius: 6px;
            padding: 0 9px;
            color: #fff;
            background: #0f766e;
            box-shadow: 0 2px 8px rgba(0,0,0,.28);
            cursor: pointer;
            font-size: 12px;
            font-weight: 700;
            white-space: nowrap;
        }
        .video-dl-btn:hover { filter: brightness(1.08); }
        .video-dl-btn.proxy-on { background: #287d3c; }
        .video-dl-btn.proxy-off { background: #66707a; }
        .video-dl-btn.cookie-on { background: #7c3aed; }
        .video-dl-btn.cookie-off { background: #66707a; }
        .video-dl-btn.mode-page { background: #0f766e; }
        .video-dl-btn.mode-direct { background: #2563eb; }
        .video-dl-btn.error { background: #b42318; }
        .video-dl-btn.ok { background: #287d3c; }
        .video-dl-overlay {
            position: fixed;
            inset: 0;
            z-index: 2147483648;
            display: flex;
            align-items: center;
            justify-content: center;
            background: rgba(0,0,0,.5);
            font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
        }
        .video-dl-settings {
            width: min(420px, calc(100vw - 32px));
            padding: 18px;
            border-radius: 8px;
            background: #fff;
            color: #1f2428;
            box-shadow: 0 12px 42px rgba(0,0,0,.25);
        }
        .video-dl-settings h3 {
            margin: 0 0 14px;
            font-size: 18px;
        }
        .video-dl-input-group { margin-bottom: 12px; }
        .video-dl-input-group label {
            display: block;
            margin-bottom: 5px;
            font-size: 13px;
            font-weight: 700;
        }
        .video-dl-input-group input {
            width: 100%;
            height: 38px;
            box-sizing: border-box;
            border: 1px solid #d9ded8;
            border-radius: 6px;
            padding: 0 10px;
            font: inherit;
        }
        .video-dl-settings-actions {
            display: flex;
            justify-content: end;
            gap: 8px;
            margin-top: 14px;
        }
        .video-dl-settings-actions button {
            height: 34px;
            border: 0;
            border-radius: 6px;
            padding: 0 12px;
            cursor: pointer;
            font-weight: 700;
        }
        .video-dl-save { background: #0f766e; color: #fff; }
    `);

    function isHttpURL(value) {
        return /^https?:\/\//i.test(value || '');
    }

    function isMediaSource(value) {
        return isHttpURL(value) || /^blob:/i.test(value || '');
    }

    function getMediaSrc(media) {
        let src = media.currentSrc || media.src || '';
        if (!isMediaSource(src)) {
            for (const source of media.querySelectorAll('source')) {
                if (isMediaSource(source.src)) {
                    src = source.src;
                    break;
                }
            }
        }
        return isMediaSource(src) ? src : '';
    }

    function getProxyKey() {
        return `${PROXY_KEY_PREFIX}${location.host}`;
    }

    function getCookieKey() {
        return `${COOKIE_KEY_PREFIX}${location.host}`;
    }

    function getModeKey() {
        return `${MODE_KEY_PREFIX}${location.host}`;
    }

    async function createOverlay(media) {
        if (mediaMap.has(media)) return;

        const container = document.createElement('div');
        container.className = 'video-dl-btn-group';

        const downloadButton = document.createElement('button');
        downloadButton.className = 'video-dl-btn';
        downloadButton.textContent = '下载';
        downloadButton.title = '按当前模式提交下载；按住 Shift 可临时反向使用页面/直链模式';

        const proxyButton = document.createElement('button');
        updateProxyButton(proxyButton, await GM.getValue(getProxyKey(), false));

        const cookieButton = document.createElement('button');
        updateCookieButton(cookieButton, await GM.getValue(getCookieKey(), true));

        const modeButton = document.createElement('button');
        updateModeButton(modeButton, await GM.getValue(getModeKey(), 'page'));

        container.append(downloadButton, modeButton, proxyButton, cookieButton);
        document.body.appendChild(container);

        downloadButton.addEventListener('click', async (event) => {
            event.preventDefault();
            event.stopPropagation();
            await submitDownload(media, downloadButton, event.shiftKey);
        });

        modeButton.addEventListener('click', async (event) => {
            event.preventDefault();
            event.stopPropagation();
            const current = await GM.getValue(getModeKey(), 'page');
            const next = current === 'direct' ? 'page' : 'direct';
            await GM.setValue(getModeKey(), next);
            updateModeButton(modeButton, next);
        });

        proxyButton.addEventListener('click', async (event) => {
            event.preventDefault();
            event.stopPropagation();
            const next = !(await GM.getValue(getProxyKey(), false));
            await GM.setValue(getProxyKey(), next);
            updateProxyButton(proxyButton, next);
        });

        cookieButton.addEventListener('click', async (event) => {
            event.preventDefault();
            event.stopPropagation();
            const next = !(await GM.getValue(getCookieKey(), true));
            await GM.setValue(getCookieKey(), next);
            updateCookieButton(cookieButton, next);
        });

        const controller = {
            container,
            media,
            updatePosition() {
                const rect = media.getBoundingClientRect();
                const visible = rect.width > 50 && rect.height > 50 && rect.top < window.innerHeight && rect.bottom > 0;
                if (!visible) {
                    container.style.display = 'none';
                    return;
                }
                container.style.display = 'flex';
                container.style.top = `${Math.max(0, rect.top + window.scrollY + 6)}px`;
                container.style.left = `${Math.max(0, rect.right + window.scrollX - 260)}px`;
                container.classList.toggle('visible', media.matches(':hover') || container.matches(':hover'));
            },
            remove() {
                container.remove();
                mediaMap.delete(media);
            }
        };

        mediaMap.set(media, controller);
        controller.updatePosition();
    }

    function updateProxyButton(button, enabled) {
        button.textContent = enabled ? '代理开' : '代理关';
        button.className = `video-dl-btn ${enabled ? 'proxy-on' : 'proxy-off'}`;
        button.title = '切换当前站点是否使用后端 PROXY_URL';
    }

    function updateCookieButton(button, enabled) {
        button.textContent = enabled ? 'CK开' : 'CK关';
        button.className = `video-dl-btn ${enabled ? 'cookie-on' : 'cookie-off'}`;
        button.title = '切换当前站点是否随任务发送 document.cookie';
    }

    function updateModeButton(button, mode) {
        const direct = mode === 'direct';
        button.textContent = direct ? '直链' : '页面';
        button.className = `video-dl-btn ${direct ? 'mode-direct' : 'mode-page'}`;
        button.title = direct ? '当前提交媒体直链；blob 链接会回退到页面' : '当前提交页面 URL，让后端自动选择最大视频';
    }

    async function submitDownload(media, button, invertMode) {
        const backend = normalizeBackend(await GM.getValue(CONFIG_KEYS.BACKEND, ''));
        const token = (await GM.getValue(CONFIG_KEYS.TOKEN, '')).trim();
        if (!backend || !token) {
            await showSettings();
            return;
        }

        const mediaSrc = getMediaSrc(media);
        const mode = await GM.getValue(getModeKey(), 'page');
        const direct = invertMode ? mode !== 'direct' : mode === 'direct';
        const targetURL = direct && isHttpURL(mediaSrc) ? mediaSrc : location.href;
        const useProxy = await GM.getValue(getProxyKey(), false);
        const useCookie = await GM.getValue(getCookieKey(), true);
        const originalText = button.textContent;
        const originalClass = button.className;

        button.textContent = '提交中';
        button.disabled = true;

        GM.xmlHttpRequest({
            method: 'POST',
            url: `${backend}/api/downloads`,
            headers: {
                'Authorization': `Bearer ${token}`,
                'Content-Type': 'application/json'
            },
            data: JSON.stringify({
                url: targetURL,
                proxy: useProxy,
                cookie: useCookie ? (document.cookie || '') : '',
                user_agent: navigator.userAgent || '',
                referer: location.href,
                headers: collectBrowserHeaders()
            }),
            responseType: 'json',
            onload(response) {
                if (response.status >= 200 && response.status < 300) {
                    button.textContent = direct && targetURL === mediaSrc ? '已提交直链' : '已提交页面';
                    button.className = 'video-dl-btn ok';
                    return;
                }
                button.textContent = `失败 ${response.status}`;
                button.className = 'video-dl-btn error';
                const message = response.response && response.response.error ? response.response.error : '提交下载失败';
                console.warn('[video_dl]', message, response);
            },
            onerror(error) {
                button.textContent = '网络错误';
                button.className = 'video-dl-btn error';
                console.warn('[video_dl] request failed', error);
            },
            onloadend() {
                setTimeout(() => {
                    button.disabled = false;
                    button.textContent = originalText;
                    button.className = originalClass;
                }, 2500);
            }
        });
    }

    function normalizeBackend(value) {
        return String(value || '').trim().replace(/\/+$/, '');
    }

    function collectBrowserHeaders() {
        const headers = {
            'Accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
            'Origin': location.origin,
            'Referer': location.href,
            'User-Agent': navigator.userAgent || ''
        };
        if (navigator.languages && navigator.languages.length) {
            headers['Accept-Language'] = navigator.languages.join(',');
        } else if (navigator.language) {
            headers['Accept-Language'] = navigator.language;
        }
        return headers;
    }

    function updateAllPositions() {
        document.querySelectorAll('video, audio').forEach((media) => {
            if (getMediaSrc(media)) {
                if (!mediaMap.has(media)) createOverlay(media);
                else mediaMap.get(media).updatePosition();
            } else if (mediaMap.has(media)) {
                mediaMap.get(media).remove();
            }
        });
    }

    const observer = new MutationObserver(updateAllPositions);
    observer.observe(document.body, { childList: true, subtree: true });
    window.addEventListener('scroll', updateAllPositions, { passive: true });
    window.addEventListener('resize', updateAllPositions, { passive: true });
    document.addEventListener('mouseover', updateAllPositions);
    setInterval(updateAllPositions, 2000);

    async function showSettings() {
        if (document.querySelector('.video-dl-overlay')) return;

        const backend = await GM.getValue(CONFIG_KEYS.BACKEND, '');
        const token = await GM.getValue(CONFIG_KEYS.TOKEN, '');
        const overlay = document.createElement('div');
        overlay.className = 'video-dl-overlay';
        overlay.innerHTML = `
            <div class="video-dl-settings">
                <h3>Video DL 设置</h3>
                <div class="video-dl-input-group">
                    <label for="video_dl_backend">后端地址</label>
                    <input id="video_dl_backend" value="${escapeAttribute(backend)}" placeholder="http://127.0.0.1:8080">
                </div>
                <div class="video-dl-input-group">
                    <label for="video_dl_token">API Token</label>
                    <input id="video_dl_token" type="password" value="${escapeAttribute(token)}">
                </div>
                <div class="video-dl-settings-actions">
                    <button type="button" id="video_dl_cancel">取消</button>
                    <button type="button" class="video-dl-save" id="video_dl_save">保存</button>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);

        overlay.querySelector('#video_dl_cancel').addEventListener('click', () => overlay.remove());
        overlay.addEventListener('click', (event) => {
            if (event.target === overlay) overlay.remove();
        });
        overlay.querySelector('#video_dl_save').addEventListener('click', async () => {
            await GM.setValue(CONFIG_KEYS.BACKEND, normalizeBackend(overlay.querySelector('#video_dl_backend').value));
            await GM.setValue(CONFIG_KEYS.TOKEN, overlay.querySelector('#video_dl_token').value.trim());
            overlay.remove();
        });
    }

    function escapeAttribute(value) {
        return String(value || '').replace(/[&<>"']/g, (char) => ({
            '&': '&amp;',
            '<': '&lt;',
            '>': '&gt;',
            '"': '&quot;',
            "'": '&#39;'
        }[char]));
    }

    GM.registerMenuCommand('Video DL 设置', showSettings);
    GM.registerMenuCommand('打开 Video DL 后台', async () => {
        const backend = normalizeBackend(await GM.getValue(CONFIG_KEYS.BACKEND, ''));
        if (backend) window.open(backend, '_blank', 'noopener,noreferrer');
        else showSettings();
    });

    updateAllPositions();
})();
