package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"studio/core/settings"
)

// newRoomManager поднимает менеджер на временном каталоге плагинов и конфиге.
// Плагины не нужны: проверяется только работа с комнатами.
func newRoomManager(t *testing.T) (*Manager, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	manager, err := NewManager(path, filepath.Join(dir, "plugins"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Shutdown)
	return manager, path
}

func roomByID(t *testing.T, rooms []settings.Room, id string) settings.Room {
	t.Helper()
	for _, room := range rooms {
		if room.ID == id {
			return room
		}
	}
	t.Fatalf("room %q not found in %v", id, rooms)
	return settings.Room{}
}

func TestListRoomsStartsEmpty(t *testing.T) {
	manager, _ := newRoomManager(t)

	if got := manager.ListRooms(); len(got) != 0 {
		t.Errorf("ListRooms: %v, want empty", got)
	}
}

func TestCreateRoomNamesItself(t *testing.T) {
	manager, path := newRoomManager(t)

	room, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if room.Name != "Комната 1" {
		t.Errorf("Name: %q, want %q", room.Name, "Комната 1")
	}

	// Новая комната обязана оказаться в файле, а не только в памяти.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config, err := settings.Parse(data, path)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(config.Rooms) != 1 || config.Rooms[0].Name != "Комната 1" {
		t.Errorf("rooms in file: %v", config.Rooms)
	}
}

// Номер выбирается свободный: удалил «Комната 2» из трёх — новая займёт именно
// его, а не пойдёт дальше. Переиспользование освободившегося номера не путает:
// удалённой комнаты больше нет, и одинаковых имён не получится.
func TestCreateRoomReusesFreedNumber(t *testing.T) {
	manager, _ := newRoomManager(t)

	var ids []string
	for i := 0; i < 3; i++ {
		room, err := manager.CreateRoom()
		if err != nil {
			t.Fatalf("CreateRoom: %v", err)
		}
		ids = append(ids, room.ID)
	}
	if want := []string{"1", "2", "3"}; !slices.Equal(ids, want) {
		t.Fatalf("ids: %v, want %v", ids, want)
	}

	if err := manager.DeleteRoom("2"); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}

	next, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom after delete: %v", err)
	}
	if next.ID != "2" {
		t.Errorf("id: %q, want the freed 2", next.ID)
	}
	if next.Name != "Комната 2" {
		t.Errorf("Name: %q, want %q", next.Name, "Комната 2")
	}
}

// Занятый номер не выдаётся: если в конфиге «2» уже есть, новая — «3».
func TestCreateRoomSkipsTakenNumbers(t *testing.T) {
	manager, _ := newRoomManager(t)

	if _, err := manager.SaveRoom(settings.Room{ID: "1", Name: "Первая"}); err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}

	room, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if room.ID != "2" {
		t.Errorf("id: %q, want 2", room.ID)
	}
}

func TestSaveRoomReplacesByID(t *testing.T) {
	manager, _ := newRoomManager(t)

	room, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	room.Title = "Новый заголовок"
	room.Font = "Cantarell"
	room.MessageSize = 20
	room.AuthorSize = 18
	if _, err := manager.SaveRoom(room); err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}

	rooms := manager.ListRooms()
	if len(rooms) != 1 {
		t.Fatalf("rooms: %v, want one", rooms)
	}
	if got := roomByID(t, rooms, room.ID); got.Title != "Новый заголовок" || got.Font != "Cantarell" {
		t.Errorf("saved room: %+v", got)
	}
}

// Пустое имя из формы не должно стирать имя комнаты: имя живёт в конфиге и
// меняется только переименованием, отдельного редактора пока нет.
func TestSaveRoomKeepsNameWhenEmpty(t *testing.T) {
	manager, _ := newRoomManager(t)

	room, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	room.Name = ""
	if _, err := manager.SaveRoom(room); err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}

	if got := roomByID(t, manager.ListRooms(), room.ID); got.Name != "Комната 1" {
		t.Errorf("Name: %q, want it kept", got.Name)
	}
}

// Размеры приходят из формы, а форма — от человека: ноль или мусор не должны
// ни ломать разметку, ни падать.
func TestSaveRoomClampsSizes(t *testing.T) {
	manager, _ := newRoomManager(t)

	room := settings.Room{ID: "9", Name: "Комната 9", MessageSize: 0, AuthorSize: 5000}
	saved, err := manager.SaveRoom(room)
	if err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}

	if saved.MessageSize != settings.MinFontSize {
		t.Errorf("MessageSize: %d, want %d", saved.MessageSize, settings.MinFontSize)
	}
	if saved.AuthorSize != settings.MaxFontSize {
		t.Errorf("AuthorSize: %d, want %d", saved.AuthorSize, settings.MaxFontSize)
	}
}

// Комната с несуществующим id — это опечатка в адресе, а не повод создать новую.
func TestSaveRoomRejectsMissingID(t *testing.T) {
	manager, _ := newRoomManager(t)

	if _, err := manager.SaveRoom(settings.Room{Name: "Без id"}); err == nil {
		t.Error("SaveRoom without id: nil error, want an error")
	}
	if got := manager.ListRooms(); len(got) != 0 {
		t.Errorf("rooms after a rejected save: %v, want empty", got)
	}
}

func TestDeleteRoomRemovesIt(t *testing.T) {
	manager, path := newRoomManager(t)

	room, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if err := manager.DeleteRoom(room.ID); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}

	if got := manager.ListRooms(); len(got) != 0 {
		t.Errorf("rooms after delete: %v, want empty", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config, err := settings.Parse(data, path)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(config.Rooms) != 0 {
		t.Errorf("rooms in file after delete: %v", config.Rooms)
	}
}

func TestDeleteUnknownRoomFails(t *testing.T) {
	manager, _ := newRoomManager(t)

	if err := manager.DeleteRoom("999"); err == nil {
		t.Error("DeleteRoom(999): nil error, want an error")
	}
}

func TestRoomsSurviveReload(t *testing.T) {
	manager, path := newRoomManager(t)

	room, err := manager.CreateRoom()
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	room.Theme = "starcraft"
	if _, err := manager.SaveRoom(room); err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}

	// Новый менеджер на том же файле — как перезапуск студии.
	restarted, err := NewManager(path, filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(restarted.Shutdown)

	rooms := restarted.ListRooms()
	if len(rooms) != 1 {
		t.Fatalf("rooms after restart: %v, want one", rooms)
	}
	if rooms[0].Theme != "starcraft" {
		t.Errorf("Theme after restart: %q, want starcraft", rooms[0].Theme)
	}
}
