// Главная: меню разделов и плитки комнат.
//
// Комнаты берутся с ядра теми же /api/rooms и показываются теми же плитками,
// что в настройках. Домашняя страница — это вход в ленту, и заходить за каждой
// комнатой в настройки ради запуска неудобно.

import '../sass/home.sass';
import {el} from './dom.js';
import {chatURL, openChat, roomCard} from './room-cards.js';

const roomsEl = document.getElementById('home-rooms');
const countEl = document.getElementById('home-rooms-count');

load();

async function load() {
    const [rooms, themes] = await Promise.all([fetchJSON('/api/rooms'), fetchJSON('/api/themes')]);

    // Ядро не ответило — молча пустой список читался бы как «комнат нет», и
    // человек пошёл бы искать, где он их потерял.
    if (!rooms) {
        countEl.textContent = '—';
        roomsEl.replaceChildren(el('p', {class: 'cards__empty'}, ['Ядро недоступно']));
        return;
    }

    const list = rooms.rooms ?? [];
    countEl.textContent = String(list.length);

    if (!list.length) {
        roomsEl.replaceChildren(el('p', {class: 'cards__empty'}, [
            'Комнат пока нет. ',
            el('a', {href: '/settings/rooms'}, ['Добавить']),
        ]));
        return;
    }

    roomsEl.replaceChildren(...list.map((room) => {
        const card = roomCard(room, themes?.themes ?? []);
        card.addEventListener('click', () => openChat(chatURL(room.id)));
        return card;
    }));
}

async function fetchJSON(url) {
    try {
        const response = await fetch(url);
        return response.ok ? await response.json() : null;
    } catch {
        return null;
    }
}