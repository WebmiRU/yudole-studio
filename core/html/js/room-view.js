// Разбор ?roomId= — какая комната открыта, что показывать в её ленте и на что
// заменить, если комнаты нет.

import {ALL_KINDS} from './events.js';

// Гарнитуры из настроек комнаты. Список один и тот же, что у комнаты в
// настройках: если бы здесь был свой, в ленте тихо поехала бы гарнитура,
// которой человек не выбирал.
const FONTS = new Set([
    'Noto Sans',
    'Open Sans',
    'Cantarell',
    'Carlito',
    'Liberation Serif',
]);

const DEFAULT_FONT = 'Noto Sans';
const DEFAULT_THEME = 'system';
const DEFAULT_MESSAGE_SIZE = 15;
const DEFAULT_AUTHOR_SIZE = 14;

// resolve читает комнату из адреса и говорит, что с ней делать.
//
// Ничего не находит — не беда, а повод сказать об этом в ленте и показать
// ленту как обычно: страница с id несуществующей комнаты не должна быть
// пустой или сломанной.
export async function resolveRoom(param = 'roomId') {
  const wanted = new URLSearchParams(location.search).get(param);

  if (!wanted) {
    return defaults('Комната не указана в адресе — показываю все события.');
  }

  let rooms;
  try {
    const response = await fetch('/api/rooms');
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    rooms = (await response.json()).rooms ?? [];
  } catch (error) {
    // Ядро недоступно — лента всё равно получит события по websocket, и
    // показывать их все разумнее, чем молчать.
    return defaults(`Ядро недоступен: ${error.message}`);
  }

  const room = rooms.find((item) => item.id === String(wanted));
  if (!room) {
    return defaults(`Комната «${wanted}» не найдена — показываю все события.`);
  }

  return fromRoom(room);
}

// defaults — то, чем заменяем ненайденное. События все: пустая лента из-за
// битой ссылки хуже, чем лишние события, и человек увидит, что что-то не так.
function defaults(problem) {
  return {
    room: null,
    problem,
    shown: null,
    font: DEFAULT_FONT,
    theme: DEFAULT_THEME,
    messageSize: DEFAULT_MESSAGE_SIZE,
    authorSize: DEFAULT_AUTHOR_SIZE,
  };
}

// fromRoom — комната найдена: берём её настройки, но каждое поле всё равно
// проверяем. В конфиге может лежать мусор — тему удалили, шрифт переименовали,
// размер забыли.
async function fromRoom(room) {
  const shown = (room.events ?? []).filter((kind) => ALL_KINDS.includes(kind));
  const font = FONTS.has(room.font) ? room.font : DEFAULT_FONT;

  let theme = room.theme;
  let themeMissing = false;
  try {
    const response = await fetch('/api/themes');
    const themes = response.ok ? ((await response.json()).themes ?? []) : [];
    if (!themes.some((item) => item.id === theme)) {
      theme = DEFAULT_THEME;
      themeMissing = true;
    }
  } catch {
    theme = DEFAULT_THEME;
    themeMissing = true;
  }

  // События, о которых комната не знает, но которые в ней включены: молча
  // выбрасывать нельзя, человек должен увидеть, что его выбор не сработал.
  const unknown = (room.events ?? []).filter((kind) => !ALL_KINDS.includes(kind));

  const parts = [];
  if (themeMissing) {
    parts.push(`тема «${room.theme}» не найдена`);
  }
  if (!FONTS.has(room.font)) {
    parts.push(`шрифт «${room.font}» неизвестен`);
  }
  if (unknown.length) {
    parts.push(`события вне списка: ${unknown.join(', ')}`);
  }

  return {
    room,
    problem: parts.length ? `${parts.join('; ')} — заменил на значения по умолчанию.` : '',
    shown,
    font,
    theme,
    messageSize: positive(room.message_size, DEFAULT_MESSAGE_SIZE),
    authorSize: positive(room.author_size, DEFAULT_AUTHOR_SIZE),
  };
}

// positive отбрасывает мусор в размере: ноль в font-size означает невидимый
// текст, а такое значение в конфиге вредно видеть.
function positive(value, fallback) {
  const size = Number(value);
  return Number.isFinite(size) && size > 0 ? size : fallback;
}

// apply ставит настройки комнаты на элемент ленты: гарнитура и размеры уходят
// в CSS-переменные, а сама лента их наследует.
export function apply(view, element) {
  element.style.setProperty('--room-font', view.font);
  element.style.setProperty('--room-message-size', `${view.messageSize}px`);
  element.style.setProperty('--room-author-size', `${view.authorSize}px`);
  element.dataset.theme = view.theme;
  element.dataset.room = view.room?.id ?? '';

  loadTheme(view.theme);
}

// Текущая тема. Держим рядом со стилем: сменить тему — значит убрать старый
// <style> и вставить новый, а искать его по документу нечем.
let themeStyle = null;
let themeID = '';

// loadTheme вставляет оформление комнаты в страницу.
//
// Тема — это кусок разметки со стилем (themes/<тема>/index.html), а не отдельный
// .css: так её пишут вручную, в том же файле, где и разметка. Поэтому файл
// забирается по адресу ядра и содержимое <style> кладётся в голову документа.
//
// Стиль вставляется последним — тогда он перебивает стили приложения. Это
// единственный способ, которым тема может переопределить оформление ленты при
// равной специфичности селекторов.
//
// Картинки темы (текстура, эмблема) в её CSS написаны относительно: url("./x.svg").
// После вставки в чужую страницу эти адреса смотрели бы в корень сайта, поэтому
// переписываются на абсолютные — иначе тема выглядела бы наполовину.
//
// Не загрузилась — тихо. Тему выбирают в настройках, и если её файла нет,
// человек получит пустую ленту вместо сообщений; лучше лента как есть.
async function loadTheme(id) {
  if (id === themeID && themeStyle) {
    return;
  }
  themeID = id;

  themeStyle?.remove();
  themeStyle = null;

  try {
    const response = await fetch(`/themes/${encodeURIComponent(id)}`);
    if (!response.ok) {
      return;
    }

    const markup = await response.text();
    const css = stylesOf(markup);
    if (!css) {
      return;
    }

    const node = document.createElement('style');
    node.dataset.roomTheme = id;
    node.textContent = rebase(css, id);
    document.head.append(node);

    themeStyle = node;
  } catch {
    // Ядро недоступно — лента остаётся в системной теме.
  }
}

// stylesOf достаёт CSS из куска разметки. Разбирать DOM ради этого дорого, а в
// файле темы стиль всегда один и обёрнут в <style rel="stylesheet">.
function stylesOf(markup) {
  const match = markup.match(/<style\b[^>]*>([\s\S]*?)<\/style>/i);
  return match ? match[1].trim() : '';
}

// rebase переписывает относительные адреса картинок на адреса темы.
//
// Только url(...): остальное в CSS от каталога не зависит. Данные и http не
// трогаем — они и так абсолютные и указывают куда надо.
function rebase(css, id) {
  const base = `/themes/${encodeURIComponent(id)}/`;

  return css.replace(/url\(\s*(['"]?)(?!data:|https?:|\/)([^'")]+)\1\s*\)/gi,
    (whole, quote, file) => `url(${quote}${base}${file.replace(/^\.\//, '')}${quote})`);
}
