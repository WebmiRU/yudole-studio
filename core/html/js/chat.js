// Страница живого чата: всё, что пишут плагины, из всех каналов и всех
// чатов, в одной ленте.
//
// Данные приходят по websocket с ядра: страница ничего не опрашивает и не
// знает, какие плагины вообще есть — что пришло, то и показали.

import '../sass/chat.sass';
import {serviceLogo} from './service-icons.js';

const API_CHAT = '/api/chat';

const state = {
    socket: null,
    // entries — то, что уже на экране, по порядку прихода.
    entries: [],
    // channels — какие каналы мы видели, чтобы в списке был выбор.
    channels: new Map(),
    // names — как называть канал. Приходит из событий плагина: настройки его
    // мы не видим, и он сам кладёт имя в каждое своё событие.
    names: new Map(),
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
        state.names.clear();
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

    // Плагин назвал канал в своём событии — запоминаем, чтобы следующие
    // сообщения о нём тоже называть по-человечески.
    //
    // Только из событий входа и выхода: там name — это имя канала из
    // настроек. А у chat.channel.info name значит другое: там название
    // трансляции, и оно названием канала не является.
    if (entry.channel && entry.payload?.name && namesChannel(entry.kind)) {
        state.names.set(entry.channel, entry.payload.name);
    }

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
    // Неизвестные события не дают блока, поэтому null убираем.
    const trimmed = shown.slice(-100).map(line).filter(Boolean);
    feedEl.replaceChildren(...trimmed);

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
        const message = entry.payload?.message ?? {};
        const head = authorHead(entry);
        // Html приходит только если плагин нашёл в строке смайлы и подставил
        // картинки; иначе это тот же самый текст без единой разметки, и
        // разбирать его как html незачем.
        // Подписка или рейд — это писал сервис, а не человек. Отмечаем: в ленте
        // оно стоит среди реплик, как у самой Twitch, но спутать нельзя.
        const notice = Boolean(entry.payload?.notice);
        if (message.html) {
            return richBlock(entry, 'neutral', head, message.html, notice);
        }
        return block(entry, 'neutral', head, message.text ?? '', notice);
    }

    return block(entry, severityOf(entry), author(entry), describe(entry));
}

// authorHead is the name of the person who wrote, with the service's mark to
// the left of it. В одной ленте сидят сразу несколько сервисов, и без значка не
// видно, кто именно написал: одно и то же имя может быть и в Twitch, и в
// YouTube, а разные ники читаются одинаково.
function authorHead(entry) {
    const name = entry.user ?? '—';
    const logo = serviceLogo(entry.plugin, 16);
    if (!logo) {
        return el('span', {class: 'msg__author'}, [name]);
    }

    const icon = el('span', {class: 'msg__service', title: serviceTitle(entry)});
    icon.append(logo);

    return el('span', {class: 'msg__author'}, [icon, name]);
}

// serviceTitle is the name of the service for the tooltip: значок без подписи —
// просто пятнышко, а с подписью человек понимает, что это за площадка.
function serviceTitle(entry) {
    const plugin = state.plugins.get(entry.plugin);
    return plugin?.title || entry.plugin || '';
}

// richBlock is a block whose text the plugin already turned into markup —
// смайлы вместо их кодов. Разметку ставим целиком: плагин её собрал сам и
// экранировал всё, что не разметка.
function richBlock(entry, severity, head, html, notice = false) {
    const text = el('div', {class: 'msg__text'});
    text.innerHTML = html;
    return messagePanel(notice, head, text);
}

// block is the shape of every message: a HUD frame, a separate header,
// message content and a small technical footer.
function block(entry, severity, head, text, notice = false) {
    const body = [...[text].flat(Infinity)].filter(
        (piece) => piece !== null && piece !== undefined && piece !== '',
    );
    if (body.length === 0) {
        return null;
    }

    const content = el('div', {class: 'msg__text'}, body.map((piece) => (
        typeof piece === 'string' ? document.createTextNode(piece) : piece
    )));
    return messagePanel(notice, head, content);
}

// messagePanel keeps the message's content flow natural while the frame,
// header ticks and footer hardware remain independent decorative layers.
function messagePanel(notice, head, content) {
    const frame = el('div', {class: 'msg__frame', 'aria-hidden': 'true'}, [
        el('span', {class: 'msg__frame-rail msg__frame-rail--top'}),
        el('span', {class: 'msg__frame-rail msg__frame-rail--bottom'}),
        el('span', {class: 'msg__frame-joint msg__frame-joint--right'}),
        el('span', {class: 'msg__frame-joint msg__frame-joint--left'}),
        el('span', {class: 'msg__frame-light'}),
    ]);

    const header = el('header', {class: 'msg__head'}, [...[head].flat().filter(Boolean)]);
    const rule = el('div', {class: 'msg__rule', 'aria-hidden': 'true'}, [
        el('span', {class: 'msg__rule-signal'}),
    ]);
    const footer = el('footer', {class: 'msg__foot', 'aria-hidden': 'true'}, [
        el('span', {class: 'msg__foot-mark'}),
        el('span', {class: 'msg__foot-line'}),
        el('span', {class: 'msg__foot-tick'}),
    ]);

    return el('article', noticeAttrs(notice), [
        frame,
        el('div', {class: 'msg__panel'}, [
            header,
            rule,
            el('div', {class: 'msg__content'}, [content]),
            footer,
        ]),
    ]);
}

// noticeAttrs — как выглядит служебное сообщение. Отметка нужна и на блоке,
// и в атрибутах: по одному цвету полоски не отличить, а тег показывает, что
// реплику писал сервис, а не человек.
function noticeAttrs(notice) {
    if (notice) {
        return {class: 'msg is-notice', 'data-severity': 'info'};
    }
    return {class: 'msg', 'data-severity': 'neutral'};
}

// author is who the service message is from: the plugin that sent it, in
// brackets, so it never gets confused with a person in the chat.
function author(entry) {
    const plugin = state.plugins.get(entry.plugin);
    const name = plugin?.title || entry.plugin || 'чат';

    // Значок тут — тем же, что и у реплик. В ленте вперемешку идут сообщения
    // Twitch, YouTube и VK Видео, и служебная строка «YouTube: в сети» должна
    // выглядеть так же, как и реплика оттуда же.
    const logo = serviceLogo(entry.plugin, 16);
    if (!logo) {
        return el('span', {class: 'msg__author msg__author--plugin'}, [`[${name}]`]);
    }
    const icon = el('span', {class: 'msg__service', title: name});
    icon.append(logo);
    return el('span', {class: 'msg__author msg__author--plugin'}, [icon, `[${name}]`]);
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
        case 'chat.channel.join.success':
            return 'success';
        case 'chat.channel.join.failed':
            return 'error';
        case 'chat.channel.unjoin.success':
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

// describe is the text of a service message. It returns pieces, not one string:
// название канала внутри фразы выделяется жирным, а для этого нужен узел.
function describe(entry) {
    const name = channelName(entry.channel);
    const reason = entry.payload?.reason;
    const service = entry.payload?.service ?? 'чат';

    switch (entry.kind) {
        case 'chat.connection.state':
            return [connectionPhrase(entry.payload?.state, service, reason)];
        case 'chat.channel.join.success':
            return ['Канал ', strong(name), ' подключён'];
        case 'chat.channel.join.failed':
            return ['Не удалось подключить канал ', strong(name), `: ${reason ?? '?'}`];
        case 'chat.channel.unjoin.success':
            return ['Канал ', strong(name), ' отключён'];
        case 'chat.channel.unjoin.failed':
            return ['Не удалось отключить канал ', strong(name), `: ${reason ?? '?'}`];
        case 'chat.channel.info':
            // Счётчики и название трансляции. Пока их не шлём, но когда
            // начнём — покажем, а не выкинем.
            return [strong(name), channelInfo(entry.payload)];
        case 'chat.message.deleted':
            return [deletedPhrase(entry.payload)];
        default:
            // Неизвестный вид события не показываем протоколом: в ленте
            // «chat.channel.join.success» — это мусор, а не сообщение.
            console.warn('chat: неизвестное событие', entry.kind, entry);
            return null;
    }
}

// connectionPhrase says how the connection to the service is. Каждое состояние
// читается как отдельная фраза, а не как слово в столбце.
function connectionPhrase(state, service, reason) {
    const withReason = (text) => (reason ? `${text}: ${reason}` : text);

    switch (state) {
        case 'online':
            return `Подключились к ${service}`;
        case 'connecting':
            return `Подключаемся к ${service}`;
        case 'reconnecting':
            return withReason(`Переподключаемся к ${service}`);
        case 'offline':
            return `Нет связи с ${service}`;
        default:
            return `${service}: ${stateWord(state)}`;
    }
}

// channelInfo adds what the plugin told about the channel. Приходит не всегда:
// если название трансляции совпало с названием канала, повторять его смысла
// нет, и плагин молчит.
function channelInfo(payload) {
    const parts = [];

    if (payload?.name) {
        parts.push([' — ', strong(payload.name)]);
    }
    if (payload?.clients) {
        parts.push([`, ${payload.clients} в канале`]);
    }
    if (payload?.premiumOnly) {
        parts.push([`, только для подписчиков`]);
    }
    if (payload?.smileOnly) {
        parts.push([`, только смайлики`]);
    }

    return parts.flat();
}

function deletedPhrase(payload) {
    return payload?.by ? `Реплику удалил ${payload.by}` : 'Реплику удалили';
}

// strong is what makes a channel name stand out inside a sentence.
function strong(text) {
    return el('b', {class: 'msg__channel'}, [text]);
}

function stateWord(value) {
    return {
        connecting: 'подключаемся',
        online: 'на связи',
        reconnecting: 'переподключаемся',
        offline: 'нет связи',
    }[value] ?? value ?? '';
}

// namesChannel reports whether the event's name is the channel's own name.
function namesChannel(kind) {
    return kind === 'chat.channel.join.success'
        || kind === 'chat.channel.join.failed'
        || kind === 'chat.channel.unjoin.success'
        || kind === 'chat.channel.unjoin.failed';
}

// channelName is what we call a channel in the text. First the name the user
// gave it in the settings, then the title the service knows it by, and only
// в конце — номер: назвать канал его номером значит сказать человеку «как
// зовут».
function channelName(id) {
    if (!id) {
        return '';
    }
    const fromSettings = state.names.get(id);
    if (fromSettings) {
        return fromSettings;
    }
    const known = state.channels.get(id);
    return known?.name || id;
}

// channelTitle is the long form, for places with room to explain.
function channelTitle(id) {
    const known = state.channels.get(id);
    const name = channelName(id);
    if (!known || !known.name || known.name === name) {
        return name;
    }
    return `${name} — «${known.name}»`;
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