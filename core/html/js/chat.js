// Универсальный движок чат-ленты. Все сообщения остаются конвертами
// {kind, payload}; внешний вид задают template.html и ресурсы выбранной темы.

import '../sass/chat.sass';
import {serviceLogo} from './service-icons.js';
import {resolveRoom, apply} from './room-view.js';

const API_CHAT = '/api/chat';
const state = {
    socket: null,
    themeId: 'system',
    // Тема, заданная адресом (?theme=). Отличается от themeId, когда тема пришла
    // из комнаты: адрес здесь главнее, иначе можно было бы неделю смотреть не
    // на ту тему, которую просил.
    themeFromURL: null,
    themeKey: '',
    templateSource: '',
    entries: [],
    channels: new Map(),
    names: new Map(),
    plugins: new Map(),
    filter: '',
    onlyLines: false,
    stuck: true,
    seen: 0,
    room: null,
    roomProblem: '',
    shown: null,
};

const feedEl = document.getElementById('feed');

// Настройки комнаты кладём на элемент, к которому относятся правила темы, а не
// на #feed: в разметке .feed — это внешний main, а #feed вложен в него как
// .messages. Переменная, заданная на потомке, вверх не поднимается, и
// «font-family: var(--room-font)» на .feed брало бы заглушку. Размеры шрифта от
// этого не ломались — их читают правила на самом тексте реплики, он ниже и
// переменную наследует, но гарнитура задавалась на .feed и не работала.
const roomTarget = feedEl.closest('.feed') ?? feedEl;
const scrollEl = document.querySelector('.feed');
const stateEl = document.getElementById('state');
const stateTextEl = document.getElementById('state-text');
const summaryEl = document.getElementById('summary');
const channelEl = document.getElementById('channel-filter');
const onlyChatsEl = document.getElementById('only-chats');

const LINE = 'chat.message.public';
const PRIVATE = 'chat.message.private';

// --- Подключение к ядру ------------------------------------------------------

function connect(themeId = state.themeId) {
    state.themeId = themeId || 'system';
    const url = new URL(API_CHAT, location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    url.searchParams.set('theme', state.themeId);

    const socket = new WebSocket(url);
    state.socket = socket;
    let incoming = Promise.resolve();

    socket.addEventListener('open', () => setConnection('online', 'на связи'));
    socket.addEventListener('message', (event) => {
        let message;
        try {
            message = JSON.parse(event.data);
        } catch {
            return;
        }
        // Снимок должен закончить загрузку темы до того, как отрисуем историю
        // или следующую живую реплику. Поэтому обработка конвертов строго по очереди.
        incoming = incoming.then(() => handle(message)).catch((error) => {
            console.error('chat: не удалось обработать сообщение ядра', error);
        });
    });
    socket.addEventListener('close', () => {
        if (state.socket !== socket) return;
        setConnection('offline', 'нет связи');
        setTimeout(() => {
            if (state.socket === socket) connect(state.themeId);
        }, 2000);
    });
    socket.addEventListener('error', () => socket.close());
}

async function handle(message) {
    if (message.kind === 'system.chat.snapshot') {
        const snapshot = message.payload ?? {};
        await applyTheme(snapshot.theme);

        state.plugins.clear();
        state.names.clear();
        for (const plugin of snapshot.plugins ?? []) {
            state.plugins.set(plugin.plugin, plugin);
        }
        state.entries = [];
        state.channels.clear();
        state.seen = 0;
        for (const entry of snapshot.history ?? []) {
            take(entry);
        }
        redrawChannelFilter();
        redraw();
        return;
    }

    // Настройки комнаты поменяли в другой вкладке. Лента не перерисовывается и
    // переподключается: она перечитывает комнату и применяет её на живом месте.
    if (message.kind === 'system.rooms.changed') {
        await refreshRoom();
        return;
    }

    if (message.kind) {
        take(message);
        redraw();
    }
}

function take(message) {
    // Здесь payload разворачивается только для логики фильтров. Шаблону передаём
    // обратно обычный конверт с вложенным payload, ничего не переименовывая.
    const entry = {kind: message.kind, ...(message.payload ?? {})};
    state.entries.push(entry);
    state.seen++;

    if (entry.channel && entry.name && namesChannel(entry.kind)) {
        state.names.set(entry.channel, entry.name);
    }

    if (entry.channel) {
        const known = state.channels.get(entry.channel) ?? {name: '', plugin: entry.plugin, count: 0};
        known.count++;
        if (entry.kind === 'chat.channel.info' && entry.name) known.name = entry.name;
        known.plugin = entry.plugin;
        state.channels.set(entry.channel, known);
        redrawChannelFilterSoon();
    }

    if (isLine(entry)) markSeen();
    // Скрипты темы могут подключать обработчики событий, но не подменяют протокол.
    document.dispatchEvent(new CustomEvent('yudole:message', {detail: message}));
}

function isLine(entry) {
    return entry.kind === LINE || entry.kind === PRIVATE;
}

// --- Загрузка темы -----------------------------------------------------------

async function applyTheme(descriptor) {
    // Старый сервер/отсутствующая тема не должны превращать чат в пустую страницу.
    const theme = descriptor ?? {
        id: 'system',
        title: 'Системная',
        template: '/themes/system/template.html',
        styles: ['/themes/system/style.css'],
        scripts: [],
    };
    const key = JSON.stringify(theme);
    if (key === state.themeKey && state.templateSource) return;

    let templateSource = '';
    try {
        const response = await fetch(theme.template || '/themes/system/template.html', {cache: 'no-store'});
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        templateSource = await response.text();
    } catch (error) {
        console.warn('chat: шаблон темы не загрузился', error);
        templateSource = '<template><article class="msg"><div class="msg__text">{{payload.message.text}}</div></article></template>';
    }

    document.querySelectorAll('[data-theme-style], [data-theme-script]').forEach((node) => node.remove());
    state.templateSource = templateSource;
    state.themeKey = key;

    for (const href of theme.styles ?? []) {
        await loadStyle(href);
    }
    for (const src of theme.scripts ?? []) {
        await loadScript(src);
    }
    document.dispatchEvent(new CustomEvent('yudole:theme-ready', {detail: {theme}}));
}

function loadStyle(href) {
    return new Promise((resolve) => {
        const link = document.createElement('link');
        link.rel = 'stylesheet';
        link.href = href;
        link.dataset.themeStyle = '';
        link.addEventListener('load', resolve, {once: true});
        link.addEventListener('error', resolve, {once: true});
        document.head.append(link);
    });
}

function loadScript(src) {
    return new Promise((resolve) => {
        const script = document.createElement('script');
        script.src = src;
        script.async = false;
        script.dataset.themeScript = '';
        script.addEventListener('load', resolve, {once: true});
        script.addEventListener('error', resolve, {once: true});
        document.body.append(script);
    });
}

// --- Рендер сообщений из template.html --------------------------------------

// Макросы — dot-path в исходном конверте. Неизвестный путь/поле возвращает
// пустую строку; единственное поле, вставляемое как HTML, — message.html.
function line(entry) {
    const node = renderTemplate(entry);
    if (!node) return null;
    if (state.shown && !state.shown.has(entry.kind)) node.style.display = 'none';
    return node;
}

function renderTemplate(entry) {
    const envelope = {kind: entry.kind, payload: entry};
    const source = state.templateSource || '<template><article class="msg"><div class="msg__text">{{payload.message.text}}</div></article></template>';
    const markup = source.replace(/\{\{\s*([\w.]+)\s*\}\}/g, (token, path) => {
        const value = path.split('.').reduce((current, key) => current == null ? undefined : current[key], envelope);
        // system.* — то, что даёт движок, а не пришло в конверте. payload.*
        // достаётся из сообщения, system.* собирается на странице, и по
        // названию видно, откуда взялось значение.
        //
        // system.service.icon — значок сервиса, откуда пришла реплика. Разметка,
        // а не текст: по нику не видно, писали из VK или с YouTube, а тема сама
        // решает, где ему стоять. Раньше его рисовал движок, и шаблон темы
        // значка оставить не мог.
        if (path === 'system.service.icon') {
            const logo = serviceLogo(entry.plugin, 16);
            return logo ? `<span class="msg__service">${logo.outerHTML}</span>` : '';
        }
        if (path === 'payload.message.html') {
            if (typeof value === 'string' && value !== '') return value;
            const fallback = entry.message?.text ?? '';
            return escapeHTML(String(fallback));
        }
        if (value === null || value === undefined) return '';
        if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
            return escapeHTML(String(value));
        }
        return '';
    });

    const fragment = document.createElement('template');
    fragment.innerHTML = markup;
    let root = fragment.content.firstElementChild;
    if (root?.tagName === 'TEMPLATE') root = root.content.firstElementChild;
    if (!root) return null;

    const node = root.cloneNode(true);
    node.classList.add('msg');
    node.dataset.kind = entry.kind;
    node.dataset.severity = severityOf(entry);
    if (entry.notice) node.classList.add('is-notice');
    return node;
}

function escapeHTML(value) {
    return value.replace(/[&<>"']/g, (char) => ({
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#39;',
    })[char]);
}

function redraw() {
    const shown = state.entries.filter(passes);
    const trimmed = shown.slice(-100).map(line).filter(Boolean);
    feedEl.replaceChildren(...trimmed);
    if (state.stuck) scrollEl.scrollTop = scrollEl.scrollHeight;
    updateSummary(trimmed);
}

function passes(entry) {
    if (state.onlyLines && !isLine(entry)) return false;
    if (state.filter && entry.channel !== state.filter) return false;
    return true;
}

function severityOf(entry) {
    switch (entry.kind) {
        case 'chat.connection.state':
            if (entry.state === 'online') return 'success';
            return entry.state === 'offline' ? 'error' : 'info';
        case 'chat.channel.join.success':
            return 'success';
        case 'chat.channel.join.failed':
            return 'error';
        case 'chat.channel.unjoin.success':
            return 'info';
        case 'chat.channel.unjoin.failed':
        case 'chat.message.deleted':
        case 'system.message.warning':
            return 'warning';
        default:
            return 'neutral';
    }
}

function namesChannel(kind) {
    return kind === 'chat.channel.join.success'
        || kind === 'chat.channel.join.failed'
        || kind === 'chat.channel.unjoin.success'
        || kind === 'chat.channel.unjoin.failed';
}

function channelName(id) {
    if (!id) return '';
    return state.names.get(id) || state.channels.get(id)?.name || id;
}

function channelTitle(id) {
    const known = state.channels.get(id);
    const name = channelName(id);
    if (!known || !known.name || known.name === name) return name;
    return `${name} — «${known.name}»`;
}

function updateSummary(shown) {
    const lines = shown.filter(isLine).length;
    const total = state.entries.filter(isLine).length;
    const hidden = total - lines;
    const parts = [`реплик: ${total}`];
    if (hidden > 0) parts.push(`показано: ${lines}`);
    if (state.channels.size) parts.push(`каналов: ${state.channels.size}`);
    summaryEl.textContent = parts.join(' · ');
}

let filterTimer = 0;
function redrawChannelFilterSoon() {
    clearTimeout(filterTimer);
    filterTimer = setTimeout(redrawChannelFilter, 500);
}

function redrawChannelFilter() {
    const chosen = state.filter;
    const options = [el('option', {value: ''}, ['все каналы'])];
    for (const [id] of [...state.channels].sort()) {
        options.push(el('option', {value: id}, [channelTitle(id)]));
    }
    channelEl.replaceChildren(...options);
    channelEl.value = state.channels.has(chosen) || chosen === '' ? chosen : '';
    state.filter = channelEl.value;
}

channelEl.addEventListener('change', () => {
    state.filter = channelEl.value;
    redraw();
});
onlyChatsEl.addEventListener('change', () => {
    state.onlyLines = onlyChatsEl.checked;
    redraw();
});
scrollEl.addEventListener('scroll', () => {
    const gap = scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
    state.stuck = gap < 40;
});

function markSeen() {
    document.title = 'Yudole studio light — чат';
}
function setConnection(name, text) {
    stateEl.dataset.state = name;
    stateTextEl.textContent = text;
}
function el(tag, attrs = {}, children = []) {
    const node = document.createElement(tag);
    for (const [name, value] of Object.entries(attrs)) {
        if (name === 'class') node.className = value;
        else if (value !== undefined && value !== null) node.setAttribute(name, value);
    }
    for (const child of children) node.append(child);
    return node;
}
function showProblem(text) {
    const bar = document.getElementById('room-problem');
    if (!bar) return;
    bar.textContent = text;
    bar.hidden = !text;
}

// setupRoom — первый раз: прочитать комнату и подключиться к ядру.
async function setupRoom() {
    state.themeFromURL = new URLSearchParams(location.search).get('theme');
    await refreshRoom();
    connect(state.themeId);
}

// refreshRoom перечитывает комнату и применяет её целиком.
//
// Это единственное место, где комната доходит до страницы. Оно нужно и при
// первом открытии, и когда настройки поменяли в другой вкладке — отличаются
// только вызовы до и после.
//
// Добавлять настройку в механику не приходится: её читает resolveRoom и кладёт
// в view, а apply разносит по странице. Страница узнаёт об изменении одним
// кадром system.rooms.changed и перечитывает комнату целиком, а не по полям —
// поэтому новая настройка не требует ни тут, ни там ничего трогать.
async function refreshRoom() {
    const view = await resolveRoom('roomId');

    state.room = view.room;
    state.roomProblem = view.problem;
    state.shown = view.shown === null ? null : new Set(view.shown);

    // Тема из адреса главнее темы комнаты: ?theme= — прямая просьба человека,
    // и комната её не отменяет.
    const wanted = state.themeFromURL || view.theme || 'system';
    if (wanted !== state.themeId) {
        state.themeId = wanted;
        // Снимок сокета приносит описание темы, которая была при подключении.
        // Тему сменили в настройках, а сокет не переоткрывался, поэтому описание
        // берём сами — иначе новая тема была бы видна только после перезагрузки.
        await applyTheme(await themeDescriptor(wanted));
    }
    apply(view, roomTarget);

    const titleEl = document.getElementById('room-title');
    if (titleEl) titleEl.textContent = view.room?.title || 'Все каналы';
    showProblem(view.problem);
    redraw();
}

// themeDescriptor — описание темы по id: шаблон, стили и скрипты.
async function themeDescriptor(id) {
    try {
        const response = await fetch('/api/themes', {cache: 'no-store'});
        if (!response.ok) return undefined;
        // Ответ — конверт: kind и payload, как у всякого ответа ядра.
        const envelope = await response.json();
        const themes = envelope?.payload?.themes ?? [];
        return themes.find((item) => item.id === id);
    } catch {
        // applyTheme умеет и без описания: system подставляется сама.
        return undefined;
    }
}

setupRoom();
