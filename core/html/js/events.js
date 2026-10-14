// Реестр событий чата: что вообще может прийти и как это называть человеку.
//
// Событие в конфиге хранится тем же kind'ом, что приходит по протоколу, —
// без перевода и сокращений: chat.message.public в config.json должен
// совпадать с chat.message.public в потоке, иначе не разберёшься, что в ленте
// включено. Лейбл нужен только для показа.

// EVENT_GROUPS — все события, разложенные по смыслу. Порядок групп и событий
// внутри них — это порядок показа в настройках комнаты.
//
// Список закрыт: он выведен из protocol.Kind* в ядре. Новое событие, о котором
// здесь не написано, в комнате не включится и в ленте не покажется — это
// заметнее, чем молча пропавший флажок.
export const EVENT_GROUPS = [
    {
        id: 'chat',
        title: 'Чат',
        events: [
            {kind: 'chat.message.public', label: 'Публичное сообщение'},
            {kind: 'chat.message.private', label: 'Личное сообщение'},
            {kind: 'chat.message.deleted', label: 'Сообщение удалено модератором'},
            {kind: 'system.message.warning', label: 'Системное предупреждение'},
        ],
    },
    {
        id: 'channels',
        title: 'Каналы',
        events: [
            {kind: 'chat.channel.join.success', label: 'Подключение к каналу'},
            {kind: 'chat.channel.join.failed', label: 'Не удалось подключиться к каналу'},
            {kind: 'chat.channel.unjoin.success', label: 'Отключение от канала'},
            {kind: 'chat.channel.unjoin.failed', label: 'Не удалось отключиться от канала'},
            {kind: 'chat.channel.info', label: 'Сведения о канале'},
        ],
    },
    {
        id: 'network',
        title: 'Сеть',
        events: [
            {kind: 'chat.connection.state', label: 'Состояние соединения'},
        ],
    },
];

// LABELS — плоский вид того же списка: kind в лейбл. Собран из групп, чтобы
// второго места, где живут названия, не было.
export const LABELS = new Map(
    EVENT_GROUPS.flatMap((group) => group.events.map((e) => [e.kind, e.label])),
);

// ALL_KINDS — все события одной строкой.
export const ALL_KINDS = [...LABELS.keys()];

// labelOf возвращает название события для человека. Неизвестный kind не
// выкидывается и не подменяется пустым: событие пришло из плагина, которого
// эта страница не знает, и показать его лучше как есть, чем спрятать.
export function labelOf(kind) {
    return LABELS.get(kind) ?? kind;
}
