// Страница настроек: список чат-клиентов и форма их опций.
//
// Плагины приходят с ядра по websocket: GET /api/plugins/events. Сначала ядро
// отдаёт все плагины разом, а потом само сообщает, что с каждым из них
// происходит — запустился, умер, занят. Опроса нет намеренно: он ходил каждые
// две секунды и затирал наполовину набранное в форме, а человек думал, что у
// него не работает ввод.

import '../sass/app.sass';

const API = '/api/plugins/chat';
const EVENTS = '/api/plugins/events';

const state = {
    plugins: [],
    // Соединение с состояниями плагинов: единственный источник, откуда мы
    // узнаём, что с ними происходит.
    socket: null,
    selectedId: pluginIdFromHash(),
    // Черновик значений выбранного плагина, до нажатия «Сохранить».
    values: {},
    // Правлено ли что-то в черновике: нужно, чтобы переподключение не
    // подменило напечатанное свежим от ядра.
    dirty: false,
    // Плагины, для которых сейчас идёт enable/disable: переключатель нельзя
    // дёргать второй раз, пока процесс поднимается.
    busy: new Set(),
    // Идёт сохранение: кнопка ждёт ответа плагина и показывает спиннер.
    saving: false,
    // Чем плагин занят прямо сейчас.
    waitingText: '',
    // Что плагин сказал о последней попытке: держим, чтобы пометки об ошибке
    // пережили перерисовку.
    fieldError: null,
};

const clientsEl = document.getElementById('clients');
const optionsEl = document.getElementById('options');
const countEl = document.getElementById('clients-count');
const toastsEl = document.getElementById('toasts');


// --- Обращение к ядру -------------------------------------------------------

async function api(path, options = {}) {
    const response = await fetch(path, {
        headers: { 'Content-Type': 'application/json' },
        ...options,
    });

    const text = await response.text();
    let data = {};
    if (text) {
        try {
            data = JSON.parse(text);
        } catch {
            throw new Error('ядро ответило не JSON');
        }
    }
    if (!response.ok) {
        const error = new Error(data.error || `HTTP ${response.status}`);
        error.fields = data.fields ?? null;
        error.items = data.items ?? null;
        error.status = response.status;
        throw error;
    }
    return data;
}

// listenStates держит открытым канал состояний плагинов.
//
// Ядро отдаёт при подключении все плагины разом, а потом шлёт то, что изменилось.
// Разрывы случаются: ядро перезапустилось, прокси разорвал соединение. Тогда
// подключаемся заново — список плагинов с каждым разом придёт и сам по себе
// обновит страницу.
function listenStates() {
    const url = new URL(EVENTS, location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';

    const socket = new WebSocket(url);
    state.socket = socket;

    socket.addEventListener('open', () => {
        // Ядро ответило: списываем «недоступно», если оно было показано.
        shownOffline = false;
    });

    socket.addEventListener('message', (event) => {
        let message;
        try {
            message = JSON.parse(event.data);
        } catch {
            return;
        }

        if (message.hello) {
            for (const view of message.hello.plugins ?? []) {
                applyView(view);
            }

            const gone = !state.plugins.some((plugin) => plugin.id === state.selectedId);
            if (gone) {
                state.selectedId = state.plugins[0]?.id ?? null;
            }
            render();

            // Настроек в потоке нет: там пароли, а состояния приходят десятки
            // раз в час. Значит, спрашиваем их отдельно — один раз, и только
            // для открытого плагина.
            loadValues();
            return;
        }

        if (message.plugin && applyView(message.plugin)) {
            render();
        }
    });

    socket.addEventListener('close', () => {
        showOffline();
        // Ядро может перезапускаться: пробуем сами, но не сразу.
        setTimeout(listenStates, 2000);
    });
    socket.addEventListener('error', () => socket.close());
}

// applyView переносит из ответа ядра состояние плагина: запущен, занят, чем
// именно.
//
// Значения формы сюда НЕ переносятся. Это черновик, который человек набирает
// прямо сейчас, а ответ ядра — про то, чем занят плагин. Сливать их в одну кучу
// нельзя: события приходят в любой момент, и человек не успевает вписать канал —
// написанное стиралось бы на полуслове, а курсор уезжал в начало поля.
//
// Возвращает, изменилось ли что-нибудь: перерисовывать без причины незачем.
function applyView(view) {
    const index = state.plugins.findIndex((plugin) => plugin.id === view.id);
    const previous = index === -1 ? null : state.plugins[index];

    const next = previous ? {...previous} : {...view};
    // Список полей, которые приходят событием. Значения формы (values) здесь
    // быть не должны: это черновик человека, а не то, чем занят плагин.
    //
    // Всё, что ядро присылает в PluginView, обязано быть в этом списке. Забытое
    // поле молча замирает на первом значении: страница загрузилась, увидела
    // «подключается» — и больше не двигалась, хотя связь давно онлайн.
    const fromView = [
        'state',
        'problem',
        'waiting',
        'waitingText',
        'connection',
        'connectionText',
        'version',
        'title',
    ];
    for (const key of fromView) {
        next[key] = view[key];
    }

    if (index === -1) {
        state.plugins.push(next);
        return true;
    }

    state.plugins[index] = next;
    return JSON.stringify(next) !== JSON.stringify(previous);
}

function replacePlugin(view) {
    const index = state.plugins.findIndex((plugin) => plugin.id === view.id);
    if (index === -1) {
        state.plugins.push(view);
    } else {
        state.plugins[index] = view;
    }
    if (view.id === state.selectedId) {
        state.values = cloneValues(view.values);
        // Ядро только что сохранило то, что человек нажал «Сохранить»: черновик
        // совпадает с сохранённым, и помечать его как правленый незачем.
        state.dirty = false;
    }
}

function selected() {
    return state.plugins.find((plugin) => plugin.id === state.selectedId) ?? null;
}

// --- Значения формы --------------------------------------------------------

function resetValues() {
    state.values = cloneValues(selected()?.values);
    state.dirty = false;
}

// loadValues спрашивает настройки открытого плагина. Значения не едут в потоке
// состояний: там среди них пароли, а состояния приходят десятки раз в час.
// Забрать их надо один раз — и только для того плагина, чья форма открыта.
async function loadValues() {
    const id = state.selectedId;
    if (!id) {
        return;
    }

    let view;
    try {
        view = await api(`${API}/${id}`);
    } catch {
        // Ядро не ответило: покажем то, что есть, форма разъедется сама.
        return;
    }

    if (state.selectedId !== id) {
        return;
    }

    const index = state.plugins.findIndex((plugin) => plugin.id === id);
    if (index !== -1) {
        state.plugins[index] = {...state.plugins[index], values: view.values ?? {}};
    }

    // Набранное не трогаем: человек мог уже править.
    if (!state.dirty) {
        resetValues();
    }
    render();
}

// cloneValues копирует настройки для формы. Копия должна быть настоящей: если
// поделить массив с сохранёнными значениями, правка в поле изменит и их, и
// «Сбросить» будет не от чего откатывать.
function cloneValues(values) {
    const out = {};
    for (const [key, value] of Object.entries(values ?? {})) {
        out[key] = Array.isArray(value)
            ? value.map((entry) => (entry && typeof entry === 'object' ? {...entry} : entry))
            : value;
    }
    return out;
}

function isEmpty(value) {
    return Array.isArray(value) ? value.length === 0 : !String(value ?? '').trim();
}

// --- Статус плагина --------------------------------------------------------

// linkOf — это про связь с чатом, а statusOf ниже — про то, в порядке ли
// плагин. Разные вещи: плагин может быть включён и настроен, а в чат при
// этом не входить.
const LINKS = {
    connecting: {kind: 'busy', text: 'Подключается'},
    online: {kind: 'ok', text: 'Онлайн'},
    reconnecting: {kind: 'warn', text: 'Переподключается'},
    offline: {kind: 'error', text: 'Офлайн'},
};

// linkOf отвечает, что с подключением к чату. Плагин говорит об этом сам и
// меняет состояние без всякого опроса, так что здесь всё уже известно.
function linkOf(plugin) {
    if (!plugin.enabled || plugin.state === 'off') {
        return {kind: 'off', text: 'Выключен'};
    }
    if (plugin.state === 'failed') {
        return {kind: 'error', text: 'Не запущен', title: plugin.problem ?? ''};
    }

    const link = LINKS[plugin.connection];
    if (link) {
        return {kind: link.kind, text: link.text, title: plugin.connectionText ?? ''};
    }
    // Молчит: включён, но ещё ничего не сказал. Так бывает между запуском
    // процесса и первой попыткой встать в чат.
    return {kind: 'off', text: 'Не в сети'};
}

function statusOf(plugin) {
    if (state.busy.has(plugin.id)) {
        return {kind: 'busy', text: plugin.enabled ? 'Выключается…' : 'Включается…'};
    }
    if (plugin.state === 'failed') {
        return {kind: 'error', text: 'Ошибка', title: plugin.problem ?? ''};
    }
    if (!plugin.enabled) {
        return {kind: 'off', text: 'Выключен'};
    }
    const missing = (plugin.options ?? []).some((option) => option.required && isEmpty(stateValue(plugin, option.key)));
    return missing ? {kind: 'warn', text: 'Требует настроек'} : {kind: 'ok', text: 'Готов'};
}

// Значение опции: черновик формы, если плагин выбран, иначе сохранённое.
function stateValue(plugin, key) {
    if (plugin.id !== state.selectedId) {
        return plugin.values?.[key];
    }
    return key in state.values ? state.values[key] : plugin.values?.[key];
}

// --- Рендер ----------------------------------------------------------------

function render() {
    // Форма пересобирается целиком, и если человек в этот момент печатает, узел
    // под курсором просто исчезает. Запоминаем, где был курсор, и возвращаем
    // после сборки — иначе перерисовка выглядит как зависание страницы.
    const mark = rememberFocus();

    countEl.textContent = String(state.plugins.length);
    renderClients();
    renderOptions();

    restoreFocus(mark);
}

// rememberFocus is where the caret is: имя поля, позиция и выделение. Без
// выделения не обойтись — иначе выделенное пропадёт при первом же рендере.
function rememberFocus() {
    const active = document.activeElement;
    if (!active || !active.name || !optionsEl.contains(active)) {
        return null;
    }

    const start = typeof active.selectionStart === 'number' ? active.selectionStart : null;
    const end = typeof active.selectionEnd === 'number' ? active.selectionEnd : null;

    return {
        name: active.name,
        start,
        end,
        // Прокрутка страницы: если человек внизу длинного списка каналов,
        // перерисовка не должна выбрасывать его наверх.
        top: window.scrollY,
    };
}

function restoreFocus(mark) {
    if (!mark) {
        return;
    }

    window.scrollTo({top: mark.top});

    // Ищем по имени, а не по порядковому номеру: нумерация съехала бы, если
    // бы плагин тем временем прислал другой список вариантов.
    const field = optionsEl.querySelector(`[name="${CSS.escape(mark.name)}"]`);
    if (!field) {
        return;
    }

    field.focus({preventScroll: true});
    if (mark.start !== null && typeof field.setSelectionRange === 'function') {
        try {
            field.setSelectionRange(mark.start, mark.end);
        } catch {
            // Не текстовое поле — ставить выделение некуда.
        }
    }
}

function renderClients() {
    if (!state.plugins.length) {
        clientsEl.replaceChildren(el('li', {class: 'panel__empty'},
            [el('p', {class: 'empty'}, ['Плагины не найдены в plugins/chat'])]));
        return;
    }

    clientsEl.replaceChildren(...state.plugins.map((plugin) => {
        const selectedNow = plugin.id === state.selectedId;

        const button = el('button', {
            type: 'button',
            class: 'client',
            'aria-pressed': String(selectedNow),
            'data-plugin': plugin.id,
        }, [
            el('span', {class: 'client__logo', 'aria-hidden': 'true'}, [markOf(plugin)]),
            el('span', {class: 'client__info'}, [
                el('span', {class: 'client__name'}, [plugin.title || plugin.id]),
                // Под названием — связь с чатом, а не «настроен»: настроен ли
                // плагин, и так видно по форме справа, а вот в сети ли он —
                // не видно нигде.
                statusBadge(linkOf(plugin)),
            ]),
        ]);
        button.addEventListener('click', () => select(plugin.id));

        return el('li', {class: 'client-row' + (selectedNow ? ' is-selected' : '')}, [
            button,
            enabledToggle(plugin),
        ]);
    }));
}

function markOf(plugin) {
    const title = plugin.title || plugin.id;
    return title.slice(0, 2).toUpperCase();
}

function enabledToggle(plugin) {
    const input = el('input', {
        type: 'checkbox',
        class: 'switch__input',
        // Отдельный id: в форме есть свой переключатель с ключом enabled.
        id: `${plugin.id}-toggle`,
        'aria-label': `${plugin.title || plugin.id}: включить плагин`,
    });
    input.checked = Boolean(plugin.enabled);
    input.disabled = state.busy.has(plugin.id);

    input.addEventListener('change', () => toggle(plugin, input.checked));

    const label = el('label', {class: 'switch switch--compact'}, [
        input,
        el('span', {class: 'switch__track', 'aria-hidden': 'true'}),
    ]);
    if (state.busy.has(plugin.id)) {
        label.classList.add('is-pending');
    }
    return label;
}

function renderOptions() {
    const plugin = selected();
    if (!plugin) {
        optionsEl.replaceChildren(el('p', {class: 'empty'}, ['Выберите плагин слева']));
        return;
    }

    const form = el('form', {class: 'options', novalidate: ''});
    const status = statusOf(plugin);

    const header = [
        el('h2', {class: 'options__title'}, [plugin.title || plugin.id]),
        el('p', {class: 'options__desc'}, [plugin.description ?? '']),
    ];
    if (plugin.state === 'failed' && plugin.problem) {
        header.push(el('p', {class: 'options__problem'}, [plugin.problem]));
    }
    if (plugin.version) {
        header.push(el('p', {class: 'options__version'}, ['версия ' + plugin.version]));
    }

    form.append(
        el('header', {class: 'options__head'}, [
            el('div', {}, header),
            statusBadge(status),
        ]),
        el('div', {class: 'options__body'}, renderBody(plugin)),
        el('footer', {class: 'options__foot'}, [
            el('span', {class: 'options__note'}, [
                plugin.enabled ? 'Настройки сохраняются в config.json' : 'Плагин выключен — настройки сохраняются, но не применяются',
            ]),
            el('span', {class: 'options__actions'}, [
                el('button', {type: 'button', class: 'btn btn--ghost btn--reset', disabled: state.saving}, ['Сбросить']),
                saveButton(plugin),
            ]),
        ]),
    );

    form.addEventListener('submit', (event) => {
        event.preventDefault();
        save(plugin);
    });
    // Класс свой, а не .btn--ghost: такой же стиль есть у кнопки «Добавить»
    // в списке значений, и запрос по классу цеплял её вместо сброса.
    // И сообщать о сбросе нечем — изменения сразу видны в полях.
    form.querySelector('.btn--reset').addEventListener('click', () => {
        resetValues();
        state.fieldError = null;
        render();
    });

    optionsEl.replaceChildren(form);

    // Пометки об ошибке накладываем после сборки формы: любая перерисовка
    // сохраняет их, а не стирает.
    if (state.fieldError) {
        showFieldErrors(plugin, state.fieldError.fields, state.fieldError.items);
    }
}

// saveButton во время сохранения показывает спиннер и то, чем занят плагин.
function saveButton(plugin) {
    const busy = state.saving || Boolean(plugin.waiting);
    const label = busy
        ? sentence(state.waitingText || plugin.waitingText || 'Сохраняем…')
        : 'Сохранить';

    const children = busy
        ? [el('span', {class: 'spinner', 'aria-hidden': 'true'}), label]
        : [label];

    return el('button', {
        type: 'submit',
        class: 'btn btn--primary',
        disabled: busy,
    }, children);
}

// renderBody lays the settings out in sections. Разделы объявляет плагин: у
// одного плагина поля и кнопки могут тянуться к разным вещам, и в одной куче
// они неразличимы.
function renderBody(plugin) {
    const sections = new Map();

    const put = (name, node) => {
        const title = name || '';
        if (!sections.has(title)) {
            sections.set(title, {title, nodes: []});
        }
        sections.get(title).nodes.push(node);
    };

    for (const option of plugin.options ?? []) {
        put(option.section, renderRow(plugin, option));
    }

    for (const button of plugin.buttons ?? []) {
        put(button.section, renderButton(plugin, button));
    }

    if (!sections.size) {
        return [el('p', {class: 'empty'},
            ['Включите плагин: ядро запустит его, и тот расскажет, какие у него настройки'])];
    }

    const rows = [];
    for (const {title, nodes} of sections.values()) {
        if (title) {
            rows.push(el('h3', {class: 'options__section'}, [title]));
        }
        rows.push(...nodes);
    }
    return rows;
}

// renderButton is an action: нажали — ядро отдало команду плагину. Пока плагин
// занят, кнопка не нажимается ещё раз.
function renderButton(plugin, button) {
    // Занятость берём и у себя (наш клик), и у плагина: он сам сообщает через
    // waiting, пока занят. Иначе обновление, начатое не этой страницей —
    // curl'ом или со второй вкладки — кнопку бы не погасила.
    const busy = state.running === button.kind || Boolean(plugin.waiting);
    const children = busy
        ? [el('span', {class: 'spinner', 'aria-hidden': 'true'}), button.title]
        : [button.title];

    const node = el('button', {
        type: 'button',
        class: 'btn btn--action',
        'data-action': button.kind,
        disabled: !plugin.enabled || busy,
    }, children);

    node.addEventListener('click', () => runAction(plugin, button));

    const row = el('div', {class: 'action'}, [node]);
    if (button.hint) {
        row.append(el('p', {class: 'action__hint'}, [button.hint]));
    }
    if (plugin.waiting && plugin.waitingText) {
        // Плагин пишет как удобно ему, а на странице это фраза: с большой
        // буквы.
        row.append(el('p', {class: 'action__busy'}, [sentence(plugin.waitingText)]));
    }
    return row;
}

// runAction asks the kernel to forward the command to the plugin. Ждём ответа:
// команда может быть долгой, и страница должна показать, что идёт дело.
async function runAction(plugin, button) {
    if (state.running || selected()?.waiting) {
        return;
    }
    state.running = button.kind;
    render();

    try {
        const answer = await api(`${API}/${plugin.id}/actions`, {
            method: 'POST',
            body: JSON.stringify({kind: button.kind}),
        });
        // Кнопка может изменить то, что плагин показывает, поэтому забираем
        // его свежий вид. Занятость придёт сама по событию: плагин сообщает
        // её, как только уйдёт в сеть.
        const view = await api(`${API}/${plugin.id}`);
        replacePlugin(view);
        // Что именно вышло, сказал сам плагин: «готово» после кнопки про
        // смайлы не значит ничего.
        toast(answer.message || 'Готово', 'success');
    } catch (error) {
        toast(error.message, 'error');
    } finally {
        state.running = '';
        render();
    }
}

function renderRow(plugin, option) {
    const id = `${plugin.id}-${option.key}`;
    const control = el('div', {class: 'field__control'});

    switch (option.type) {
        case 'bool':
            control.append(renderSwitch(plugin, option, id));
            break;
        case 'select':
            control.append(renderSelect(plugin, option, id));
            break;
        case 'text/multiple':
            // Список строк — подписи поля не нужны: каждая строка со своим вводом.
            control.append(renderList(plugin, option));
            break;
        case 'select/multiple':
            // Варианты с названиями: «E.Wolf» вместо «53029».
            control.append(renderChoices(plugin, option));
            break;
        default:
            control.append(renderInput(plugin, option, id));
    }

    // Слот для ошибки нужен у любого поля: плагин может не принять и необязательное.
    control.append(el('p', {class: 'field__error'}, ['']));

    let label = el('span', {class: 'field__label field__label--empty', 'aria-hidden': 'true'});
    if (option.type !== 'bool') {
        label = el('label', {class: 'field__label', for: id}, [option.label || option.key]);
        if (option.hint) {
            label.append(el('span', {class: 'field__hint'}, [option.hint]));
        }
    }

    return el('div', {class: 'field', 'data-field': option.key}, [label, control]);
}

function renderInput(plugin, option, id) {
    const input = el('input', {
        class: 'input',
        name: option.key,
        id,
        type: option.type === 'password' ? 'password' : option.type === 'number' ? 'number' : 'text',
        placeholder: option.placeholder ?? '',
        autocomplete: 'off',
    });
    input.value = stateValue(plugin, option.key) ?? '';

    input.addEventListener('input', () => {
        state.values[option.key] = option.type === 'number' ? Number(input.value) : input.value;
        changed(plugin, input);
    });
    return input;
}

function renderSelect(plugin, option, id) {
    const select = el('select', {class: 'select', id, name: option.key},
        (option.choices ?? []).map((choice) => el('option', {value: choice.value}, [choice.label ?? choice.value])));

    select.value = stateValue(plugin, option.key) ?? (option.choices?.[0]?.value ?? '');
    select.addEventListener('change', () => {
        state.values[option.key] = select.value;
        changed(plugin, select);
    });
    return select;
}

function renderSwitch(plugin, option, id) {
	const input = el('input', {
		type: 'checkbox',
		class: 'switch__input',
		id: id ?? `${plugin.id}-${option.key}`,
	});
	input.checked = Boolean(stateValue(plugin, option.key));

	input.addEventListener('change', () => {
		state.values[option.key] = input.checked;
		changed(plugin, input);
	});

	return el('label', {class: 'switch'}, [
		input,
		el('span', {class: 'switch__track', 'aria-hidden': 'true'}),
		el('span', {class: 'switch__text'}, [option.label || option.key]),
	]);
}

// Список значений: вертикальный список текстовых полей. Пустая строка не имеет
// смысла и отбрасывается при сохранении.
function renderList(plugin, option) {
	const list = el('div', {class: 'fields-list'});
	const items = Array.isArray(state.values[option.key])
		? state.values[option.key]
		: (Array.isArray(option.default) ? [...option.default] : []);
	state.values[option.key] = items;

	const commit = (source) => {
		state.values[option.key] = items.filter((item) => String(item).trim() !== '');
		changed(plugin, source);
	};

	const draw = () => {
		list.replaceChildren(...items.map((item, index) => {
			const input = el('input', {
				class: 'input',
				name: `${option.key}.${index}`,
				value: item,
				placeholder: option.placeholder ?? '',
				autocomplete: 'off',
			});
			input.addEventListener('input', () => {
				items[index] = input.value;
				input.parentElement.dataset.value = input.value;
				changed(plugin, input);
			});

			const remove = el('button', {
				type: 'button',
				class: 'field-row__remove',
				'aria-label': `Убрать поле ${index + 1}`,
			}, ['\u00d7']);
			remove.addEventListener('click', () => {
				items.splice(index, 1);
				draw();
				commit(remove);
			});

			return el('div', {class: 'field-row', 'data-value': item}, [input, remove]);
		}));

		const add = el('button', {type: 'button', class: 'btn btn--ghost btn--small'},
			[items.length ? '+ \u0415\u0449\u0451' : '+ \u0414\u043e\u0431\u0430\u0432\u0438\u0442\u044c']);
		add.addEventListener('click', () => {
			items.push('');
			draw();
			list.querySelector('.field-row:last-of-type .input')?.focus();
		});
		list.append(add);
	};

	draw();
	return list;
}

// Список с названиями: у каждой записи есть фактическое значение и то, что
// видит пользователь. Плагин присылает варианты в choices, а добавить своё
// можно руками — например канал, которого он не знает.
function renderChoices(plugin, option) {
    const wrap = el('div', {class: 'choices'});
    const picked = Array.isArray(state.values[option.key]) ? state.values[option.key] : [];
    state.values[option.key] = picked;

    const values = new Set(picked.map((entry) => entry.value));
    const commit = (source) => changed(plugin, source);

    const known = new Set();

    const rows = (option.choices ?? []).map((choice) => {
        known.add(choice.value);

        const input = el('input', {type: 'checkbox', class: 'choices__input'});
        input.checked = values.has(choice.value);

        const toggle = () => {
            if (input.checked) {
                values.add(choice.value);
                add({value: choice.value, label: choice.label || choice.value});
            } else {
                values.delete(choice.value);
                remove(choice.value);
            }
            commit(input);
        };
        input.addEventListener('change', toggle);

        return el('label', {class: 'choices__row', 'data-value': choice.value}, [
            input,
            el('span', {class: 'choices__label'}, [choice.label || choice.value]),
            el('span', {class: 'choices__value'}, [choice.value]),
        ]);
    });

    const add = (entry) => {
        if (values.has(entry.value)) {
            return;
        }
        values.add(entry.value);
        picked.push(entry);
        draw();
    };

    const remove = (value) => {
        const index = picked.findIndex((entry) => entry.value === value);
        if (index !== -1) {
            picked.splice(index, 1);
        }
        values.delete(value);
        draw();
    };

    const manual = el('input', {
        class: 'input',
        name: `${option.key}.new`,
        placeholder: option.placeholder ?? 'значение',
    });
    const manualAdd = el('button', {type: 'button', class: 'btn btn--ghost btn--small'}, ['Добавить']);
    manualAdd.addEventListener('click', () => {
        const value = manual.value.trim();
        manual.value = '';
        if (!value) {
            return;
        }
        add({value, label: value});
        commit(manual);
    });
    manual.addEventListener('keydown', (event) => {
        if (event.key === 'Enter') {
            event.preventDefault();
            manualAdd.click();
        }
    });

    const draw = () => {
        wrap.replaceChildren(...rows);

        const extras = picked.filter((entry) => !known.has(entry.value));
        if (extras.length) {
            wrap.append(el('div', {class: 'choices__list'}, extras.map((entry) => {
                const removeButton = el('button', {
                    type: 'button',
                    class: 'field-row__remove',
                    'aria-label': `Убрать ${entry.label || entry.value}`,
                }, ['\u00d7']);
                removeButton.addEventListener('click', () => {
                    remove(entry.value);
                    commit(removeButton);
                });
                return el('div', {class: 'field-row', 'data-value': entry.value}, [
                    el('span', {class: 'choices__added'}, [entry.label || entry.value]),
                    removeButton,
                ]);
            })));
        }

        if (!(option.choices ?? []).length) {
            wrap.append(el('p', {class: 'choices__hint'},
                ['Плагин пока не прислал варианты — можно добавить значения вручную']));
        }

        wrap.append(el('div', {class: 'choices__manual'}, [manual, manualAdd]));
    };

    draw();
    return wrap;
}

// --- Действия --------------------------------------------------------------

async function toggle(plugin, enabled) {
    if (state.busy.has(plugin.id)) {
        return;
    }
    state.busy.add(plugin.id);
    renderClients();

    const name = plugin.title || plugin.id;
    try {
        const action = enabled ? 'enable' : 'disable';
        const view = await api(`${API}/${plugin.id}/${action}`, {method: 'POST'});
        replacePlugin(view);

        if (view.state === 'failed') {
            toast(view.problem || 'не запустился', 'error', 60, name);
        } else {
            toast(enabled ? 'включён' : 'выключен', 'success');
        }
    } catch (error) {
        toast(error.message, 'error');
    } finally {
        state.busy.delete(plugin.id);
        render();
    }
}

function validate(plugin) {
    let firstInvalid = null;

    for (const option of plugin.options ?? []) {
        const row = optionsEl.querySelector(`[data-field="${CSS.escape(option.key)}"]`);
        const error = row?.querySelector('.field__error');
        if (!error) {
            continue;
        }
        if (option.required && isEmpty(state.values[option.key])) {
            error.textContent = 'Обязательное поле';
            firstInvalid ??= row;
        } else {
            error.textContent = '';
        }
    }
    return firstInvalid;
}

function save(plugin) {
    const invalid = validate(plugin);
    if (invalid) {
        invalid.querySelector('input, select, .tags__input')?.focus();
        toast('Заполните обязательные поля', 'warning');
        return;
    }

    state.saving = true;
    state.waitingText = '';
    render();

    // Плагин может уйти в сеть на раздолгое время. Спрашивать нечего: он сам
    // сообщает через событие, что занят и чем именно — так кнопка гаснет и
    // оживает без всякого опроса.
    api(`${API}/${plugin.id}/options`, {
        method: 'PUT',
        body: JSON.stringify({values: state.values}),
    }).then((view) => {
        replacePlugin(view);
        state.fieldError = null;
        clearFieldErrors(plugin);
        toast('Настройки сохранены', 'success');
    }).catch((error) => {
        // Плагин сказал, что не так: помечаем сами поля и конкретные записи в
        // них. Всплывашку не показываем — она бы только отвлекла от сути.
        if (error.fields) {
            state.fieldError = {fields: error.fields, items: error.items};
            showFieldErrors(plugin, error.fields, error.items);
            return;
        }
        toast(error.message, 'error');
    }).finally(() => {
        state.saving = false;
        state.waitingText = '';
        render();
    });
}

// showFieldErrors помечает поля, которые плагин не принял. Если плагин сказал,
// какое именно значение не годится, отмечаем только его.
function showFieldErrors(plugin, fields, items) {
    // Сначала конкретные записи: если плагин назвал значение, красным должно
    // быть только оно, а не всё поле целиком.
    const blamed = new Set();

    for (const [key, entries] of Object.entries(items ?? {})) {
        const row = optionsEl.querySelector(`[data-field="${CSS.escape(key)}"]`);
        for (const [value, reason] of Object.entries(entries ?? {})) {
            const entry = findEntryRow(row, value);
            if (!entry) {
                blamed.add(key);
                continue;
            }
            blamed.add(key);
            entry.classList.add('item--invalid');
            // Красная рамка — на самом поле, а не вокруг строки: крестик
            // удаления к делу не относится.
            for (const field of entry.querySelectorAll('.input, .choices__added')) {
                field.classList.add('input--invalid');
            }
            entry.append(el('p', {class: 'item__error'}, [reason]));
        }
    }

    for (const [key, message] of Object.entries(fields ?? {})) {
        const row = optionsEl.querySelector(`[data-field="${CSS.escape(key)}"]`);
        const error = row?.querySelector('.field__error');
        if (error) {
            error.textContent = message;
        }
        row?.classList.add('field--invalid');

        // Виновна не одна запись, а всё поле — обводим его целиком.
        if (!blamed.has(key)) {
            for (const input of row?.querySelectorAll('.input, .choices__row') ?? []) {
                input.classList.add('input--invalid');
            }
        }
    }
}

// findEntryRow находит конкретную запись списка по тому, что в ней написано.
// findEntryRow ищет именно ту запись, о которой плагин сказал. Сравниваем по
// data-value целиком: «https://goodgame.ru/» — кусок «https://goodgame.ru/E.Wolf»,
// и по вхождению одна ошибка встала бы на чужую запись.
function findEntryRow(row, value) {
    if (!row) {
        return null;
    }
    const wanted = String(value).trim();
    return row.querySelector(`[data-value="${CSS.escape(wanted)}"]`);
}

function clearFieldErrors(plugin) {
    for (const option of plugin.options ?? []) {
        const row = optionsEl.querySelector(`[data-field="${CSS.escape(option.key)}"]`);
        row?.querySelector('.field__error')?.replaceChildren();
        row?.classList.remove('field--invalid');
        row?.querySelectorAll('.item--invalid')
            .forEach((entry) => {
                entry.classList.remove('item--invalid');
                entry.querySelector('.item__error')?.remove();
            });
    }
}

// Обновляет список и бейджи, не трогая форму: перерисовка снесла бы фокус и
// позицию курсора в поле ввода.
function changed(plugin, source) {
    if (source) {
        state.fieldError = null;
        // Что-то поправили: теперь черновик нельзя молча подменять на
        // сохранённое, даже если придёт событие или переподключение.
        state.dirty = true;

        const row = source.closest('[data-field]');
        row?.querySelector('.field__error')?.replaceChildren();
        row?.classList.remove('field--invalid');
        row?.querySelectorAll('.item--invalid')
            .forEach((entry) => {
                entry.classList.remove('item--invalid');
                entry.querySelector('.item__error')?.remove();
            });
    }
    renderClients();

    const headerBadge = optionsEl.querySelector('.options__head .status');
    if (headerBadge) {
        headerBadge.replaceWith(statusBadge(statusOf(plugin)));
    }
}

function statusBadge(status) {
    return el('span', {class: 'status status--' + status.kind, title: status.title ?? ''}, [status.text]);
}

function select(id) {
    if (id === state.selectedId || !state.plugins.some((plugin) => plugin.id === id)) {
        return;
    }
    state.selectedId = id;
    location.hash = id;
    resetValues();
    render();
    // Настройки этого плагина мы ещё не видели: в потоке их нет.
    loadValues();
}

window.addEventListener('hashchange', () => {
    const id = pluginIdFromHash();
    if (id && id !== state.selectedId) {
        state.selectedId = state.plugins.some((plugin) => plugin.id === id) ? id : state.selectedId;
        resetValues();
        render();
        loadValues();
    }
});

// selectedPlugin is the one whose options are open: от него тост берёт имя, если
// вызывающий не назвал плагин сам.
function selectedPlugin() {
    return state.plugins.find((plugin) => plugin.id === state.selectedId) ?? null;
}

function pluginIdFromHash() {
    return location.hash.replace(/^#/, '') || null;
}

// --- Вспомогательное -------------------------------------------------------

// toast is a short-lived notice, and it is built from the very same bricks as a
// chat message: автор сверху, полоска, текст снизу. Вид и время жизни задаёт
// вызывающий — «сохранилось» это не то же самое, что «не вышло».
//
// Тостов может быть сколько угодно: нажали «Сохранить» пять раз — показаны все
// пять, и у каждого своё время. Раньше элемент был один, и новый просто
// затирал предыдущий, как будто человек нажал один раз.
//
// По умолчанию пять секунд: дольше — уже мешает, короче — не успевают прочесть.
function toast(text, severity = 'info', seconds = 5, author = null) {
    const who = author ?? selectedPlugin()?.title ?? selectedPlugin()?.id ?? 'Студия';

    const node = el('article', {
        class: 'msg toast',
        'data-severity': severity,
        role: 'status',
    }, [
        // Автор в квадратных скобках, как служебное сообщение в ленте: не спутать
        // с тем, кто пишет в чате.
        el('div', {class: 'msg__head'}, [
            el('span', {class: 'msg__author msg__author--plugin'}, [`[${who}]`]),
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

// sentence starts a message with a capital — including the part after a colon.
// Сообщение читается как фраза: «GoodGame: настройки сохранены» выглядит так,
// будто слова продолжали другую строку.
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

// Плагины приходят по websocket, а не ответом на запрос, поэтому недоступность
// ядра видна иначе: соединение не открылось и ничего не пришло. Показать это
// нужно один раз — дальше listenStates переподключается сам, и страница
// оживёт, как только ядро поднимется.
let shownOffline = false;

function showOffline() {
    if (shownOffline || state.plugins.length) {
        return;
    }
    shownOffline = true;

    countEl.textContent = '0';
    clientsEl.replaceChildren(el('li', {}, [
        el('p', {class: 'empty'}, ['Ядро недоступно']),
    ]));
    optionsEl.replaceChildren(el('p', {class: 'empty'}, ['Запусти ядро: go run .']));
}

listenStates();