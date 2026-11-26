package messages

import (
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestDownloadFileNameSanitizesPathTraversal(t *testing.T) {
	msg := Message{
		Id:       "msg-1",
		FileName: "../../.ssh/authorized_keys",
	}

	got := downloadFileName(msg)
	if got != "authorized_keys" {
		t.Fatalf("expected sanitized basename, got %q", got)
	}
}

func TestDownloadFileNameSanitizesWindowsPathTraversal(t *testing.T) {
	msg := Message{
		Id:       "msg-2",
		FileName: `..\..\AppData\Roaming\startup.bat`,
	}

	got := downloadFileName(msg)
	if got != "startup.bat" {
		t.Fatalf("expected sanitized basename, got %q", got)
	}
}

func TestDownloadFileNameFallsBackForInvalidName(t *testing.T) {
	msg := Message{
		Id:       "msg-3",
		FileName: "..",
		MimeType: "image/png",
	}

	got := downloadFileName(msg)
	if got != "msg-3.png" {
		t.Fatalf("expected fallback filename, got %q", got)
	}
}

func TestIncomingLIDMessageMergesIntoSavedPNChat(t *testing.T) {
	sm := &SessionManager{}
	sm.Init(nil)
	sm.lidResolver = func(lid types.JID) (types.JID, error) {
		if lid.User == "176377294803067" {
			return types.NewJID("919130918271", types.DefaultUserServer), nil
		}
		return types.EmptyJID, nil
	}

	pnJID := "919130918271@s.whatsapp.net"
	sm.db.AddContact(Contact{Id: pnJID, Name: "Anurag TAABI", Short: "Anurag TAABI"})
	sm.db.AddChat(Chat{Id: pnJID, Name: "Anurag TAABI"})
	sm.db.AddMessage(Message{
		Id:        "out-1",
		ChatId:    pnJID,
		FromMe:    true,
		Text:      "hi",
		Timestamp: 100,
	}, false)

	lidJID := types.NewJID("176377294803067", types.HiddenUserServer)
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   lidJID,
				Sender: lidJID,
			},
			ID:        "in-1",
			PushName:  "Anurag",
			Timestamp: time.Unix(200, 0),
		},
		Message: &waProto.Message{
			Conversation: proto.String("hey"),
		},
	}

	msg, action, ok := sm.eventHandler.normalizeEventMessage(evt)
	if !ok || action != "" {
		t.Fatalf("expected normalized message, got ok=%v action=%q", ok, action)
	}
	sm.db.AddMessage(msg, false)

	if msg.ChatId != pnJID {
		t.Fatalf("expected ChatId %q, got %q", pnJID, msg.ChatId)
	}
	if msg.SenderId != pnJID {
		t.Fatalf("expected SenderId %q, got %q", pnJID, msg.SenderId)
	}
	if msg.ContactName != "Anurag TAABI" || msg.ContactShort != "Anurag TAABI" {
		t.Fatalf("expected saved contact name 'Anurag TAABI', got name=%q short=%q", msg.ContactName, msg.ContactShort)
	}
	if gotChats := sm.db.GetChatIds(); len(gotChats) != 1 {
		t.Fatalf("expected 1 merged chat, got %d (%+v)", len(gotChats), gotChats)
	}
	if gotMsgs := sm.db.GetMessages(pnJID); len(gotMsgs) != 2 {
		t.Fatalf("expected 2 messages in %s, got %d", pnJID, len(gotMsgs))
	}
}

func TestIncomingLIDMessageUsesSenderAltWhenAvailable(t *testing.T) {
	sm := &SessionManager{}
	sm.Init(nil)

	pnJID := types.NewJID("919130918271", types.DefaultUserServer)
	lidJID := types.NewJID("176377294803067", types.HiddenUserServer)
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:      lidJID,
				Sender:    lidJID,
				SenderAlt: pnJID,
			},
			ID:        "in-2",
			Timestamp: time.Unix(300, 0),
		},
		Message: &waProto.Message{
			Conversation: proto.String("yo"),
		},
	}

	msg, _, ok := sm.eventHandler.normalizeEventMessage(evt)
	if !ok {
		t.Fatalf("expected normalized message")
	}
	if msg.ChatId != pnJID.String() || msg.SenderId != pnJID.String() {
		t.Fatalf("expected %s, got chat=%q sender=%q", pnJID.String(), msg.ChatId, msg.SenderId)
	}
}
