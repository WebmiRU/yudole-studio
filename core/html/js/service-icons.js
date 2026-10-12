// Значки сервисов.
//
// Рисуем сами, а не берём чужие логотипы: логотип — чужой товарный знак, и
// встраивать его на чужую страницу без разрешения нельзя. Это узнаваемые по
// цвету и форме значки, а не копии.
//
// Значок ставится слева от ника в ленте: в одной ленте сидят сразу несколько
// сервисов, и без него не видно, кто именно написал.

const SIZES = {
    // Каждому значку свой цвет и своя форма — чтобы различались и на глаз, и
    // для того, кто цвета не различает.
    twitch: `
        <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" focusable="false">
            <rect x="1" y="2" width="22" height="20" rx="5" fill="#9146FF"/>
            <path d="M6 6h13v9l-3.6 3.6h-3L10 16H7.4V13H6V6zm2.2 2.2v5.2h2v1.6l1.5 1.5h1.6l1.7-1.7V8.2H8.2z"
                  fill="#fff"/>
        </svg>`,
    youtube: `
        <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" focusable="false">
            <rect x="1" y="4" width="22" height="16" rx="5" fill="#FF0000"/>
            <path d="M10 8.6v6.8l5.6-3.4L10 8.6z" fill="#fff"/>
        </svg>`,
    vkvideo: `
        <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" focusable="false">
            <rect x="1" y="2" width="22" height="20" rx="6" fill="#0077FF"/>
            <path d="M6.6 7.2h2.9c1.5 0 2.3.8 2.3 2.1 0 .9-.4 1.5-1.2 1.8.9.2 1.5.9 1.5 1.9 0 1.4-.9 2.2-2.5 2.2H6.6V7.2zm2.6 3.2c.6 0 .9-.3.9-.8s-.3-.7-.9-.7H8.7v1.5h.5zm.1 3.2c.7 0 1-.3 1-.9 0-.5-.4-.8-1-.8H8.7v1.7h.6z"
                  fill="#fff"/>
            <circle cx="17.6" cy="12" r="2.1" fill="#fff"/>
        </svg>`,
    goodgame: `
        <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" focusable="false">
            <rect x="1" y="1" width="22" height="22" rx="6" fill="#F5A623"/>
            <rect x="5" y="9" width="4" height="6" rx="1" fill="#fff"/>
            <rect x="9.5" y="6" width="4" height="9" rx="1" fill="#fff"/>
            <rect x="14" y="10" width="5" height="5" rx="1" fill="#fff"/>
        </svg>`,
};

// Значок сервиса по его id. Неизвестный сервис остаётся без значка: лучше пусто,
// чем выдуманная картинка, которую человек прочитает как «отсюда писали».
export function serviceIcon(pluginId) {
    return SIZES[pluginId] ?? null;
}

// Есть ли у сервиса свой значок: нужно, чтобы не плодить пустые элементы.
export function hasServiceIcon(pluginId) {
    return Boolean(SIZES[pluginId]);
}