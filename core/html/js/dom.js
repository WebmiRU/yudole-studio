// el — сборка узла. Общая на всех страницах.
//
// Пока её копировали в каждой странице, одна и та же правка (скажем, value=
// как false не ставить) три раза разъезжалась по-разному. Здесь — одна копия.

export function el(tag, attrs = {}, children = []) {
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