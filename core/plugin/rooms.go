package plugin

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"studio/core/settings"
)

// Комнаты живут в том же конфиге, что и плагины, и пишет его Manager: у него уже
// есть путь до файла и атомарная запись. Второй писатель config.json означал бы
// два места, где решают, кто главнее, и потерю правок при одновременной записи.

// ListRooms returns the rooms in the order they stand in the config.
func (m *Manager) ListRooms() []settings.Room {
	m.mu.Lock()
	defer m.mu.Unlock()

	rooms := make([]settings.Room, len(m.config.Rooms))
	copy(rooms, m.config.Rooms)
	return rooms
}

// GetRoom returns one room by id.
func (m *Manager) GetRoom(id string) (settings.Room, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, room := range m.config.Rooms {
		if room.ID == id {
			return room, true
		}
	}
	return settings.Room{}, false
}

// SaveRoom puts a room into the config, replacing the one with the same id, and
// writes the file. Комната с несуществующим id не создаётся: это опечатка в
// адресе, а не намерение добавить комнату.
func (m *Manager) SaveRoom(room settings.Room) (settings.Room, error) {
	if room.ID == "" {
		return settings.Room{}, fmt.Errorf("room without id")
	}
	room.MessageSize = settings.ClampFontSize(room.MessageSize)
	room.AuthorSize = settings.ClampFontSize(room.AuthorSize)

	m.mu.Lock()
	index := -1
	for i, existing := range m.config.Rooms {
		if existing.ID == room.ID {
			index = i
			break
		}
	}
	if index >= 0 {
		m.config.Rooms[index] = room
	} else {
		m.config.Rooms = append(m.config.Rooms, room)
	}
	m.mu.Unlock()

	if err := m.save(); err != nil {
		return settings.Room{}, err
	}
	return room, nil
}

// CreateRoom adds a room with the next free "Комната N" title and writes the file.
func (m *Manager) CreateRoom() (settings.Room, error) {
	m.mu.Lock()
	room := settings.Room{
		ID:          strconv.Itoa(m.nextRoomNumber()),
		Font:        "Noto Sans",
		Theme:       "system",
		MessageSize: settings.DefaultMessageSize,
		AuthorSize:  settings.DefaultAuthorSize,
	}
	room.Title = roomTitle(room.ID)
	room.Events = slices.Clone(settings.DefaultEvents)
	m.config.Rooms = append(m.config.Rooms, room)
	m.mu.Unlock()

	if err := m.save(); err != nil {
		return settings.Room{}, err
	}
	slog.Info("room created", "id", room.ID, "title", room.Title)
	return room, nil
}

// DeleteRoom removes a room and writes the file.
func (m *Manager) DeleteRoom(id string) error {
	m.mu.Lock()
	index := -1
	for i, room := range m.config.Rooms {
		if room.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		m.mu.Unlock()
		return fmt.Errorf("room %q not found", id)
	}
	removed := m.config.Rooms[index]
	m.config.Rooms = append(m.config.Rooms[:index], m.config.Rooms[index+1:]...)
	m.mu.Unlock()

	if err := m.save(); err != nil {
		return err
	}
	slog.Info("room deleted", "id", removed.ID, "title", removed.Title)
	return nil
}

// nextRoomNumber подбирает незанятый номер: id — строка, и «новый» номер может
// совпасть с чужим, если комнаты добавляли и удаляли. Внутри mu.
func (m *Manager) nextRoomNumber() int {
	taken := make(map[string]bool, len(m.config.Rooms))
	for _, room := range m.config.Rooms {
		taken[room.ID] = true
	}
	for n := 1; ; n++ {
		if !taken[strconv.Itoa(n)] {
			return n
		}
	}
}

// roomTitle даёт новой комнате название по её номеру. Внутри mu.
func roomTitle(id string) string {
	return "Комната " + strings.TrimSpace(id)
}
