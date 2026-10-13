// Раздел «Комнаты»: список комнат и настройки выбранной.
//
// Всё хранится на ядре в config.json: комнаты читаются через /api/rooms и
// пишутся обратно. Локальных копий нет намеренно — иначе правка в двух вкладках
// молча перетирала бы другую.

import '../sass/rooms.sass';

// Гарнитуры. Взяты те, что реально стоят в системе и есть в любом браузере без
// загрузки файлов. Первый — тот, которым сейчас рисуется лента: system-ui на
// linux-системах с Noto Sans отдаёт именно Noto Sans, и переключать комнату на
// другую гарнитуру не должно незаметно менять шрифт.
const FONTS = [
    'Noto Sans',
    'Open Sans',
    'Cantarell',
    'Carlito',
    'Liberation Serif',
];

const THEMES_API = '/api/themes';
const ROOMS_API = '/api/rooms';

// Заголовок пустым сохранять можно — человек снял текст и хочет без него. Но в
// списке пустая строка выглядит как дырка, поэтому там подставляется это.
const UNTITLED = 'Без названия';

const state = {
    rooms: [],
    themes: [],
    selectedId: null,
    // Черновик настроек выбранной комнаты. Отдельный от комнаты, потому что до
    // «Сохранить» правки не должны попасть в список.
    draft: null,
    saving: false,
    error: '',
};

const roomsEl = document.getElementById('rooms');
const optionsEl = document.getElementById('room-options');
const countEl = document.getElementById('rooms-count');
const toastsEl = document.getElementById('toasts');

// --- Загрузка -----------------------------------------------------------------

// load спрашивает комнаты и темы. Темы — каталог с диска, снятый ядром при
// старте; комнаты — из config.json.
async function load() {
    const [rooms, themes] = await Promise.all([fetchJSON(ROOMS_API), fetchJSON(THEMES_API)]);

    state.rooms = rooms?.rooms ?? [];
    state.themes = themes?.themes ?? [];
    render();
}

async function fetchJSON(url, options) {
    try {
        const response = await fetch(url, options);
        if (!response.ok) {
            throw new Error(`HTTP ${response.status}`);
        }
        return await response.json();
    } catch (error) {
        state.error = `${url}: ${error.message}`;
        return null;
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
            // is-selected ставится на саму строку, а не на li: оформление
            // выбранной строки живёт в _rooms-list на .room-row.is-selected,
            // и класс на li его не задевал бы. Так же сделано у чат-клиентов.
            class: `room-row${selectedNow ? ' is-selected' : ''}`,
            'aria-pressed': String(selectedNow),
            'data-room': room.id,
        }, [el('span', {class: 'room__name'}, [room.title || UNTITLED])]);
        button.addEventListener('click', () => select(room.id));

        return el('li', {}, [button]);
    }));
}

function renderOptions() {
    const room = selected();

    if (!room) {
        optionsEl.replaceChildren(el('p', {class: 'empty'},
            // Класс .empty — тот же, что у плагинов. .panel__empty задаёт
            // только отступы: текст в нём остался бы цвета страницы, и
            // подпись выглядела бы иначе, чем «Выберите плагин». И без
            // «слева»: в узком окне панель встаёт под списком, и слово
            // указывало бы не туда.
            ['Выберите комнату']));
        return;
    }

    // Подвал лежит внутри формы, а не рядом с ней. Снаружи кнопке
    // «Сохранить» пришлось бы ссылаться на форму через атрибут form, и одна
    // забытая буква в id молча отменяла бы отправку: кнопка нажимается, ничего
    // не происходит. Внутри формы submit достаётся ей сам.
    const form = el('form', {}, [
        el('div', {class: 'options__head'}, [
            el('div', {}, [
                el('h2', {class: 'options__title'}, [room.title || UNTITLED]),
                el('p', {class: 'options__desc'}, ['Как эта комната выглядит в ленте сообщений.']),
            ]),
        ]),
        el('div', {class: 'options__body'}, [
            field('Заголовок', 'Как комната называется в ленте',
                textInput('title')),
            field('Шрифт', 'Гарнитура реплик в этой комнате',
                fontSelect('font')),
            field('Тема', 'Оформление комнаты', themeSelect('theme')),
            field('Размер шрифта сообщений', 'Высота реплики в ленте, px',
                numberInput('message_size')),
            field('Размер шрифта никнеймов', 'Высота имени автора, px',
                numberInput('author_size')),
        ]),
        el('div', {class: 'options__foot'}, [
            el('span', {class: 'options__note'}, ['Настройки сохраняются в config.json']),
            el('span', {class: 'options__actions'}, [
                el('button', {type: 'button', class: 'btn btn--danger', id: 'room-delete'},
                    ['Удалить']),
                el('button', {type: 'submit', class: 'btn btn--primary'},
                    [state.saving ? 'Сохраняю…' : 'Сохранить']),
            ]),
        ]),
    ]);

    form.addEventListener('submit', save);
    form.querySelector('#room-delete')?.addEventListener('click', remove);

    optionsEl.replaceChildren(el('div', {class: 'options'}, [form]));

    if (state.error) {
        optionsEl.append(el('p', {class: 'options__problem'}, [state.error]));
    }
}

// field — строка формы: подпись слева, управление справа. Разметка та же, что у
// полей плагина: в студии одна форма, а не две похожие.
function field(label, hint, control) {
    return el('div', {class: 'field'}, [
        el('label', {class: 'field__label'}, [
            label,
            el('div', {class: 'field__hint'}, [hint]),
        ]),
        el('div', {class: 'field__control'}, [control]),
    ]);
}

// --- Поля ---------------------------------------------------------------------

function textInput(key) {
    const input = el('input', {class: 'input', type: 'text', value: state.draft[key] ?? ''});
    input.addEventListener('input', () => {
        state.draft[key] = input.value;
    });
    return input;
}

// numberInput, а не select: размер шрифта — число, и человек вправе поставить
// любое. Границы всё равно проверяет ядро.
function numberInput(key) {
    const input = el('input', {
        class: 'input input--mono',
        type: 'number',
        min: '8',
        max: '72',
        step: '1',
        value: String(state.draft[key] ?? ''),
    });
    input.addEventListener('input', () => {
        const value = Number.parseInt(input.value, 10);
        // Пустое поле — не ноль: человек ещё печатает. Ноль отправить нельзя,
        // текст с нулевым font-size не виден вовсе.
        state.draft[key] = Number.isFinite(value) && value > 0 ? value : null;
    });
    return input;
}

function fontSelect(key) {
    const select = el('select', {class: 'select'},
        FONTS.map((font) => el('option', {value: font}, [font])));

    // Если в конфиге шрифт, которого в списке нет, он остаётся выбором: иначе
    // значение молча сменилось бы на первое в списке.
    if (state.draft[key] && !FONTS.includes(state.draft[key])) {
        select.prepend(el('option', {value: state.draft[key]},
            [`${state.draft[key]} — нет в списке`]));
    }
    select.value = state.draft[key] ?? FONTS[0];
    select.addEventListener('change', () => {
        state.draft[key] = select.value;
    });
    return select;
}

// themeSelect собирает список из тем ядра. Тема, которой больше нет на диске,
// остаётся в списке с пометкой: иначе выбор тихо уехал бы на первую строку и
// человек не понял бы, что его тему удалили.
function themeSelect(key) {
    const choices = state.themes.map((theme) => ({value: theme.id, label: theme.title}));

    if (state.draft[key] && !choices.some((choice) => choice.value === state.draft[key])) {
        choices.push({value: state.draft[key], label: `${state.draft[key]} — темы нет на диске`});
    }
    if (choices.length === 0) {
        choices.push({value: '', label: 'Тем нет на диске'});
    }

    const select = el('select', {class: 'select'},
        choices.map((choice) => el('option', {value: choice.value}, [choice.label])));
    select.value = state.draft[key] ?? '';
    select.addEventListener('change', () => {
        state.draft[key] = select.value;
    });
    return select;
}

// --- Сохранение и удаление ----------------------------------------------------

async function save(event) {
    event.preventDefault();
    if (state.saving) {
        return;
    }

    const room = selected();
    if (!room) {
        return;
    }

    state.saving = true;
    state.error = '';
    renderOptions();

    // Пустое поле размера — это «человек стёр и не ввёл», а не «ноль».
    // Ноль отправлять нельзя: текст с нулевым font-size не виден.
    const body = {
        title: state.draft.title ?? '',
        font: state.draft.font ?? '',
        theme: state.draft.theme ?? '',
        message_size: state.draft.message_size ?? 0,
        author_size: state.draft.author_size ?? 0,
    };

    const saved = await fetchJSON(`${ROOMS_API}/${encodeURIComponent(room.id)}`, {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(body),
    });

    state.saving = false;

    if (saved?.id) {
        const index = state.rooms.findIndex((item) => item.id === saved.id);
        if (index >= 0) {
            state.rooms[index] = saved;
        }
        state.draft = {...saved};
        toast(`Настройки сохранены: ${saved.title || UNTITLED}`, 'success');
    } else {
        // fetch положил причину в state.error: молча пропавший тост человек
        // принял бы за «ничего не изменилось».
        toast(state.error || 'Не удалось сохранить', 'error', 8);
    }
    render();
}

// remove спрашивает подтверждение обычным confirm: это единственное место, где
// действие необратимо и нечего подтверждать красивым окном — стандартный
// вопрос браузера читается как «а ты уверен» без лишнего интерфейса.
async function remove() {
    const room = selected();
    if (!room) {
        return;
    }

    if (!window.confirm(`Вы точно хотите удалить комнату "${room.title || UNTITLED}"?`)) {
        return;
    }

    state.error = '';
    const done = await fetchJSON(`${ROOMS_API}/${encodeURIComponent(room.id)}`, {method: 'DELETE'});

    if (!done?.deleted) {
        toast(state.error || 'Не удалось удалить', 'error', 8);
        return;
    }

    state.rooms = state.rooms.filter((item) => item.id !== room.id);
    state.selectedId = null;
    state.draft = null;
    toast(`Комната удалена: ${room.title || UNTITLED}`, 'info');
    render();
}

// add создаёт комнату. Имя даёт ядро — оно знает, какие номера уже заняты.
async function add() {
    state.error = '';
    const room = await fetchJSON(ROOMS_API, {method: 'POST'});

    if (room?.id) {
        state.rooms.push(room);
        state.selectedId = room.id;
        state.draft = {...room};
        toast(`Комната создана: ${room.title || UNTITLED}`, 'success');
    } else {
        toast(state.error || 'Не удалось создать комнату', 'error', 8);
    }
    render();
}

function select(id) {
    state.selectedId = state.selectedId === id ? null : id;
    const room = selected();
    // Черновик берётся из комнаты целиком: предыдущие правки другой комнаты не
    // должны подставляться сюда.
    state.draft = room ? {...room} : null;
    render();
}

function selected() {
    return state.rooms.find((room) => room.id === state.selectedId) ?? null;
}

// --- Тосты --------------------------------------------------------------------

// toast — короткая весть о том, что произошло. Собрана из тех же кирпичиков,
// что и на странице плагинов: та же разметка, та же полоса жизни. Своя копия
// потому, что модуль на страницу, а тост нужен обеим.
function toast(text, severity = 'info', seconds = 5) {
    const node = el('article', {
        class: 'msg toast',
        'data-severity': severity,
        role: 'status',
    }, [
        el('div', {class: 'msg__head'}, [
            el('span', {class: 'msg__author msg__author--plugin'}, ['[Комнаты]']),
        ]),
        el('div', {class: 'msg__rule'}),
        el('div', {class: 'msg__text'}, [sentence(text)]),
        el('span', {class: 'msg__life', style: `--life: ${seconds}s`}),
    ]);

    // Под тостом может стоять такой же: разводим их по высоте, иначе полосы
    // жизни слипнутся в одну.
    toastsEl.append(node);

    const remove = () => node.remove();
    const timer = setTimeout(remove, seconds * 1000);

    // Убрали раньше, чем истекло время, — убираем и таймер, иначе он выстрелит
    // по уже несуществующему узлу.
    node.addEventListener('transitionend', () => {
        if (node.dataset.gone) {
            return;
        }
        node.dataset.gone = '';
        clearTimeout(timer);
        remove();
    }, {once: true});
}

// sentence начинает фразу с заглавной — включая часть после двоеточия.
function sentence(text) {
    const trimmed = String(text ?? '').trim();
    if (!trimmed) {
        return '';
    }
    return trimmed
        .split(/:\s+/)
        .map((part) => part.charAt(0).toLocaleUpperCase('ru') + part.slice(1))
        .join(': ');
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

document.getElementById('rooms-add')?.addEventListener('click', add);
load();
