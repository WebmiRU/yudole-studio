// Плитка комнаты — общая для главной и настроек.
//
// На главной плитка открывает ленту, в настройках — выбирает комнату. Поэтому
// обработчик нажатия вешает вызывающий: сама плитка знает только, как выглядит.

import {el} from './dom.js';

// Заголовок пустым сохранить можно: человек снял текст и хочет без него. Но в
// плитке пустое место выглядит как дырка, поэтому там подставляется это.
export const UNTITLED = 'Без названия';

export function roomCard(room, themes, {selected = false} = {}) {
    return el('button', {
        type: 'button',
        class: `card${selected ? ' is-selected' : ''}`,
        'aria-pressed': String(selected),
        'data-room': room.id,
    }, [
        el('span', {class: 'card__title'}, [room.title || UNTITLED]),
        el('span', {class: 'card__note'}, [roomNote(room, themes)]),
        el('span', {class: 'card__link'}, [chatURL(room.id)]),
    ]);
}

// roomNote — коротко о том, как комната выглядит: сколько событий она
// показывает и какая тема. Без этого плитки с одинаковыми названиями не
// отличить друг от друга.
export function roomNote(room, themes) {
    const events = room.events?.length ?? 0;
    const names = new Map((themes ?? []).map((theme) => [theme.id, theme.title]));
    const theme = names.get(room.theme) ?? room.theme ?? 'системная';
    const shown = events === 0 ? 'без событий' : `событий: ${events}`;
    return `${shown} · ${theme}`;
}

// chatURL собирает полный адрес ленты. Полный, а не путь: ссылку копируют и
// отправляют кому-то, а «/chat?roomId=2» без хоста ни о чём не говорит. Хост
// берётся свой же, так что на :1080 получится localhost:1080, на :1081 —
// localhost:1081, и скопированная ссылка ведёт туда же, откуда скопировали.
export function chatURL(id) {
    return new URL(`/chat?roomId=${encodeURIComponent(id)}`, location.origin).href;
}

// openChat открывает ленту в отдельном узком окне.
//
// popup=yes обязателен: без него браузер откроет вкладку и размеры проигнорирует.
// 400x800 — лента читается в такое окно как в панель стрима.
export function openChat(url) {
    window.open(url, 'studio-chat',
        'popup=yes,width=400,height=800,left=80,top=60,noopener,noreferrer');
}