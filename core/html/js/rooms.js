// Раздел «Комнаты»: список комнат.
//
// Список пока не подключён к ядру — комнат как сущности ещё нет. Поэтому здесь
// только сам список и кнопка добавления без обработчика. Когда появится API,
// замена будет здесь же: чтение списка, создание комнаты, выбор строки.

import '../sass/rooms.sass';

// Комнаты в будущем придут с ядра. Сейчас их три, и они заданы здесь: держать
// в коде то, что завтра придёт по сети, — значит забыть убрать.
const ROOMS = [
    {id: '1', name: 'Комната 1'},
    {id: '2', name: 'Комната 2'},
    {id: '3', name: 'Комната 3'},
];

const state = {
    rooms: ROOMS,
    selectedId: null,
};

const roomsEl = document.getElementById('rooms');
const countEl = document.getElementById('rooms-count');

render();

function render() {
    countEl.textContent = String(state.rooms.length);

    if (state.rooms.length === 0) {
        roomsEl.replaceChildren(el('li', {class: 'rooms__empty'},
            ['Комнат пока нет. Добавьте первую.']));
        return;
    }

    roomsEl.replaceChildren(...state.rooms.map((room) => {
        const selectedNow = room.id === state.selectedId;

        const button = el('button', {
            type: 'button',
            class: 'room-row',
            'aria-pressed': String(selectedNow),
            'data-room': room.id,
        }, [
            el('span', {class: 'room__name'}, [room.name]),
        ]);
        button.addEventListener('click', () => select(room.id));

        return el('li', {}, [button]);
    }));
}

// select отмечает комнату и снимает отметку с прежней. Настройки комнаты справа
// появятся позже; пока выбор только виден.
function select(id) {
    state.selectedId = state.selectedId === id ? null : id;
    render();
}

// el собирает элемент. Тот же помощник, что на странице чат-клиентов: правила
// одни, а на каждой странице он живёт отдельно — модуль на страницу.
function el(tag, attrs = {}, children = []) {
    const node = document.createElement(tag);
    for (const [name, value] of Object.entries(attrs)) {
        if (value === undefined || value === null || value === false) {
            continue;
        }
        node.setAttribute(name, value === true ? '' : value);
    }
    node.append(...children);
    return node;
}
