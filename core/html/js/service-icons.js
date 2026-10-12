// Значки сервисов.
//
// Один набор на обе страницы: список настроек и живой чат. Раньше рисунки были
// разведены по файлам, и один и тот же YouTube в двух местах страницы выглядел
// по-разному — крупным треугольником в настройках и мелкой плашкой в ленте.
//
// Рисуем сами, а не берём чужие логотипы: логотип — чужой товарный знак, и
// встраивать его на чужую страницу без разрешения нельзя. Это узнаваемые по
// цвету и форме знаки, а не копии.

const NS = 'http://www.w3.org/2000/svg';

function node(tag, attrs = {}, children = []) {
    const svg = document.createElementNS(NS, tag);
    for (const [name, value] of Object.entries(attrs)) {
        if (value === undefined || value === null) {
            continue;
        }
        svg.setAttribute(name, String(value));
    }
    svg.append(...children);
    return svg;
}

// Каждому сервису — свой цвет и своя форма, чтобы значки различались и на глаз,
// и для того, кто цвета не различает.
const LOGOS = {
    youtube: (size) => [
        node('rect', {x: 2.5, y: 6.5, width: 27, height: 19, rx: 5.5, fill: '#FF0033'}),
        node('path', {d: 'M13 11.2 21.2 16 13 20.8Z', fill: '#FFFFFF'}),
    ],
    twitch: () => [
        node('path', {d: 'M5 3H29V20L22 27H17L12 31V27H5Z', fill: '#9146FF'}),
        node('path', {d: 'M9 7H25V19L20 24H16L12 27V24H9Z', fill: '#FFFFFF'}),
        node('rect', {x: 13, y: 10, width: 2.5, height: 6, fill: '#9146FF'}),
        node('rect', {x: 19, y: 10, width: 2.5, height: 6, fill: '#9146FF'}),
    ],
    goodgame: () => [
        node('rect', {x: 1.5, y: 1.5, width: 29, height: 29, rx: 8, fill: '#3478D4'}),
        node('text', {
            x: 16, y: 22.5, 'text-anchor': 'middle',
            'font-family': 'Arial, Helvetica, sans-serif',
            'font-size': 17, 'font-weight': 800, 'letter-spacing': -2.2,
            fill: '#FFFFFF',
        }, ['GG']),
    ],
    vkvideo: () => [
        node('rect', {x: 1, y: 1, width: 30, height: 30, rx: 8, fill: '#0077FF'}),
        node('rect', {x: 7, y: 1, width: 24, height: 30, rx: 9, fill: '#FF2448'}),
        node('text', {
            x: 19, y: 19.2, 'text-anchor': 'middle',
            'font-family': 'Arial, Helvetica, sans-serif',
            'font-size': 7.8, 'font-weight': 800, 'letter-spacing': 0.15,
            fill: '#FFFFFF',
        }, ['LIVE']),
    ],
};

// serviceKey угадывает сервис по названию или id.
//
// Сверяемся и по тому, и по другому: в настройках у плагина есть заголовок
// («VK Видео Live»), а в ленте приходит только id («vkvideo»), и без обоих
// признаков один и тот же плагин угадывался бы на одной странице и не
// угадывался на другой.
export function serviceKey(plugin) {
    const parts = typeof plugin === 'string'
        ? [plugin]
        : [plugin?.id, plugin?.title];
    const identity = parts
        .filter(Boolean)
        .join(' ')
        .toLocaleLowerCase('ru')
        .replace(/[._-]+/g, ' ');

    if (identity.includes('youtube')) return 'youtube';
    if (identity.includes('twitch')) return 'twitch';
    if (/good\s*game/.test(identity)) return 'goodgame';
    if (/(vk\s*video|вк\s*видео)/.test(identity)) return 'vkvideo';
    return null;
}

// serviceLogo is the service's mark at the given size. Возвращает null, если
// сервис не знаком: подставлять выдуманную картинку нельзя — человек прочтёт её
// как «отсюда писали».
export function serviceLogo(plugin, size = 28) {
    const key = serviceKey(plugin);
    if (!key) {
        return null;
    }

    return node('svg', {
        class: `service-logo service-logo--${key}`,
        viewBox: '0 0 32 32',
        width: size,
        height: size,
        'aria-hidden': 'true',
        focusable: 'false',
    }, LOGOS[key](size));
}