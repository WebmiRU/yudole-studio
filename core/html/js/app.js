// Страница настроек: список чат-клиентов и форма их опций.
//
// Прототип: данные лежат здесь же, в CHAT_CLIENTS. Когда появится API, этот
// файл заменит fetch('/api/plugins/chat'), а рендер останется прежним.

import '../sass/app.sass';

const STORAGE_PREFIX = 'studio.options.';

const CHAT_CLIENTS = [
    {
        id: 'twitch',
        name: 'Twitch',
        mark: 'TW',
        description: 'Чат через Twitch IRC',
        fields: [
            {
                id: 'nick',
                label: 'Ник в чате',
                type: 'text',
                placeholder: 'my_nickname',
                required: true,
            },
            {
                id: 'token',
                label: 'OAuth-токен',
                type: 'password',
                mono: true,
                placeholder: 'oauth:xxxxxxxxxxxx',
                hint: 'Токен выдаёт Twitch на twitch.tv/chat-settings. Читать каналы можно и без него.',
            },
            {
                id: 'channels',
                label: 'Каналы',
                type: 'tags',
                placeholder: 'канал, Enter — добавить',
                hint: 'Подключаться сразу к нескольким каналам.',
            },
            {
                id: 'showInMain',
                label: 'Сообщения в общую ленту',
                type: 'switch',
                value: true,
            },
            {
                id: 'autoconnect',
                label: 'Подключаться при запуске',
                type: 'switch',
                value: true,
            },
        ],
    },
    {
        id: 'goodgame',
        name: 'GoodGame',
        mark: 'GG',
        description: 'Чат игровой площадки goodgame.ru',
        fields: [
            {
                id: 'login',
                label: 'Логин',
                type: 'text',
                placeholder: 'user или e-mail',
                required: true,
            },
            {
                id: 'password',
                label: 'Пароль',
                type: 'password',
                placeholder: '••••••••',
            },
            {
                id: 'room',
                label: 'Комната по умолчанию',
                type: 'select',
                choices: ['Общий чат', 'Игровая комната', 'Личный блог', 'Прямой эфир'],
            },
            {
                id: 'games',
                label: 'Игры',
                type: 'tags',
                placeholder: 'игра, Enter — добавить',
                hint: 'Ограничить ленту сообщениями из этих игровых комнат.',
            },
            {
                id: 'autoconnect',
                label: 'Подключаться при запуске',
                type: 'switch',
                value: true,
            },
        ],
    },
];

const ENABLED_FIELD = { id: 'enabled', label: 'Плагин включён', type: 'switch', value: true };

// Выбранный клиент живёт в хеше адреса (#twitch): переживает перезагрузку
// страницы, даёт прямые ссылки и работает с кнопкой «назад».
const state = {
    selectedId: clientIdFromHash() ?? CHAT_CLIENTS[0]?.id ?? null,
    draft: Object.fromEntries(CHAT_CLIENTS.map((client) => [client.id, loadValues(client)])),
};

const clientsEl = document.getElementById('clients');
const optionsEl = document.getElementById('options');
const countEl = document.getElementById('clients-count');
const toastEl = document.getElementById('toast');

// --- Значения опций --------------------------------------------------------

function defaultsOf(client) {
    const values = { enabled: true };
    for (const field of client.fields) {
        switch (field.type) {
            case 'tags':
                values[field.id] = [];
                break;
            case 'switch':
                values[field.id] = Boolean(field.value);
                break;
            case 'select':
                values[field.id] = field.choices[0] ?? '';
                break;
            default:
                values[field.id] = '';
        }
    }
    return values;
}

function loadValues(client) {
    const values = defaultsOf(client);
    let saved;
    try {
        saved = JSON.parse(localStorage.getItem(STORAGE_PREFIX + client.id) ?? 'null');
    } catch {
        saved = null;
    }
    if (!saved || typeof saved !== 'object') {
        return values;
    }
    for (const [key, value] of Object.entries(saved)) {
        if (key in values) {
            values[key] = value;
        }
    }
    return values;
}

function persist(client) {
    try {
        localStorage.setItem(STORAGE_PREFIX + client.id, JSON.stringify(state.draft[client.id]));
    } catch {
        // Приватный режим или переполненное хранилище — для прототипа не важно.
    }
}

// --- Статус клиента --------------------------------------------------------

function statusOf(client) {
    const values = state.draft[client.id];
    if (!values.enabled) {
        return { kind: 'off', text: 'Выключен' };
    }
    const missing = client.fields.some((field) => field.required && isEmpty(values[field.id]));
    return missing ? { kind: 'warn', text: 'Требует настроек' } : { kind: 'ok', text: 'Готов' };
}

function isEmpty(value) {
    return Array.isArray(value) ? value.length === 0 : !String(value ?? '').trim();
}

function summaryOf(client) {
    const values = state.draft[client.id];
    const list = values.channels ?? values.games;
    if (Array.isArray(list) && list.length) {
        return list.join(', ');
    }
    return values.nick || values.login || values.room || 'не настроен';
}

// --- Рендер ----------------------------------------------------------------

function render() {
    countEl.textContent = String(CHAT_CLIENTS.length);
    renderClients();
    renderOptions();
}

function renderClients() {
    const items = CHAT_CLIENTS.map((client) => {
        const selected = client.id === state.selectedId;
        const button = el('button', {
            type: 'button',
            class: 'client',
            'aria-pressed': String(selected),
            'data-client': client.id,
        }, [
            el('span', { class: 'client__logo', 'aria-hidden': 'true' }, [client.mark]),
            el('span', { class: 'client__info' }, [
                el('span', { class: 'client__name' }, [client.name]),
                el('span', { class: 'client__meta' }, [summaryOf(client)]),
            ]),
        ]);
        button.addEventListener('click', () => select(client.id));

        return el('li', { class: 'client-row' + (selected ? ' is-selected' : '') }, [
            button,
            enabledToggle(client),
        ]);
    });

    clientsEl.replaceChildren(...items);
}

// Тумблер вкл/выкл прямо в списке. Это отдельная метка, а не вложенный input:
// кнопка выбора клиента не должна содержать интерактивный элемент.
function enabledToggle(client) {
    const input = el('input', {
        type: 'checkbox',
        class: 'switch__input',
        id: `${client.id}-enabled-list`,
        'data-toggle-enabled': client.id,
        'aria-label': `${client.name}: включить плагин`,
    });
    input.checked = Boolean(state.draft[client.id].enabled);

    input.addEventListener('change', () => setEnabled(client, input.checked));

    return el('label', { class: 'switch switch--compact' }, [
        input,
        el('span', { class: 'switch__track', 'aria-hidden': 'true' }),
    ]);
}

function statusBadge(status) {
    return el('span', {
        class: 'status status--' + status.kind,
        title: status.text,
    }, [status.text]);
}

function renderOptions() {
    const client = CHAT_CLIENTS.find((item) => item.id === state.selectedId);
    if (!client) {
        optionsEl.replaceChildren(el('p', { class: 'empty' }, ['Чат-клиенты не установлены']));
        return;
    }

    const values = state.draft[client.id];
    const form = el('form', { class: 'options', novalidate: '' });

    form.append(
        el('header', { class: 'options__head' }, [
            el('div', {}, [
                el('h2', { class: 'options__title' }, [client.name]),
                el('p', { class: 'options__desc' }, [client.description]),
            ]),
            statusBadge(statusOf(client)),
        ]),
        el('div', { class: 'options__body' }, [
            renderRow(client, ENABLED_FIELD),
            ...client.fields.map((field) => renderRow(client, field)),
        ]),
        el('footer', { class: 'options__foot' }, [
            el('span', { class: 'options__note' }, ['Пока это прототип: настройки живут в браузере']),
            el('span', { class: 'options__actions' }, [
                el('button', { type: 'button', class: 'btn btn--ghost' }, ['Сбросить']),
                el('button', { type: 'submit', class: 'btn btn--primary' }, ['Сохранить']),
            ]),
        ]),
    );

    form.addEventListener('submit', (event) => {
        event.preventDefault();
        const invalid = validate(form, client);
        if (invalid) {
            invalid.querySelector('input, select, .tags__input')?.focus();
            toast('Заполните обязательные поля');
            return;
        }
        persist(client);
        render();
        toast(`${client.name}: настройки сохранены`);
    });

    form.querySelector('.btn--ghost').addEventListener('click', () => {
        state.draft[client.id] = defaultsOf(client);
        persist(client);
        render();
        toast(`${client.name}: настройки сброшены`);
    });

    optionsEl.replaceChildren(form);
}

function renderRow(client, field) {
    const values = state.draft[client.id];
    const control = el('div', { class: 'field__control' });

    if (field.type === 'switch') {
        control.append(renderSwitch(client, values, field));
    } else if (field.type === 'tags') {
        control.append(renderTags(client, values, field));
    } else if (field.type === 'select') {
        control.append(renderSelect(client, values, field));
    } else {
        control.append(renderInput(client, values, field));
    }

    if (field.required) {
        control.append(el('p', { class: 'field__error' }, ['']));
    }

    // У переключателя своя подпись рядом с тумблером, колонка-label не нужна.
    let label = el('span', { class: 'field__label field__label--empty', 'aria-hidden': 'true' });
    if (field.type !== 'switch') {
        label = el('label', { class: 'field__label', for: `${client.id}-${field.id}` }, [field.label]);
        if (field.hint) {
            label.append(el('span', { class: 'field__hint' }, [field.hint]));
        }
    }

    return el('div', { class: 'field', 'data-field': field.id }, [label, control]);
}

function renderInput(client, values, field) {
    const input = el('input', {
        class: 'input' + (field.mono ? ' input--mono' : ''),
        id: `${client.id}-${field.id}`,
        type: field.type === 'password' ? 'password' : 'text',
        placeholder: field.placeholder ?? '',
        autocomplete: 'off',
    });
    input.value = values[field.id] ?? '';
    input.addEventListener('input', (event) => {
        values[field.id] = input.value;
        changed(client, event.target);
    });
    return input;
}

function renderSelect(client, values, field) {
    const select = el('select', { class: 'select', id: `${client.id}-${field.id}` },
        field.choices.map((choice) => el('option', { value: choice }, [choice])));
    select.value = values[field.id] ?? field.choices[0] ?? '';
    select.addEventListener('change', (event) => {
        values[field.id] = select.value;
        changed(client, event.target);
    });
    return select;
}

function renderSwitch(client, values, field) {
    const input = el('input', {
        type: 'checkbox',
        class: 'switch__input',
        id: `${client.id}-${field.id}`,
        ...(field.id === 'enabled' ? { 'data-toggle-enabled': client.id } : {}),
    });
    input.checked = Boolean(values[field.id]);

    input.addEventListener('change', () => {
        if (field.id === 'enabled') {
            setEnabled(client, input.checked);
            return;
        }
        values[field.id] = input.checked;
        changed(client, input);
    });

    return el('label', { class: 'switch' }, [
        input,
        el('span', { class: 'switch__track', 'aria-hidden': 'true' }),
        el('span', { class: 'switch__text' }, [field.label]),
    ]);
}

// Список тегов: Enter или запятая добавляет значение, Backspace в пустом поле
// убирает последнее.
function renderTags(client, values, field) {
    const box = el('div', { class: 'tags' });
    const items = values[field.id];
    const input = el('input', { class: 'tags__input', id: `${client.id}-${field.id}`, autocomplete: 'off' });

    const draw = () => {
        box.replaceChildren(
            ...items.map((item) => {
                const remove = el('button', {
                    type: 'button',
                    class: 'tag__remove',
                    'aria-label': `Убрать ${item}`,
                }, ['×']);
                remove.addEventListener('click', () => {
                    items.splice(items.indexOf(item), 1);
                    draw();
                    changed(client, remove);
                });
                return el('span', { class: 'tag' }, [item, remove]);
            }),
        );
        if (!items.length) {
            box.append(el('span', { class: 'tags__empty' }, [field.placeholder ?? 'пусто']));
        }
        input.placeholder = items.length ? 'добавить' : '';
        box.append(input);
    };

    const add = () => {
        const value = input.value.trim().replace(/^#/, '');
        if (!value || items.includes(value)) {
            input.value = '';
            return;
        }
        items.push(value);
        input.value = '';
        draw();
        changed(client, input);
    };

    input.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ',') {
            event.preventDefault();
            add();
        } else if (event.key === 'Backspace' && !input.value && items.length) {
            items.pop();
            draw();
            changed(client, input);
        }
    });
    input.addEventListener('blur', add);
    box.addEventListener('click', (event) => {
        if (event.target === box) {
            input.focus();
        }
    });

    draw();
    return box;
}

// --- Валидация и обновление статусов --------------------------------------

function validate(form, client) {
    const values = state.draft[client.id];
    let firstInvalid = null;

    for (const field of client.fields) {
        const row = form.querySelector(`[data-field="${field.id}"]`);
        const error = row?.querySelector('.field__error');
        if (!error) {
            continue;
        }
        if (field.required && isEmpty(values[field.id])) {
            error.textContent = 'Обязательное поле';
            firstInvalid ??= row;
        } else {
            error.textContent = '';
        }
    }
    return firstInvalid;
}

// Обновляет список клиентов и бейджи статуса, не трогая форму: перерисовка
// снесла бы фокус и позицию курсора в поле ввода.
function changed(client, source) {
    if (source) {
        source.closest('[data-field]')?.querySelector('.field__error')?.replaceChildren();
    }

    renderClients();

    const headerBadge = optionsEl.querySelector('.options__head .status');
    if (headerBadge) {
        headerBadge.replaceWith(statusBadge(statusOf(client)));
    }
}

// Тумблер «вкл/выкл» есть и в списке, и в шапке формы — держат одно значение
// и обновляются вместе, форма при этом не перерисовывается.
function setEnabled(client, enabled) {
    state.draft[client.id].enabled = enabled;

    renderClients();

    for (const input of optionsEl.querySelectorAll('[data-toggle-enabled]')) {
        input.checked = enabled;
    }

    const headerBadge = optionsEl.querySelector('.options__head .status');
    if (headerBadge) {
        headerBadge.replaceWith(statusBadge(statusOf(client)));
    }
}

function select(id, { updateHash = true } = {}) {
    if (!CHAT_CLIENTS.some((client) => client.id === id)) {
        return;
    }
    if (id === state.selectedId) {
        return;
    }
    state.selectedId = id;
    if (updateHash) {
        location.hash = id;
    }
    render();
}

// Хеш меняют и руками (ссылка, кнопка «назад»), и кликом по клиенту.
window.addEventListener('hashchange', () => {
    select(clientIdFromHash() ?? CHAT_CLIENTS[0]?.id, { updateHash: false });
});

function clientIdFromHash() {
    const id = location.hash.replace(/^#/, '');
    return CHAT_CLIENTS.some((client) => client.id === id) ? id : null;
}

// --- Вспомогательное -------------------------------------------------------

let toastTimer = 0;

function toast(text) {
    toastEl.textContent = text;
    toastEl.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
        toastEl.hidden = true;
    }, 2500);
}

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

render();