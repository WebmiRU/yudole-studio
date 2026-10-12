// Страница живого чата: всё, что пишут плагины, из всех каналов и всех
// чатов, в одной ленте.
//
// Данные приходят по websocket с ядра: страница ничего не опрашивает и не
// знает, какие плагины вообще есть — что пришло, то и показали.

import '../sass/chat.sass';

const API_CHAT = '/api/chat';

const state = {
    socket: null,
    // entries — то, что уже на экране, по порядку прихода.
    entries: [],
    // channels — какие каналы мы видели, чтобы в списке был выбор.
    channels: new Map(),
    // plugins — кто пишет: имя плагина и его заголовок.
    plugins: new Map(),
    // filter — канал, который выбран в списке; пустая строка означает «все».
    filter: '',
    // onlyLines — прятать события о каналах и соединении, оставив реплики.
    onlyLines: false,
    // stuck — прижата ли лента к низу.
    stuck: true,
    seen: 0,
};

const feedEl = document.getElementById('feed');
const stateEl = document.getElementById('state');
const stateTextEl = document.getElementById('state-text');
const summaryEl = document.getElementById('summary');
const channelEl = document.getElementById('channel-filter');
const onlyChatsEl = document.getElementById('only-chats');

const LINE = 'chat.message.public';
const PRIVATE = 'chat.message.private';

// --- Подключение -----------------------------------------------------------

function connect() {
    const url = new URL(API_CHAT, location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';

    const socket = new WebSocket(url);
    state.socket = socket;

    socket.addEventListener('open', () => setConnection('online', 'на связи'));
    socket.addEventListener('message', (event) => {
        let message;
        try {
            message = JSON.parse(event.data);
        } catch {
            return;
        }
        handle(message);
    });
    socket.addEventListener('close', () => {
        setConnection('offline', 'нет связи');
        // Ядро может перезапускаться: пробуем сами, но не часто.
        setTimeout(connect, 2000);
    });
    socket.addEventListener('error', () => socket.close());
}

function handle(message) {
    if (message.hello) {
        state.plugins.clear();
        for (const plugin of message.hello.plugins ?? []) {
            state.plugins.set(plugin.plugin, plugin);
        }
        state.entries = [];
        state.channels.clear();
        for (const entry of message.hello.history ?? []) {
            take(entry);
        }
        redrawChannelFilter();
        redraw();
        return;
    }
    if (message.event) {
        take(message.event);
        redraw();
    }
}

// take records one entry and remembers its channel.
function take(entry) {
    state.entries.push(entry);
    state.seen++;

    if (entry.channel) {
        const known = state.channels.get(entry.channel) ?? {name: '', plugin: entry.plugin, count: 0};
        known.count++;
        if (entry.kind === 'chat.channel.info' && entry.payload?.name) {
            known.name = entry.payload.name;
        }
        known.plugin = entry.plugin;
        state.channels.set(entry.channel, known);
        redrawChannelFilterSoon();
    }

    if (isLine(entry)) {
        markSeen();
    }
}

// isLine reports whether the entry is a chat line.
function isLine(entry) {
    return entry.kind === LINE || entry.kind === PRIVATE;
}

// --- Отрисовка --------------------------------------------------------------

function redraw() {
    const shown = state.entries.filter(passes);

    // Держим не больше сотни строк: иначе страница растёт бесконечно.
    const trimmed = shown.slice(-100);
    feedEl.replaceChildren(...trimmed.map(line));

    if (state.stuck) {
        feedEl.scrollTop = feedEl.scrollHeight;
    }
    updateSummary(trimmed);
}

// passes is the filter: the chosen channel, and whether service messages are
// shown at all.
function passes(entry) {
    if (state.onlyLines && !isLine(entry)) {
        return false;
    }
    if (state.filter && entry.channel !== state.filter) {
        return false;
    }
    return true;
}

// Виды сообщений: рамка слева. Всё, что не реплика, — служебное, и у служебного
// есть важность: об одном человек стоит знать, другое он и сам увидит.
const SEVERITIES = ['neutral', 'info', 'success', 'warning', 'error'];

// line is one block: a chat line, or a note about a channel.
function line(entry) {
    if (isLine(entry)) {
        const text = entry.text ?? entry.payload?.message?.text ?? '';
        return block(entry, 'neutral', el('span', {class: 'msg__author'}, [entry.user ?? '—']), text);
    }

    return block(entry, severityOf(entry), author(entry), describe(entry));
}

// block is the shape of every message: who wrote it, a rule under the name, and
// then the text itself.
function block(entry, severity, head, text) {
    return el('article', {class: 'msg', 'data-severity': severity}, [
        // Полоска стоит между автором и текстом, а не рядом с автором: шапка
        // выстроена в строку, и внутри неё полоска легла бы вбок.
        el('div', {class: 'msg__head'}, [...[head].flat().filter(Boolean)]),
        el('div', {class: 'msg__rule'}),
        el('div', {class: 'msg__text'}, [text]),
    ]);
}

// author is who the service message is from: the plugin that sent it, in
// brackets, so it never gets confused with a person in the chat.
function author(entry) {
    const plugin = state.plugins.get(entry.plugin);
    const name = plugin?.title || entry.plugin || 'чат';
    return el('span', {class: 'msg__author msg__author--plugin'}, [`[${name}]`]);
}

// severityOf says how much a service event matters.
//
// Разбивка не «на глаз»: важность выводится из того, что произошло, чтобы одна
// и та же строка всегда выглядела одинаково — и на странице, и в тестах.
function severityOf(entry) {
    switch (entry.kind) {
        case 'chat.connection.state': {
            const state = entry.payload?.state;
            if (state === 'online') {
                return 'success';
            }
            // Пока переподключаемся — это ещё не беда; когда связи нет совсем,
            // человек должен это заметить.
            return state === 'offline' ? 'error' : 'info';
        }
        case 'chat.channel.join.successful':
            return 'success';
        case 'chat.channel.join.failed':
            return 'error';
        case 'chat.channel.unjoin.successful':
            // Мы вышли сами, по настройкам: это было задумано, а не поломка.
            return 'info';
        case 'chat.channel.unjoin.failed':
            return 'warning';
        case 'chat.message.deleted':
            // Кто-то удалил чужую реплику: чат живёт, но человек это заметит.
            return 'warning';
        default:
            return 'info';
    }
}

function describe(entry) {
    switch (entry.kind) {
        case 'chat.connection.state':
            return `${entry.payload?.service ?? 'чат'}: ${stateWord(entry.payload?.state)}`;
        case 'chat.channel.join.successful':
            return `вошли в канал ${channelTitle(entry.channel)}`;
        case 'chat.channel.join.failed':
            return `не вошли в канал ${channelTitle(entry.channel)}: ${entry.payload?.reason ?? '?'}`;
        case 'chat.channel.unjoin.successful':
            return `вышли из канала ${channelTitle(entry.channel)}`;
        case 'chat.channel.unjoin.failed':
            return `не вышли из канала ${channelTitle(entry.channel)}: ${entry.payload?.reason ?? '?'}`;
        case 'chat.channel.info':
            return `${channelTitle(entry.channel)}${entry.payload?.clients ? ` — ${entry.payload.clients} в канале` : ''}`;
        case 'chat.message.deleted':
            return `реплику ${entry.payload?.messageId ?? '?'} удалили`;
        default:
            return entry.kind;
    }
}

function stateWord(value) {
    return {
        connecting: 'подключаемся',
        online: 'на связи',
        reconnecting: 'переподключаемся',
        offline: 'нет связи',
    }[value] ?? value ?? '';
}

function channelTitle(id) {
    const known = state.channels.get(id);
    if (!known) {
        return id;
    }
    return known.name ? `${id} — ${known.name}` : id;
}

function updateSummary(shown) {
    const lines = shown.filter(isLine).length;
    const total = state.entries.filter(isLine).length;
    const hidden = total - lines;

    const parts = [`реплик: ${total}`];
    if (hidden > 0) {
        parts.push(`показано: ${lines}`);
    }
    if (state.channels.size) {
        parts.push(`каналов: ${state.channels.size}`);
    }
    summaryEl.textContent = parts.join(' · ');
}

// --- Каналы -----------------------------------------------------------------

// redrawChannelFilter is called on every event, but the list itself changes
// rarely: rebuilding a select while the person picks from it would be rude.
let filterTimer = 0;

function redrawChannelFilterSoon() {
    clearTimeout(filterTimer);
    filterTimer = setTimeout(redrawChannelFilter, 500);
}

function redrawChannelFilter() {
    const chosen = state.filter;
    const options = [el('option', {value: ''}, ['все каналы'])];

    for (const [id, known] of [...state.channels].sort()) {
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

// --- Прокрутка --------------------------------------------------------------

// follow the tail, but only while the person is already at the bottom: иначе
// новая строка выбивала бы то, что он читает.
feedEl.addEventListener('scroll', () => {
    const gap = feedEl.scrollHeight - feedEl.scrollTop - feedEl.clientHeight;
    state.stuck = gap < 40;
});

function markSeen() {
    document.title = `studio — чат`;
}

// --- Прочее -----------------------------------------------------------------

function setConnection(name, text) {
    stateEl.dataset.state = name;
    stateTextEl.textContent = text;
}

function el(tag, attrs = {}, children = []) {
    const node = document.createElement(tag);
    for (const [name, value] of Object.entries(attrs)) {
        if (name === 'class') {
            node.className = value;
        } else if (value !== undefined && value !== null) {
            node.setAttribute(name, value);
        }
    }
    for (const child of children) {
        node.append(child);
    }
    return node;
}

connect();