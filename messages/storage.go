package messages

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

// MessageDatabase stores messages and contact data.
type MessageDatabase struct {
	messages     map[string][]Message
	messagesById map[string]Message
	chats        map[string]Chat
	contacts     map[string]Contact
	sqlDB        *sql.DB

	contactLock sync.RWMutex
	chatLock    sync.RWMutex
	messageLock sync.RWMutex
}

// Init initializes the message database.
func (md *MessageDatabase) Init() {
	md.messages = make(map[string][]Message)
	md.messagesById = make(map[string]Message)
	md.chats = make(map[string]Chat)
	md.contacts = make(map[string]Contact)
}

// OpenSQLite opens a persistent SQLite database and hydrates the in-memory store.
func (md *MessageDatabase) OpenSQLite(dbPath string) error {
	if md.messages == nil {
		md.Init()
	}
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS chats (
		id TEXT PRIMARY KEY,
		is_group INTEGER NOT NULL,
		name TEXT NOT NULL,
		unread INTEGER NOT NULL,
		last_message INTEGER NOT NULL,
		archived INTEGER NOT NULL DEFAULT 0,
		pinned INTEGER NOT NULL DEFAULT 0,
		muted_until INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS contacts (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		short TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		sender_id TEXT NOT NULL,
		contact_id TEXT NOT NULL,
		contact_name TEXT NOT NULL,
		contact_short TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		from_me INTEGER NOT NULL,
		forwarded INTEGER NOT NULL,
		text TEXT NOT NULL,
		kind TEXT NOT NULL,
		mime_type TEXT NOT NULL,
		file_name TEXT NOT NULL,
		unread INTEGER NOT NULL,
		quoted_id TEXT NOT NULL DEFAULT '',
		quoted_sender TEXT NOT NULL DEFAULT '',
		quoted_text TEXT NOT NULL DEFAULT '',
		raw_proto BLOB
	);
	CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages(chat_id, timestamp);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return err
	}
	_, _ = db.Exec("ALTER TABLE chats ADD COLUMN archived INTEGER NOT NULL DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE chats ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE chats ADD COLUMN muted_until INTEGER NOT NULL DEFAULT 0")
	md.sqlDB = db
	return md.loadFromSQLite()
}

func (md *MessageDatabase) loadFromSQLite() error {
	if md.sqlDB == nil {
		return nil
	}

	cRows, err := md.sqlDB.Query("SELECT id, name, short FROM contacts")
	if err != nil {
		return err
	}
	defer cRows.Close()
	md.contactLock.Lock()
	for cRows.Next() {
		var c Contact
		if err := cRows.Scan(&c.Id, &c.Name, &c.Short); err == nil {
			md.contacts[c.Id] = c
		}
	}
	md.contactLock.Unlock()

	chRows, err := md.sqlDB.Query("SELECT id, is_group, name, unread, last_message, archived, pinned, muted_until FROM chats")
	if err != nil {
		return err
	}
	defer chRows.Close()
	md.chatLock.Lock()
	for chRows.Next() {
		var ch Chat
		var isGroup, archived, pinned int
		if err := chRows.Scan(&ch.Id, &isGroup, &ch.Name, &ch.Unread, &ch.LastMessage, &archived, &pinned, &ch.MutedUntil); err == nil {
			ch.IsGroup = isGroup != 0
			ch.Archived = archived != 0
			ch.Pinned = pinned != 0
			md.chats[ch.Id] = ch
		}
	}
	md.chatLock.Unlock()

	mRows, err := md.sqlDB.Query(`SELECT id, chat_id, sender_id, contact_id, contact_name, contact_short,
		timestamp, from_me, forwarded, text, kind, mime_type, file_name, unread,
		quoted_id, quoted_sender, quoted_text, raw_proto
		FROM messages ORDER BY timestamp ASC, id ASC`)
	if err != nil {
		return err
	}
	defer mRows.Close()
	md.messageLock.Lock()
	for mRows.Next() {
		var msg Message
		var fromMe, forwarded, unread int
		var kindStr string
		var rawBytes []byte
		if err := mRows.Scan(
			&msg.Id, &msg.ChatId, &msg.SenderId, &msg.ContactId, &msg.ContactName, &msg.ContactShort,
			&msg.Timestamp, &fromMe, &forwarded, &msg.Text, &kindStr, &msg.MimeType, &msg.FileName, &unread,
			&msg.QuotedId, &msg.QuotedSender, &msg.QuotedText, &rawBytes,
		); err == nil {
			msg.FromMe = fromMe != 0
			msg.Forwarded = forwarded != 0
			msg.Unread = unread != 0
			msg.Kind = MessageKind(kindStr)
			if len(rawBytes) > 0 {
				raw := &waProto.Message{}
				if uErr := proto.Unmarshal(rawBytes, raw); uErr == nil {
					msg.RawMessage = raw
				}
			}
			md.messagesById[msg.Id] = msg
			md.messages[msg.ChatId] = append(md.messages[msg.ChatId], msg)
		}
	}
	md.messageLock.Unlock()
	return nil
}

// Close closes the underlying SQLite database if open.
func (md *MessageDatabase) Close() error {
	if md.sqlDB != nil {
		err := md.sqlDB.Close()
		md.sqlDB = nil
		return err
	}
	return nil
}

// ClearAll removes all stored messages, chats, and contacts from memory and SQLite.
func (md *MessageDatabase) ClearAll() {
	md.messageLock.Lock()
	md.chatLock.Lock()
	md.contactLock.Lock()
	md.messages = make(map[string][]Message)
	md.messagesById = make(map[string]Message)
	md.chats = make(map[string]Chat)
	md.contacts = make(map[string]Contact)
	if md.sqlDB != nil {
		_, _ = md.sqlDB.Exec("DELETE FROM messages; DELETE FROM chats; DELETE FROM contacts;")
	}
	md.contactLock.Unlock()
	md.chatLock.Unlock()
	md.messageLock.Unlock()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (md *MessageDatabase) persistMessage(msg Message) {
	if md.sqlDB == nil {
		return
	}
	var rawBytes []byte
	if msg.RawMessage != nil {
		if b, err := proto.Marshal(msg.RawMessage); err == nil {
			rawBytes = b
		}
	}
	_, _ = md.sqlDB.Exec(`INSERT OR REPLACE INTO messages
		(id, chat_id, sender_id, contact_id, contact_name, contact_short, timestamp, from_me, forwarded,
		 text, kind, mime_type, file_name, unread, quoted_id, quoted_sender, quoted_text, raw_proto)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.Id, msg.ChatId, msg.SenderId, msg.ContactId, msg.ContactName, msg.ContactShort,
		int64(msg.Timestamp), boolToInt(msg.FromMe), boolToInt(msg.Forwarded),
		msg.Text, string(msg.Kind), msg.MimeType, msg.FileName, boolToInt(msg.Unread),
		msg.QuotedId, msg.QuotedSender, msg.QuotedText, rawBytes,
	)
}

func (md *MessageDatabase) persistChat(chat Chat) {
	if md.sqlDB == nil {
		return
	}
	_, _ = md.sqlDB.Exec(`INSERT OR REPLACE INTO chats (id, is_group, name, unread, last_message, archived, pinned, muted_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		chat.Id, boolToInt(chat.IsGroup), chat.Name, chat.Unread, chat.LastMessage, boolToInt(chat.Archived), boolToInt(chat.Pinned), chat.MutedUntil)
}

// SetChatSettings updates archive/pin/mute flags for a chat and persists them.
func (md *MessageDatabase) SetChatSettings(chatID string, archived, pinned *bool, mutedUntil *int64) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()

	chat, ok := md.chats[chatID]
	if !ok {
		return
	}
	if archived != nil {
		chat.Archived = *archived
		if *archived {
			chat.Pinned = false
		}
	}
	if pinned != nil {
		chat.Pinned = *pinned
	}
	if mutedUntil != nil {
		chat.MutedUntil = *mutedUntil
	}
	md.chats[chatID] = chat
	md.persistChat(chat)
}

// IsChatMuted reports whether the given chat is currently muted.
func (md *MessageDatabase) IsChatMuted(chatID string) bool {
	md.chatLock.RLock()
	defer md.chatLock.RUnlock()
	if chat, ok := md.chats[chatID]; ok {
		return chat.IsMuted()
	}
	return false
}

func (md *MessageDatabase) persistContact(contact Contact) {
	if md.sqlDB == nil {
		return
	}
	_, _ = md.sqlDB.Exec(`INSERT OR REPLACE INTO contacts (id, name, short) VALUES (?, ?, ?)`,
		contact.Id, contact.Name, contact.Short)
}

// GetChatUnread returns the current unread count for a chat.
func (md *MessageDatabase) GetChatUnread(chatID string) int {
	md.chatLock.RLock()
	defer md.chatLock.RUnlock()
	if chat, ok := md.chats[chatID]; ok {
		return chat.Unread
	}
	return 0
}

// AddMessage stores a message and updates related chat state.
func (md *MessageDatabase) AddMessage(msg Message, markUnread bool) bool {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	if existing, ok := md.messagesById[msg.Id]; ok {
		// Keep the first version, but upgrade metadata if the newer message has richer data.
		if existing.RawMessage == nil && msg.RawMessage != nil {
			existing.RawMessage = msg.RawMessage
		}
		if existing.Kind == MessageKindUnknown && msg.Kind != MessageKindUnknown {
			existing.Kind = msg.Kind
		}
		if existing.Text == "" && msg.Text != "" {
			existing.Text = msg.Text
		}
		if existing.FileName == "" && msg.FileName != "" {
			existing.FileName = msg.FileName
		}
		if existing.MimeType == "" && msg.MimeType != "" {
			existing.MimeType = msg.MimeType
		}
		existing.Unread = existing.Unread || markUnread
		md.messagesById[msg.Id] = existing
		md.replaceMessageLocked(existing)
		md.persistMessage(existing)
		md.updateChatFromMessageLocked(existing, markUnread)
		return false
	}

	msg.Unread = markUnread
	md.messagesById[msg.Id] = msg
	md.messages[msg.ChatId] = append(md.messages[msg.ChatId], msg)
	md.persistMessage(msg)
	md.updateChatFromMessageLocked(msg, markUnread)
	return true
}

func (md *MessageDatabase) replaceMessageLocked(msg Message) {
	msgs := md.messages[msg.ChatId]
	for idx, current := range msgs {
		if current.Id == msg.Id {
			msgs[idx] = msg
			md.messages[msg.ChatId] = msgs
			return
		}
	}
}

func (md *MessageDatabase) updateChatFromMessageLocked(msg Message, markUnread bool) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()

	chat, exists := md.chats[msg.ChatId]
	if !exists {
		chat = Chat{
			Id:      msg.ChatId,
			IsGroup: strings.Contains(msg.ChatId, GROUPSUFFIX),
			Name:    msg.ContactName,
		}
	}
	if chat.Name == "" {
		chat.Name = msg.ContactName
	}
	if int64(msg.Timestamp) > chat.LastMessage {
		chat.LastMessage = int64(msg.Timestamp)
	}
	if markUnread {
		chat.Unread++
	}
	md.chats[msg.ChatId] = chat
	md.persistChat(chat)

	if msg.ContactId != "" {
		md.contactLock.Lock()
		if _, ok := md.contacts[msg.ContactId]; !ok {
			c := Contact{
				Id:    msg.ContactId,
				Name:  msg.ContactName,
				Short: msg.ContactShort,
			}
			md.contacts[msg.ContactId] = c
			md.persistContact(c)
		}
		md.contactLock.Unlock()
	}
}

// AddChat adds or updates a chat in the database.
func (md *MessageDatabase) AddChat(chat Chat) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()

	existing, ok := md.chats[chat.Id]
	if ok {
		if chat.Name == "" {
			chat.Name = existing.Name
		}
		if chat.LastMessage < existing.LastMessage {
			chat.LastMessage = existing.LastMessage
		}
		if chat.Unread < existing.Unread {
			chat.Unread = existing.Unread
		}
		chat.Archived = existing.Archived || chat.Archived
		chat.Pinned = existing.Pinned || chat.Pinned
		if chat.MutedUntil == 0 {
			chat.MutedUntil = existing.MutedUntil
		}
	}
	md.chats[chat.Id] = chat
	md.persistChat(chat)
}

// UpdateChatUnread syncs unread counts from external sources such as history sync.
func (md *MessageDatabase) UpdateChatUnread(chatID string, unread int) {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	ids := md.lastIncomingMessageIDsLocked(chatID, unread)
	unreadSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		unreadSet[id] = struct{}{}
	}

	msgs := md.messages[chatID]
	for idx, msg := range msgs {
		_, ok := unreadSet[msg.Id]
		msg.Unread = ok
		msgs[idx] = msg
		if stored, found := md.messagesById[msg.Id]; found {
			stored.Unread = ok
			md.messagesById[msg.Id] = stored
			md.persistMessage(stored)
		}
	}
	md.messages[chatID] = msgs

	md.chatLock.Lock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Unread = len(ids)
		md.chats[chatID] = chat
		md.persistChat(chat)
	}
	md.chatLock.Unlock()
}

func (md *MessageDatabase) lastIncomingMessageIDsLocked(chatID string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	msgs := md.messages[chatID]
	ids := make([]string, 0, limit)
	for idx := len(msgs) - 1; idx >= 0 && len(ids) < limit; idx-- {
		if !msgs[idx].FromMe {
			ids = append(ids, msgs[idx].Id)
		}
	}
	return ids
}

// MarkChatRead clears unread state for the given chat and returns the unread messages that were cleared.
func (md *MessageDatabase) MarkChatRead(chatID string) []Message {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	msgs := md.messages[chatID]
	cleared := make([]Message, 0)
	for idx, msg := range msgs {
		if msg.Unread {
			cleared = append(cleared, msg)
			msg.Unread = false
			msgs[idx] = msg
			stored := md.messagesById[msg.Id]
			stored.Unread = false
			md.messagesById[msg.Id] = stored
			md.persistMessage(stored)
		}
	}
	md.messages[chatID] = msgs

	md.chatLock.Lock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Unread = 0
		md.chats[chatID] = chat
		md.persistChat(chat)
	}
	md.chatLock.Unlock()

	return cleared
}

// MarkMessageRevoked updates a message to show that it was revoked.
func (md *MessageDatabase) MarkMessageRevoked(messageID string) bool {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	msg, ok := md.messagesById[messageID]
	if !ok {
		return false
	}
	msg.Text = "[message revoked]"
	msg.RawMessage = nil
	msg.Kind = MessageKindUnknown
	md.messagesById[messageID] = msg
	md.replaceMessageLocked(msg)
	md.persistMessage(msg)
	return true
}

// AddContact adds or updates a contact in the database.
func (md *MessageDatabase) AddContact(contact Contact) {
	md.contactLock.Lock()
	defer md.contactLock.Unlock()

	existing, ok := md.contacts[contact.Id]
	if ok {
		if contact.Name == "" {
			contact.Name = existing.Name
		}
		if contact.Short == "" {
			contact.Short = existing.Short
		}
	}
	md.contacts[contact.Id] = contact
	md.persistContact(contact)
}

// GetChatIds returns chats sorted by most recent message first.
func (md *MessageDatabase) GetChatIds() []Chat {
	md.chatLock.RLock()
	defer md.chatLock.RUnlock()

	allChats := make([]Chat, 0, len(md.chats))
	for _, chat := range md.chats {
		allChats = append(allChats, chat)
	}
	sort.Slice(allChats, func(i, j int) bool {
		if allChats[i].Pinned != allChats[j].Pinned {
			return allChats[i].Pinned
		}
		if allChats[i].LastMessage == allChats[j].LastMessage {
			return allChats[i].Name < allChats[j].Name
		}
		return allChats[i].LastMessage > allChats[j].LastMessage
	})
	return allChats
}

// GetMessages returns all messages for the given chat, sorted by timestamp.
func (md *MessageDatabase) GetMessages(chatID string) []Message {
	md.messageLock.RLock()
	msgs := md.messages[chatID]
	out := make([]Message, len(msgs))
	copy(out, msgs)
	md.messageLock.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Timestamp == out[j].Timestamp {
			return out[i].Id < out[j].Id
		}
		return out[i].Timestamp < out[j].Timestamp
	})
	return out
}

// GetMessage returns a single message by ID.
func (md *MessageDatabase) GetMessage(id string) (Message, bool) {
	md.messageLock.RLock()
	defer md.messageLock.RUnlock()
	msg, ok := md.messagesById[id]
	return msg, ok
}

// GetOldestMessage returns the oldest stored message in a chat.
func (md *MessageDatabase) GetOldestMessage(chatID string) (Message, bool) {
	md.messageLock.RLock()
	defer md.messageLock.RUnlock()
	msgs := md.messages[chatID]
	if len(msgs) == 0 {
		return Message{}, false
	}
	oldest := msgs[0]
	for _, m := range msgs[1:] {
		if m.Timestamp < oldest.Timestamp || (m.Timestamp == oldest.Timestamp && m.Id < oldest.Id) {
			oldest = m
		}
	}
	return oldest, true
}

// GetMessageInfo returns a human-readable description of a message.
func (md *MessageDatabase) GetMessageInfo(id string) string {
	msg, ok := md.GetMessage(id)
	if !ok {
		return "Message not found"
	}

	name := md.GetIdName(msg.ContactId)
	short := md.GetIdShort(msg.ContactId)
	direction := "←"
	if msg.FromMe {
		direction = "→"
	}

	kind := string(msg.Kind)
	if kind == "" {
		kind = string(MessageKindUnknown)
	}

	info := fmt.Sprintf(
		"ID: %s\nType: %s\nFrom: %s (%s) %s\nTime: %s\nChat: %s",
		msg.Id,
		kind,
		name,
		short,
		direction,
		time.Unix(int64(msg.Timestamp), 0).Format(time.RFC1123),
		msg.ChatId,
	)
	if msg.FileName != "" {
		info += "\nFile: " + msg.FileName
	}
	if msg.MimeType != "" {
		info += "\nMIME: " + msg.MimeType
	}
	if msg.SenderId != "" {
		info += "\nSender: " + msg.SenderId
	}
	return info
}

// GetIdName resolves a contact or chat ID to a display name.
func (md *MessageDatabase) GetIdName(id string) string {
	if id == "" {
		return "Unknown"
	}

	md.contactLock.RLock()
	contact, ok := md.contacts[id]
	md.contactLock.RUnlock()
	if ok {
		if contact.Name != "" {
			return contact.Name
		}
		if contact.Short != "" {
			return contact.Short
		}
	}

	md.chatLock.RLock()
	chat, ok := md.chats[id]
	md.chatLock.RUnlock()
	if ok && chat.Name != "" {
		return chat.Name
	}

	return strings.TrimSuffix(strings.TrimSuffix(id, CONTACTSUFFIX), GROUPSUFFIX)
}

// GetIdShort resolves a contact or chat ID to a short display name.
func (md *MessageDatabase) GetIdShort(id string) string {
	if id == "" {
		return "Unknown"
	}

	md.contactLock.RLock()
	contact, ok := md.contacts[id]
	md.contactLock.RUnlock()
	if ok {
		if contact.Short != "" {
			return contact.Short
		}
		if contact.Name != "" {
			return contact.Name
		}
	}

	md.chatLock.RLock()
	chat, ok := md.chats[id]
	md.chatLock.RUnlock()
	if ok && chat.Name != "" {
		return chat.Name
	}

	return strings.TrimSuffix(strings.TrimSuffix(id, CONTACTSUFFIX), GROUPSUFFIX)
}
