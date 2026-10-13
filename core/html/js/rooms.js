// Раздел «Комнаты»: список комнат и настройки выбранной.
//
// Комнаты в будущем придут с ядра, и настройки их будут храниться там же.
// Пока сущности нет, список и значения заданы здесь: держать захардкоженное в
// коде — значит забыть выкинуть. Места, где это заменить, помечены.

import '../sass/rooms.sass';

// Шрифты. Взяты те, что есть в любом браузере без загрузки файлов: системный,
// моноширинный, с засечками и без. Подключать веб-шрифты будем позже, когда
// появится место, откуда их грузить.
const FONTS = [
    {value: 'system', label: 'Системный', stack: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif'},
    {value: 'sans', label: 'Без засечек', stack: 'Arial, Helvetica, "Liberation Sans", sans-serif'},
    {value: 'serif', label: 'С засечками', stack: 'Georgia, "Times New Roman", serif'},
    {value: 'mono', label: 'Моноширинный', stack: 'ui-monospace, "JetBrains Mono", Menlo, Consolas, monospace'},
];

// Размеры в пикселях: список, а не число, потому что человек выбирает глазами.
// Ниже 12 читается плохо, выше 22 лента разъезжается.
const SIZES = [12, 13, 14, 15, 16, 18, 20, 22, 24];

// Помечено: до появления комнат в ядре список и настройки живут здесь.
const ROOMS = [
    {id: '1', name: 'Комната 1', title: 'Комната 1', font: 'system', theme: 'system',
        messageSize: 15, authorSize: 14},
    {id: '2', name: 'Комната 2', title: 'Комната 2', font: 'system', theme: 'system',
        messageSize: 15, authorSize: 14},
    {id: '3', name: 'Комната 3', title: 'Комната 3', font: 'system', theme: 'starcraft',
        messageSize: 16, authorSize: 14},
];

const state = {
    rooms: ROOMS,
    selectedId: null,
    // Темы приходят с ядра: /api/themes отдаёт каталог, снятый при старте.
    themes: [],
    themesLoaded: false,
};

const roomsEl = document.getElementById('rooms');
const optionsEl = document.getElementById('room-options');
const countEl = document.getElementById('rooms-count');

loadThemes();
render();

// --- Данные -------------------------------------------------------------------

// loadThemes спрашивает каталог тем один раз. Сканируется он на стороне ядра
// тоже один раз, при старте, — здесь перечитывать нечего.
async function loadThemes() {
    try {
        const response = await fetch('/api/themes');
        if (!response.ok) {
            throw new Error(`HTTP ${response.status}`);
        }
        const data = await response.json();
        state.themes = Array.isArray(data.themes) ? data.themes : [];
    } catch (error) {
        // Тем нет — не беда: селект останется с одной системной, и комната
        // продолжит работать. Ошибку показываем, чтобы было видно, что при
        //чина не в комнате.
        console.warn('rooms: cannot load the theme catalog', error);
        state.themes = [{id: 'system', title: 'Системная'}];
    } finally {
        state.themesLoaded = true;
        render();
    }
}

// --- Рендер -------------------------------------------------------------------

function render() {
    countEl.textContent = String(state.rooms.length);
    renderRooms();
    renderOptions();
}

function renderRooms() {
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

// renderOptions показывает настройки выбранной комнаты. Пока эта форма никуда не
// отправляется: значения меняются в памяти и переживают переключение между
// комнатами, но переживать перезагрузку страницы не умеют.
function renderOptions() {
    const room = selected();

    if (!room) {
        optionsEl.replaceChildren(el('p', {class: 'panel__empty'},
            ['Выберите комнату слева, чтобы задать её настройки.']));
        return;
    }

    optionsEl.replaceChildren(
        el('div', {class: 'options'}, [
            el('div', {class: 'options__head'}, [
                el('div', {}, [
                    el('h2', {class: 'options__title'}, [room.name]),
                    el('p', {class: 'options__desc'}, ['Как эта комната выглядит в ленте сообщений.']),
                ]),
            ]),
            el('form', {class: 'options__body'}, [
                field('Заголовок', 'Как комната называется в ленте', textInput(room, 'title')),
                field('Шрифт', 'Гарнитура реплик в этой комнате', selectInput(room, 'font',
                    FONTS.map((f) => ({value: f.value, label: f.label})))),
                field('Тема', 'Оформление комнаты', themeSelect(room)),
                field('Размер шрифта сообщений', 'Высота реплики в ленте', selectInput(room, 'messageSize',
                    SIZES.map((s) => ({value: String(s), label: `${s} px`})))),
                field('Размер шрифта никнеймов', 'Высота имени автора', selectInput(room, 'authorSize',
                    SIZES.map((s) => ({value: String(s), label: `${s} px`})))),
            ]),
        ]),
    );
}

// field — строка формы: подпись слева, управление справа. Разметка и отступы те
// же, что у полей плагина: форма одна на всю студию.
function field(label, hint, control) {
    return el('div', {class: 'field'}, [
        el('label', {class: 'field__label'}, [
            label,
            el('div', {class: 'field__hint'}, [hint]),
        ]),
        el('div', {class: 'field__control'}, [control]),
    ]);
}

function textInput(room, key) {
    const input = el('input', {class: 'input', type: 'text', value: room[key] ?? ''});
    input.addEventListener('input', () => {
        room[key] = input.value;
    });
    return input;
}

function selectInput(room, key, choices) {
    const select = el('select', {class: 'select'},
        choices.map((choice) => el('option', {value: choice.value}, [choice.label])));
    select.value = String(room[key] ?? '');
    select.addEventListener('change', () => {
        room[key] = select.value;
    });
    return select;
}

// themeSelect собирает список из тем ядра. Если у комнаты стоит тема, которой
// больше нет, она остаётся в списке: иначе выбор молча сдвинулся бы на первую
// строку и человек не понял бы, что его тему удалили.
function themeSelect(room) {
    const choices = state.themes.map((theme) => ({value: theme.id, label: theme.title}));

    if (room.theme && !choices.some((choice) => choice.value === room.theme)) {
        choices.push({value: room.theme, label: `${room.theme} — темы нет на диске`});
    }
    if (choices.length === 0) {
        choices.push({value: '', label: 'Тем нет'});
    }

    return selectInput(room, 'theme', choices);
}

function select(id) {
    state.selectedId = state.selectedId === id ? null : id;
    render();
}

function selected() {
    return state.rooms.find((room) => room.id === state.selectedId) ?? null;
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
