package messages

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestAddMessageAndMarkChatRead(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	first := Message{
		Id:           "msg-1",
		ChatId:       "123@s.whatsapp.net",
		ContactId:    "123@s.whatsapp.net",
		ContactName:  "Alice",
		ContactShort: "Alice",
		Timestamp:    100,
		Text:         "hello",
		Kind:         MessageKindText,
	}
	second := Message{
		Id:           "msg-2",
		ChatId:       "123@s.whatsapp.net",
		ContactId:    "123@s.whatsapp.net",
		ContactName:  "Alice",
		ContactShort: "Alice",
		Timestamp:    101,
		Text:         "[IMAGE]",
		Kind:         MessageKindImage,
	}

	if !db.AddMessage(first, false) {
		t.Fatal("expected first message to be new")
	}
	if !db.AddMessage(second, true) {
		t.Fatal("expected second message to be new")
	}

	msgs := db.GetMessages(first.ChatId)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Id != "msg-1" || msgs[1].Id != "msg-2" {
		t.Fatalf("messages not sorted by timestamp: %#v", msgs)
	}

	chats := db.GetChatIds()
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if chats[0].Unread != 1 {
		t.Fatalf("expected unread count 1, got %d", chats[0].Unread)
	}

	cleared := db.MarkChatRead(first.ChatId)
	if len(cleared) != 1 || cleared[0].Id != "msg-2" {
		t.Fatalf("expected msg-2 to be cleared, got %#v", cleared)
	}

	chats = db.GetChatIds()
	if chats[0].Unread != 0 {
		t.Fatalf("expected unread count 0 after mark read, got %d", chats[0].Unread)
	}
}

func TestUpdateChatUnreadMarksLatestIncomingMessages(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	for idx := 0; idx < 4; idx++ {
		db.AddMessage(Message{
			Id:           string(rune('a' + idx)),
			ChatId:       "group@g.us",
			SenderId:     "user@s.whatsapp.net",
			ContactId:    "user@s.whatsapp.net",
			ContactName:  "User",
			ContactShort: "User",
			Timestamp:    uint64(idx + 1),
			FromMe:       idx == 0,
			Text:         "payload",
			Kind:         MessageKindText,
		}, false)
	}

	db.UpdateChatUnread("group@g.us", 2)

	msgs := db.GetMessages("group@g.us")
	unread := 0
	for _, msg := range msgs {
		if msg.Unread {
			unread++
		}
	}
	if unread != 2 {
		t.Fatalf("expected 2 unread messages, got %d", unread)
	}
}

func TestSQLitePersistenceSurvivesReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")

	db1 := &MessageDatabase{}
	if err := db1.OpenSQLite(dbPath); err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}

	db1.AddContact(Contact{Id: "111@s.whatsapp.net", Name: "Alice", Short: "Alice"})
	db1.AddChat(Chat{Id: "222@s.whatsapp.net", Name: "Bob", LastMessage: 50})
	db1.AddMessage(Message{
		Id:           "m1",
		ChatId:       "111@s.whatsapp.net",
		SenderId:     "111@s.whatsapp.net",
		ContactId:    "111@s.whatsapp.net",
		ContactName:  "Alice",
		ContactShort: "Alice",
		Timestamp:    200,
		Text:         "replying",
		Kind:         MessageKindText,
		QuotedId:     "m0",
		QuotedSender: "Me",
		QuotedText:   "original",
		RawMessage:   &waProto.Message{Conversation: proto.String("replying")},
	}, true)
	if err := db1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	db2 := &MessageDatabase{}
	if err := db2.OpenSQLite(dbPath); err != nil {
		t.Fatalf("reopen OpenSQLite failed: %v", err)
	}
	defer db2.Close()

	chats := db2.GetChatIds()
	if len(chats) != 2 || chats[0].Id != "111@s.whatsapp.net" {
		t.Fatalf("expected Alice sorted first by LastMessage, got %+v", chats)
	}
	msgs := db2.GetMessages("111@s.whatsapp.net")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(msgs))
	}
	if msgs[0].QuotedId != "m0" || msgs[0].QuotedSender != "Me" || msgs[0].QuotedText != "original" {
		t.Fatalf("expected quoted fields persisted, got %+v", msgs[0])
	}
	if msgs[0].RawMessage == nil || msgs[0].RawMessage.GetConversation() != "replying" {
		t.Fatalf("expected RawMessage persisted, got %+v", msgs[0].RawMessage)
	}
	if oldest, ok := db2.GetOldestMessage("111@s.whatsapp.net"); !ok || oldest.Id != "m1" {
		t.Fatalf("expected oldest anchor m1 after reopen, got ok=%v oldest=%+v", ok, oldest)
	}
}

func TestChatSettingsArchivePinMuteAndSort(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chats.db")
	db := &MessageDatabase{}
	if err := db.OpenSQLite(dbPath); err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}

	db.AddChat(Chat{Id: "a@s.whatsapp.net", Name: "Older Unpinned", LastMessage: 10})
	db.AddChat(Chat{Id: "b@s.whatsapp.net", Name: "Newer Unpinned", LastMessage: 500})
	db.AddChat(Chat{Id: "c@s.whatsapp.net", Name: "Pinned Older", LastMessage: 20})

	pinTrue := true
	archTrue := true
	mutedForever := int64(-1)
	db.SetChatSettings("c@s.whatsapp.net", nil, &pinTrue, nil)
	db.SetChatSettings("a@s.whatsapp.net", &archTrue, nil, &mutedForever)

	chats := db.GetChatIds()
	if len(chats) != 3 || chats[0].Id != "c@s.whatsapp.net" {
		t.Fatalf("expected pinned chat first, got %+v", chats)
	}
	if !db.IsChatMuted("a@s.whatsapp.net") {
		t.Fatalf("expected archived chat to be muted")
	}

	// Archiving a pinned chat should clear Pinned.
	db.SetChatSettings("c@s.whatsapp.net", &archTrue, nil, nil)
	_ = db.Close()

	db2 := &MessageDatabase{}
	if err := db2.OpenSQLite(dbPath); err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer db2.Close()

	for _, ch := range db2.GetChatIds() {
		if ch.Id == "c@s.whatsapp.net" && (ch.Pinned || !ch.Archived) {
			t.Fatalf("expected c to be archived and unpinned after reopen, got %+v", ch)
		}
		if ch.Id == "a@s.whatsapp.net" && (!ch.Archived || !ch.IsMuted()) {
			t.Fatalf("expected a to be archived and muted after reopen, got %+v", ch)
		}
	}
}
