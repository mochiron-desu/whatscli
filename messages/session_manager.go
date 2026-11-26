package messages

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gen2brain/beeep"
	_ "github.com/mattn/go-sqlite3" // SQLite driver
	"github.com/normen/whatscli/config"
	"github.com/normen/whatscli/qrcode"
	"github.com/rivo/tview"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var urlPattern = regexp.MustCompile(`https?://[^\s]+`)

// SessionManager deals with the connection and receives commands from the UI.
type SessionManager struct {
	db              *MessageDatabase
	currentReceiver string
	uiHandler       UiMessageHandler
	client          *whatsmeow.Client
	container       *sqlstore.Container
	BatteryChannel  chan BatteryMsg
	StatusChannel   chan StatusMsg
	CommandChannel  chan Command
	ChatChannel     chan Chat
	ContactChannel  chan Contact
	TextChannel     chan *waProto.Message
	statusInfo      SessionStatus
	lastSent        time.Time
	started         bool
	eventHandler    *eventHandler
	Logger          waLog.Logger
	PairPhone       string
	lidResolver     func(types.JID) (types.JID, error)
}

// Init initializes the SessionManager.
func (sm *SessionManager) Init(handler UiMessageHandler) {
	sm.db = &MessageDatabase{}
	sm.db.Init()
	sm.uiHandler = handler
	if sm.Logger == nil {
		sm.Logger = waLog.Noop
	}
	sm.BatteryChannel = make(chan BatteryMsg, 10)
	sm.StatusChannel = make(chan StatusMsg, 10)
	sm.CommandChannel = make(chan Command, 10)
	sm.ChatChannel = make(chan Chat, 10)
	sm.ContactChannel = make(chan Contact, 10)
	sm.TextChannel = make(chan *waProto.Message, 10)
	sm.eventHandler = &eventHandler{sm: sm}
}

func (sm *SessionManager) log() waLog.Logger {
	if sm.Logger == nil {
		return waLog.Noop
	}
	return sm.Logger
}

func (sm *SessionManager) resolveCanonicalJID(jid, alt types.JID) types.JID {
	if jid.IsEmpty() {
		return jid
	}
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if sm.lidResolver != nil {
		if pn, err := sm.lidResolver(jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	} else if sm.client != nil && sm.client.Store != nil && sm.client.Store.LIDs != nil {
		if pn, err := sm.client.Store.LIDs.GetPNForLID(context.Background(), jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid
}

func (sm *SessionManager) normalizeJID(jid types.JID) types.JID {
	return sm.resolveCanonicalJID(jid, types.EmptyJID)
}

// StartManager starts the receiver and message handling goroutine.
func (sm *SessionManager) StartManager() error {
	if sm.started {
		return errors.New("session manager running, send commands to control")
	}
	sm.started = true
	go sm.runManager()
	return nil
}

func (sm *SessionManager) runManager() error {
	client, err := sm.getConnection()
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to create WhatsApp connection: %v", err))
		return err
	}
	if client == nil {
		return errors.New("could not establish WhatsApp connection")
	}

	if err = sm.loginWithConnection(client); err != nil {
		sm.uiHandler.PrintError(err)
	}

	for sm.started {
		select {
		case command := <-sm.CommandChannel:
			sm.execCommand(command)
		case batteryMsg := <-sm.BatteryChannel:
			sm.statusInfo.BatteryLoading = batteryMsg.loading
			sm.statusInfo.BatteryPowersave = batteryMsg.powersave
			sm.statusInfo.BatteryCharge = batteryMsg.charge
			sm.uiHandler.SetStatus(sm.statusInfo)
		case statusMsg := <-sm.StatusChannel:
			prevStatus := sm.statusInfo.Connected
			if statusMsg.err == nil {
				sm.statusInfo.Connected = statusMsg.connected
			}
			if sm.client != nil {
				sm.statusInfo.Connected = sm.client.IsConnected()
			} else {
				sm.statusInfo.Connected = false
			}
			sm.uiHandler.SetStatus(sm.statusInfo)
			if prevStatus != sm.statusInfo.Connected {
				if sm.statusInfo.Connected {
					sm.uiHandler.PrintText("connected")
				} else {
					sm.uiHandler.PrintText("disconnected")
				}
			}
		}
	}

	fmt.Fprintln(sm.uiHandler.GetWriter(), "closing the receiver")
	if sm.client != nil {
		sm.client.Disconnect()
	}
	_ = sm.db.Close()
	return nil
}

func (sm *SessionManager) setCurrentReceiver(id string) {
	sm.currentReceiver = id
	msgs := sm.getMessages(id)
	sm.uiHandler.NewScreen(msgs)
	if config.Config.General.AutoMarkRead && id != "" && sm.db.GetChatUnread(id) > 0 {
		sm.markCurrentChatReadInternal(true)
	}
	if config.Config.General.InlineImages && id != "" {
		go sm.prefetchChatStickers(msgs, 5)
	}
}

func (sm *SessionManager) getConnection() (*whatsmeow.Client, error) {
	if sm.db.sqlDB == nil {
		msgDBPath := config.GetSessionFilePath() + ".messages.db"
		if err := sm.db.OpenSQLite(msgDBPath); err != nil {
			sm.log().Warnf("failed to open messages db %s: %v", msgDBPath, err)
		} else if chats := sm.db.GetChatIds(); len(chats) > 0 {
			sm.uiHandler.SetChats(chats)
		}
	}
	if sm.client == nil {
		dbPath := config.GetSessionFilePath() + ".db"
		container, err := sqlstore.New(context.Background(), "sqlite3", "file:"+dbPath+"?_foreign_keys=on", sm.log().Sub("DB"))
		if err != nil {
			return nil, fmt.Errorf("failed to connect to database: %v", err)
		}
		deviceStore, err := container.GetFirstDevice(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to get device: %v", err)
		}
		client := whatsmeow.NewClient(deviceStore, sm.log().Sub("Client"))
		client.AddEventHandler(sm.eventHandler.Handle)
		sm.client = client
		sm.container = container
	}
	return sm.client, nil
}

func (sm *SessionManager) login() error {
	sm.client = nil
	client, err := sm.getConnection()
	if err != nil {
		return fmt.Errorf("failed to create WhatsApp connection: %v", err)
	}
	return sm.loginWithConnection(client)
}

func (sm *SessionManager) loginWithConnection(client *whatsmeow.Client) error {
	sm.uiHandler.PrintText("connecting..")
	if client.IsConnected() {
		client.Disconnect()
		sm.StatusChannel <- StatusMsg{false, nil}
		time.Sleep(500 * time.Millisecond)
	}

	if client.Store.ID == nil {
		if sm.PairPhone != "" {
			return sm.loginWithPairPhone(client, sm.PairPhone)
		}
		return sm.loginWithQRCode(client)
	}

	if err := client.Connect(); err != nil {
		if errors.Is(err, whatsmeow.ErrNotConnected) || errors.Is(err, whatsmeow.ErrNotLoggedIn) {
			sm.uiHandler.PrintText("Session expired, need to scan QR code again")
			if delErr := client.Store.Delete(context.Background()); delErr != nil {
				return fmt.Errorf("failed to clear expired session: %v", delErr)
			}
			sm.client = nil
			client, err = sm.getConnection()
			if err != nil {
				return fmt.Errorf("failed to create new connection: %v", err)
			}
			return sm.loginWithQRCode(client)
		}
		return fmt.Errorf("connection failed: %v", err)
	}

	sm.uiHandler.PrintText("Session restored successfully")
	sm.StatusChannel <- StatusMsg{true, nil}
	go sm.loadRecentChats()
	return nil
}

func (sm *SessionManager) loginWithQRCode(client *whatsmeow.Client) error {
	sm.uiHandler.PrintText("Please scan the QR code with your phone")
	qrChan, err := client.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("failed to initialize QR channel: %v", err)
	}
	if err = client.Connect(); err != nil {
		return fmt.Errorf("error connecting to WhatsApp: %v", err)
	}

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			terminal := qrcode.New()
			terminal.SetOutput(tview.ANSIWriter(sm.uiHandler.GetWriter()))
			terminal.Get(evt.Code).Print()
		case "success":
			sm.uiHandler.PrintText("Successfully logged in!")
			sm.StatusChannel <- StatusMsg{true, nil}
			go sm.loadRecentChats()
			return nil
		default:
			sm.uiHandler.PrintText("QR event: " + evt.Event)
		}
	}
	return errors.New("QR code channel closed without success")
}

func (sm *SessionManager) loginWithPairPhone(client *whatsmeow.Client, phone string) error {
	sm.uiHandler.PrintText("Requesting pairing code for " + phone + "...")
	if err := client.Connect(); err != nil {
		return fmt.Errorf("error connecting to WhatsApp: %v", err)
	}
	time.Sleep(1 * time.Second)

	code, err := client.PairPhone(context.Background(), phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return fmt.Errorf("failed to request pairing code: %v", err)
	}
	sm.uiHandler.PrintText("Pairing code: [::b]" + code + "[::-] (WhatsApp -> Linked Devices -> Link with phone number)")

	deadline := time.Now().Add(2 * time.Minute)
	for sm.started && time.Now().Before(deadline) {
		if client.Store.ID != nil {
			sm.uiHandler.PrintText("Successfully paired with phone number!")
			sm.StatusChannel <- StatusMsg{true, nil}
			go sm.loadRecentChats()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("phone pairing timed out")
}

func (sm *SessionManager) loadRecentChats() {
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	sm.loadContacts()
	addedChats := 0

	if sm.client.Store != nil && sm.client.Store.Contacts != nil {
		contacts, err := sm.client.Store.Contacts.GetAllContacts(context.Background())
		if err != nil {
			sm.log().Warnf("loadRecentChats: GetAllContacts error: %v", err)
		} else {
			for jid, contact := range contacts {
				if jid.Server != types.DefaultUserServer {
					continue
				}
				// Only add saved address-book contacts to the chat list (#139);
				// unsaved group-member contacts remain in db.contacts for name resolution.
				if contact.FullName == "" && contact.FirstName == "" {
					continue
				}
				name := contact.FullName
				if name == "" {
					name = contact.FirstName
				}
				sm.db.AddChat(Chat{
					Id:      jid.String(),
					IsGroup: false,
					Name:    name,
				})
				addedChats++
			}
		}
	}

	groupCount := 0
	groups, err := sm.client.GetJoinedGroups(context.Background())
	if err != nil {
		sm.log().Warnf("loadRecentChats: GetJoinedGroups error: %v", err)
	} else {
		for _, group := range groups {
			sm.db.AddChat(Chat{
				Id:      group.JID.String(),
				IsGroup: true,
				Name:    group.Name,
			})
			addedChats++
			groupCount++
		}
	}

	for _, ch := range sm.db.GetChatIds() {
		sm.applyStoredChatSettings(ch.Id)
	}

	sm.log().Infof("loadRecentChats: addedChats=%d groups=%d", addedChats, groupCount)
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	if addedChats > 0 {
		sm.uiHandler.PrintText(fmt.Sprintf("Loaded %d chats", addedChats))
	}
}

func (sm *SessionManager) applyStoredChatSettings(chatID string) {
	if sm.client == nil || sm.client.Store == nil || sm.client.Store.ChatSettings == nil {
		return
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return
	}
	settings, err := sm.client.Store.ChatSettings.GetChatSettings(context.Background(), jid)
	if err != nil || !settings.Found {
		if altJID, altErr := sm.client.Store.GetAltJID(context.Background(), jid); altErr == nil && !altJID.IsEmpty() {
			if altSettings, sErr := sm.client.Store.ChatSettings.GetChatSettings(context.Background(), altJID); sErr == nil && altSettings.Found {
				settings = altSettings
				err = nil
			}
		}
	}
	if err != nil || !settings.Found {
		return
	}
	var muted int64
	if !settings.MutedUntil.IsZero() {
		muted = settings.MutedUntil.Unix()
	}
	sm.db.SetChatSettings(chatID, &settings.Archived, &settings.Pinned, &muted)
}

func (sm *SessionManager) loadContacts() {
	if sm.client == nil || sm.client.Store == nil || sm.client.Store.Contacts == nil {
		return
	}

	contacts, err := sm.client.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to load contacts: %v", err))
		return
	}

	contactCount := 0
	addContactEntry := func(jid types.JID, contact types.ContactInfo) {
		canon := sm.normalizeJID(jid)
		name := contact.FullName
		if name == "" {
			name = contact.FirstName
		}
		if name == "" {
			name = contact.PushName
		}
		if name == "" {
			name = canon.User
		}
		shortName := contact.FullName
		if shortName == "" {
			shortName = contact.FirstName
		}
		if shortName == "" {
			shortName = contact.PushName
		}
		sm.db.AddContact(Contact{
			Id:    canon.String(),
			Name:  name,
			Short: shortName,
		})
		contactCount++
	}
	// Process phone-number JIDs first so saved FullName isn't shadowed by @lid push-names.
	for jid, contact := range contacts {
		if jid.Server == types.DefaultUserServer {
			addContactEntry(jid, contact)
		}
	}
	for jid, contact := range contacts {
		if jid.Server != types.DefaultUserServer {
			addContactEntry(jid, contact)
		}
	}
	if contactCount > 0 {
		sm.uiHandler.PrintText(fmt.Sprintf("Loaded %d contacts", contactCount))
	}
}

func (sm *SessionManager) getChatName(jid types.JID) string {
	jid = sm.normalizeJID(jid)
	if jid.Server == types.GroupServer {
		groupInfo, err := sm.client.GetGroupInfo(context.Background(), jid)
		if err == nil && groupInfo.Name != "" {
			return groupInfo.Name
		}
	}
	if sm.client != nil && sm.client.Store != nil && sm.client.Store.Contacts != nil {
		contact, err := sm.client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.FirstName != "" {
				return contact.FirstName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	return sm.db.GetIdName(jid.String())
}

func (sm *SessionManager) disconnect() error {
	if sm.client != nil && sm.client.IsConnected() {
		sm.client.Disconnect()
		sm.StatusChannel <- StatusMsg{false, nil}
	}
	return nil
}

func (sm *SessionManager) logout() error {
	if sm.client == nil {
		sm.StatusChannel <- StatusMsg{false, nil}
		sm.uiHandler.PrintText("Already logged out")
		return nil
	}

	if sm.client.Store != nil && sm.client.Store.ID != nil {
		if err := sm.client.Logout(context.Background()); err != nil && !errors.Is(err, whatsmeow.ErrNotConnected) {
			sm.uiHandler.PrintText("Warning: Couldn't fully log out: " + err.Error())
		}
	}
	sm.client = nil
	sm.container = nil
	sm.db.ClearAll()
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.StatusChannel <- StatusMsg{false, nil}
	sm.uiHandler.PrintText("Successfully logged out")
	return nil
}

func (sm *SessionManager) execCommand(command Command) {
	switch command.Name {
	default:
		sm.uiHandler.PrintText("[" + config.Config.Colors.Negative + "]Unknown command: [-]" + command.Name)
	case "backlog":
		sm.loadBacklog()
	case "login", "connect":
		err := sm.login()
		if err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("WhatsApp connection failed: %v", err))
			sm.uiHandler.PrintText("Try using /reset to completely reset the connection")
		} else {
			sm.uiHandler.PrintText("Successfully connected to WhatsApp")
		}
	case "reset":
		sm.resetSession()
	case "disconnect":
		sm.uiHandler.PrintError(sm.disconnect())
	case "logout":
		sm.uiHandler.PrintError(sm.logout())
	case "send":
		if checkParam(command.Params, 2) {
			sm.sendText(command.Params[0], strings.Join(command.Params[1:], " "))
		} else {
			sm.printCommandUsage("send", "[chat-id[] [message text[]")
		}
	case "reply":
		if checkParam(command.Params, 3) {
			sm.sendReply(command.Params[0], command.Params[1], strings.Join(command.Params[2:], " "))
		} else {
			sm.printCommandUsage("reply", "[chat-id[] [msg-id[] [message text[]")
		}
	case "select":
		if checkParam(command.Params, 1) {
			sm.setCurrentReceiver(command.Params[0])
		} else {
			sm.printCommandUsage("select", "[chat-id[]")
		}
	case "read":
		sm.markCurrentChatRead()
	case "archive":
		sm.setChatArchive(true)
	case "unarchive":
		sm.setChatArchive(false)
	case "pin":
		sm.setChatPin(true)
	case "unpin":
		sm.setChatPin(false)
	case "mute":
		sm.setChatMute(true, command.Params)
	case "unmute":
		sm.setChatMute(false, nil)
	case "info":
		if checkParam(command.Params, 1) {
			sm.uiHandler.PrintText(sm.db.GetMessageInfo(command.Params[0]))
		} else {
			sm.printCommandUsage("info", "[message-id[]")
		}
	case "download":
		sm.downloadCommand(command.Params, false, false)
	case "open":
		sm.downloadCommand(command.Params, true, false)
	case "show":
		sm.downloadCommand(command.Params, true, true)
	case "preview":
		sm.previewCommand(command.Params)
	case "url":
		sm.openMessageURL(command.Params)
	case "upload":
		sm.sendMediaCommand(command.Params, MessageKindDocument)
	case "sendimage":
		sm.sendMediaCommand(command.Params, MessageKindImage)
	case "sendvideo":
		sm.sendMediaCommand(command.Params, MessageKindVideo)
	case "sendaudio":
		sm.sendMediaCommand(command.Params, MessageKindAudio)
	case "revoke":
		sm.revokeMessage(command.Params)
	case "leave":
		sm.leaveCurrentGroup()
	case "create":
		sm.createGroup(command.Params)
	case "add":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangeAdd, "add", "added new members")
	case "remove":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangeRemove, "remove", "removed members")
	case "admin":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangePromote, "admin", "promoted members")
	case "removeadmin":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangeDemote, "removeadmin", "demoted members")
	case "subject":
		sm.updateCurrentGroupSubject(command.Params)
	case "colorlist":
		out := ""
		for idx := range tcell.ColorNames {
			out += "[" + idx + "]" + idx + "[-]\n"
		}
		sm.uiHandler.PrintText(out)
	case "more":
		sm.loadBacklog()
	}
}

func (sm *SessionManager) loadBacklog() {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("backlog", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	jid, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	existingMessages := sm.db.GetMessages(sm.currentReceiver)
	sm.log().Infof("loadBacklog: receiver=%s existingMessages=%d", sm.currentReceiver, len(existingMessages))
	sm.uiHandler.PrintText("Retrieving message history...")

	oldest, ok := sm.db.GetOldestMessage(sm.currentReceiver)
	if !ok {
		sm.log().Infof("loadBacklog: no local message anchor for receiver=%s", sm.currentReceiver)
		sm.uiHandler.PrintText("No local message anchor found yet. Open the chat after WhatsApp sync delivers some history, then try /backlog again.")
		return
	}
	sm.log().Infof("loadBacklog: anchor id=%s ts=%d fromMe=%v", oldest.Id, oldest.Timestamp, oldest.FromMe)

	senderJID := types.EmptyJID
	if oldest.SenderId != "" {
		if parsedSender, parseErr := types.ParseJID(oldest.SenderId); parseErr == nil {
			senderJID = parsedSender
		}
	}
	req := sm.client.BuildHistorySyncRequest(&types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     jid,
			Sender:   senderJID,
			IsFromMe: oldest.FromMe,
			IsGroup:  strings.Contains(sm.currentReceiver, GROUPSUFFIX),
		},
		ID:        types.MessageID(oldest.Id),
		Timestamp: time.Unix(int64(oldest.Timestamp), 0),
	}, config.Config.General.BacklogMsgQuantity)
	if _, err = sm.client.SendPeerMessage(context.Background(), req); err != nil {
		sm.log().Warnf("loadBacklog: SendPeerMessage failed: %v", err)
		sm.uiHandler.PrintError(fmt.Errorf("failed to request message history: %v", err))
		sm.uiHandler.NewScreen(existingMessages)
		return
	}
	sm.log().Infof("loadBacklog: SendPeerMessage succeeded for receiver=%s", sm.currentReceiver)

	deadline := time.Now().Add(5 * time.Second)
	for len(sm.db.GetMessages(sm.currentReceiver)) == len(existingMessages) && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}

	updated := sm.db.GetMessages(sm.currentReceiver)
	sm.log().Infof("loadBacklog: after wait updated=%d existing=%d", len(updated), len(existingMessages))
	if len(updated) > len(existingMessages) {
		sm.uiHandler.NewScreen(updated)
	} else {
		sm.uiHandler.PrintText("No additional messages found. WhatsApp may limit history access.")
	}
}

func (sm *SessionManager) resetSession() {
	if sm.client != nil {
		if sm.client.IsConnected() {
			sm.client.Disconnect()
		}
		if sm.client.Store != nil {
			if err := sm.client.Store.Delete(context.Background()); err != nil {
				sm.uiHandler.PrintText("Warning: Couldn't remove session: " + err.Error())
			}
		}
	}

	sm.client = nil
	sm.container = nil
	sm.db.ClearAll()
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	dbPath := config.GetSessionFilePath() + ".db"
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		sm.uiHandler.PrintText("Warning: Couldn't remove database file: " + err.Error())
	}
	sm.StatusChannel <- StatusMsg{false, nil}
	sm.uiHandler.PrintText("Session reset. Use /connect to reconnect with a new QR code.")
}

func (sm *SessionManager) markCurrentChatRead() {
	sm.markCurrentChatReadInternal(false)
}

func (sm *SessionManager) markCurrentChatReadInternal(quiet bool) {
	if sm.currentReceiver == "" {
		if !quiet {
			sm.printCommandUsage("read", "-> only works in a chat")
		}
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		if !quiet {
			sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		}
		return
	}

	chatJID, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		if !quiet {
			sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		}
		return
	}

	unreadMessages := sm.db.MarkChatRead(sm.currentReceiver)
	if len(unreadMessages) == 0 {
		sm.uiHandler.SetChats(sm.db.GetChatIds())
		if !quiet {
			sm.uiHandler.PrintText("No unread messages in current chat")
		}
		return
	}

	type senderBatch struct {
		sender    types.JID
		ids       []types.MessageID
		timestamp time.Time
	}
	batches := make(map[string]*senderBatch)
	for _, msg := range unreadMessages {
		sender := chatJID
		if strings.Contains(sm.currentReceiver, GROUPSUFFIX) && msg.SenderId != "" {
			sender, err = types.ParseJID(msg.SenderId)
			if err != nil {
				continue
			}
		}
		key := sender.String()
		if _, ok := batches[key]; !ok {
			batches[key] = &senderBatch{sender: sender}
		}
		batches[key].ids = append(batches[key].ids, types.MessageID(msg.Id))
		ts := time.Unix(int64(msg.Timestamp), 0)
		if ts.After(batches[key].timestamp) {
			batches[key].timestamp = ts
		}
	}

	for _, batch := range batches {
		if batch.timestamp.IsZero() {
			batch.timestamp = time.Now()
		}
		if err := sm.client.MarkRead(context.Background(), batch.ids, batch.timestamp, chatJID, batch.sender); err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("failed to mark messages as read: %v", err))
		}
	}

	sm.uiHandler.SetChats(sm.db.GetChatIds())
}

func (sm *SessionManager) downloadCommand(params []string, preview, show bool) {
	if !checkParam(params, 1) {
		name := "download"
		if preview && !show {
			name = "open"
		} else if show {
			name = "show"
		}
		sm.printCommandUsage(name, "[message-id[]")
		return
	}

	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		sm.uiHandler.PrintError(errors.New("message not found"))
		return
	}
	if show && msg.Kind != MessageKindImage && msg.Kind != MessageKindSticker {
		sm.uiHandler.PrintError(errors.New("show only works for image and sticker messages"))
		return
	}

	path, err := sm.downloadMessage(msg, preview)
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if show {
		sm.uiHandler.PrintFile(path)
		return
	}
	if preview {
		sm.uiHandler.OpenFile(path)
		return
	}
	sm.uiHandler.PrintText("[::d] -> " + path + "[::-]")
}

func (sm *SessionManager) previewCommand(params []string) {
	if !checkParam(params, 1) {
		return
	}
	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		return
	}
	if msg.Kind != MessageKindImage && msg.Kind != MessageKindSticker {
		return
	}
	go func() {
		if _, err := sm.downloadMessage(msg, true); err != nil {
			sm.uiHandler.PrintError(err)
			return
		}
		if sm.currentReceiver == msg.ChatId {
			sm.uiHandler.NewScreen(sm.getMessages(msg.ChatId))
		}
	}()
}

func (sm *SessionManager) openMessageURL(params []string) {
	if !checkParam(params, 1) {
		sm.printCommandUsage("url", "[message-id[]")
		return
	}
	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		sm.uiHandler.PrintError(errors.New("message not found"))
		return
	}
	url := urlPattern.FindString(msg.Text)
	if url == "" {
		sm.uiHandler.PrintText("No URL found in message")
		return
	}
	sm.uiHandler.OpenFile(url)
}

func (sm *SessionManager) sendMediaCommand(params []string, kind MessageKind) {
	if sm.currentReceiver == "" {
		sm.printCommandUsage(commandNameForKind(kind), "-> only works in a chat")
		return
	}
	if !checkParam(params, 1) {
		sm.printCommandUsage(commandNameForKind(kind), "/path/to/file")
		return
	}
	path := normalizeUploadPath(strings.Join(params, " "))
	sm.uiHandler.PrintError(sm.sendMedia(sm.currentReceiver, path, kind))
}

func normalizeUploadPath(raw string) string {
	p := strings.TrimSpace(raw)
	if len(p) >= 2 && ((p[0] == '"' && p[len(p)-1] == '"') || (p[0] == '\'' && p[len(p)-1] == '\'')) {
		p = p[1 : len(p)-1]
	}
	if strings.HasPrefix(p, "file://") {
		p = strings.TrimPrefix(p, "file://")
		if unescaped, err := url.PathUnescape(p); err == nil {
			p = unescaped
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if _, err := os.Stat(p); err != nil && strings.Contains(p, `\`) {
		replacer := strings.NewReplacer(
			`\ `, " ",
			`\'`, "'",
			`\"`, `"`,
			`\(`, "(",
			`\)`, ")",
			`\[`, "[",
			`\]`, "]",
			`\&`, "&",
		)
		unescaped := replacer.Replace(p)
		if _, err2 := os.Stat(unescaped); err2 == nil {
			return unescaped
		}
	}
	return p
}

func (sm *SessionManager) revokeMessage(params []string) {
	if !checkParam(params, 1) {
		sm.printCommandUsage("revoke", "[message-id[]")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		sm.uiHandler.PrintError(errors.New("message not found"))
		return
	}
	chatJID, err := types.ParseJID(msg.ChatId)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid chat JID: %v", err))
		return
	}
	if _, err = sm.client.RevokeMessage(context.Background(), chatJID, types.MessageID(msg.Id)); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	sm.db.MarkMessageRevoked(msg.Id)
	if sm.currentReceiver == msg.ChatId {
		sm.uiHandler.NewScreen(sm.getMessages(msg.ChatId))
	}
	sm.uiHandler.PrintText("revoked: " + msg.Id)
}

func (sm *SessionManager) leaveCurrentGroup() {
	groupJID, err := sm.currentGroupJID()
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if err = sm.client.LeaveGroup(context.Background(), groupJID); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	sm.uiHandler.PrintText("left group " + groupJID.String())
}

func (sm *SessionManager) createGroup(params []string) {
	if !checkParam(params, 1) {
		sm.printCommandUsage("create", "[user-id[] [user-id[] New Group Subject")
		sm.printCommandUsage("create", "New Group Subject")
		return
	}

	participants := make([]types.JID, 0)
	idx := 0
	for idx < len(params) && strings.Contains(params[idx], CONTACTSUFFIX) {
		participant, err := types.ParseJID(params[idx])
		if err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("invalid user id %q: %v", params[idx], err))
			return
		}
		participants = append(participants, participant)
		idx++
	}

	name := strings.Join(params[idx:], " ")
	if name == "" {
		name = strings.Join(params, " ")
		participants = nil
	}

	groupInfo, err := sm.client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: participants,
	})
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}

	sm.db.AddChat(Chat{
		Id:          groupInfo.JID.String(),
		IsGroup:     true,
		Name:        groupInfo.Name,
		LastMessage: time.Now().Unix(),
	})
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.uiHandler.PrintText("created new group " + groupInfo.JID.String())
}

func (sm *SessionManager) updateCurrentGroupParticipants(params []string, action whatsmeow.ParticipantChange, command, success string) {
	groupJID, err := sm.currentGroupJID()
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if !checkParam(params, 1) {
		sm.printCommandUsage(command, "[user-id[]")
		return
	}

	participants := make([]types.JID, 0, len(params))
	for _, raw := range params {
		jid, err := types.ParseJID(raw)
		if err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("invalid user id %q: %v", raw, err))
			return
		}
		participants = append(participants, jid)
	}

	if _, err = sm.client.UpdateGroupParticipants(context.Background(), groupJID, participants, action); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	sm.uiHandler.PrintText(success + " for " + groupJID.String())
}

func (sm *SessionManager) updateCurrentGroupSubject(params []string) {
	groupJID, err := sm.currentGroupJID()
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if !checkParam(params, 1) {
		sm.printCommandUsage("subject", "new-subject -> in group chat")
		return
	}

	name := strings.Join(params, " ")
	if err = sm.client.SetGroupName(context.Background(), groupJID, name); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}

	sm.db.AddChat(Chat{
		Id:      groupJID.String(),
		IsGroup: true,
		Name:    name,
	})
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.uiHandler.PrintText("updated subject for " + groupJID.String())
}

func (sm *SessionManager) currentGroupJID() (types.JID, error) {
	if sm.currentReceiver == "" || !strings.Contains(sm.currentReceiver, GROUPSUFFIX) {
		return types.JID{}, errors.New("not a group")
	}
	return types.ParseJID(sm.currentReceiver)
}

func (sm *SessionManager) printCommandUsage(command, usage string) {
	sm.uiHandler.PrintText("[" + config.Config.Colors.Negative + "]Usage:[-] " + command + " " + usage)
}

func checkParam(arr []string, length int) bool {
	return arr != nil && len(arr) >= length
}

func (sm *SessionManager) getMessages(wid string) []Message {
	return sm.db.GetMessages(wid)
}

func (sm *SessionManager) sendText(wid, text string) {
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	receiver, err := types.ParseJID(wid)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	raw := &waProto.Message{Conversation: proto.String(text)}
	sm.lastSent = time.Now()
	resp, err := sm.client.SendMessage(context.Background(), receiver, raw)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to send message: %v", err))
		return
	}

	newMsg := sm.outgoingMessageFromSendResponse(resp, wid, raw, MessageKindText, text, "", "")
	sm.db.AddMessage(newMsg, false)
	if sm.currentReceiver == wid {
		sm.uiHandler.NewMessage(newMsg)
	}
	sm.uiHandler.SetChats(sm.db.GetChatIds())
}

func (sm *SessionManager) setChatArchive(archive bool) {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("archive", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}
	jid, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}
	patch := appstate.BuildArchive(jid, archive, time.Time{}, nil)
	if err := sm.client.SendAppState(context.Background(), patch); err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to update archive state: %v", err))
		return
	}
	sm.db.SetChatSettings(sm.currentReceiver, &archive, nil, nil)
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	if archive {
		sm.uiHandler.PrintText("Chat archived")
	} else {
		sm.uiHandler.PrintText("Chat unarchived")
	}
}

func (sm *SessionManager) setChatPin(pin bool) {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("pin", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}
	jid, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}
	patch := appstate.BuildPin(jid, pin)
	if err := sm.client.SendAppState(context.Background(), patch); err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to update pin state: %v", err))
		return
	}
	sm.db.SetChatSettings(sm.currentReceiver, nil, &pin, nil)
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	if pin {
		sm.uiHandler.PrintText("Chat pinned")
	} else {
		sm.uiHandler.PrintText("Chat unpinned")
	}
}

func (sm *SessionManager) setChatMute(mute bool, params []string) {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("mute", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}
	jid, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	var duration time.Duration
	var mutedUntil int64
	if mute {
		duration = 8 * time.Hour
		if len(params) > 0 {
			arg := strings.ToLower(strings.TrimSpace(params[0]))
			if arg == "0" || arg == "forever" {
				duration = 0
			} else if hrs, convErr := strconv.Atoi(arg); convErr == nil && hrs > 0 {
				duration = time.Duration(hrs) * time.Hour
			} else {
				sm.printCommandUsage("mute", "[hours|forever]")
				return
			}
		}
		if duration == 0 {
			mutedUntil = -1
		} else {
			mutedUntil = time.Now().Add(duration).Unix()
		}
	}

	patch := appstate.BuildMute(jid, mute, duration)
	if err := sm.client.SendAppState(context.Background(), patch); err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to update mute state: %v", err))
		return
	}
	sm.db.SetChatSettings(sm.currentReceiver, nil, nil, &mutedUntil)
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	if !mute {
		sm.uiHandler.PrintText("Chat unmuted")
	} else if mutedUntil == -1 {
		sm.uiHandler.PrintText("Chat muted forever")
	} else {
		sm.uiHandler.PrintText(fmt.Sprintf("Chat muted for %d hours", int(duration.Hours())))
	}
}

func (sm *SessionManager) sendReply(wid, quotedMsgID, text string) {
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}
	receiver, err := types.ParseJID(wid)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}
	quoted, ok := sm.db.GetMessage(quotedMsgID)
	if !ok {
		sm.uiHandler.PrintError(errors.New("quoted message not found"))
		return
	}

	quotedProto := quoted.RawMessage
	if quotedProto == nil {
		quotedProto = &waProto.Message{Conversation: proto.String(quoted.Text)}
	}

	participant := quoted.SenderId
	if quoted.FromMe {
		if sm.client.Store != nil && sm.client.Store.ID != nil {
			participant = sm.client.Store.ID.ToNonAD().String()
		}
	} else if participant == "" {
		participant = wid
	}

	raw := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waProto.ContextInfo{
				StanzaID:      proto.String(quoted.Id),
				Participant:   proto.String(participant),
				QuotedMessage: quotedProto,
			},
		},
	}

	sm.lastSent = time.Now()
	resp, err := sm.client.SendMessage(context.Background(), receiver, raw)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to send reply: %v", err))
		return
	}

	newMsg := sm.outgoingMessageFromSendResponse(resp, wid, raw, MessageKindText, text, "", "")
	newMsg.QuotedId = quoted.Id
	if quoted.FromMe {
		newMsg.QuotedSender = "Me"
	} else {
		newMsg.QuotedSender = quoted.ContactShort
	}
	newMsg.QuotedText = quoted.Text

	sm.db.AddMessage(newMsg, false)
	if sm.currentReceiver == wid {
		sm.uiHandler.NewMessage(newMsg)
	}
	sm.uiHandler.SetChats(sm.db.GetChatIds())
}

func (sm *SessionManager) sendMedia(chatID, path string, kind MessageKind) error {
	if sm.client == nil || !sm.client.IsConnected() {
		return errors.New("not connected to WhatsApp")
	}

	path = normalizeUploadPath(path)
	data, mimeType, fileName, err := readUploadFile(path)
	if err != nil {
		return err
	}

	receiver, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid JID: %v", err)
	}

	uploadResp, err := sm.client.Upload(context.Background(), data, uploadMediaType(kind))
	if err != nil {
		return fmt.Errorf("failed to upload file: %v", err)
	}

	fileLength := uploadResp.FileLength
	raw := &waProto.Message{}
	switch kind {
	case MessageKindImage:
		raw.ImageMessage = &waProto.ImageMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
		}
	case MessageKindVideo:
		raw.VideoMessage = &waProto.VideoMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
		}
	case MessageKindAudio:
		raw.AudioMessage = &waProto.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
			PTT:           proto.Bool(false),
		}
	case MessageKindDocument:
		raw.DocumentMessage = &waProto.DocumentMessage{
			Mimetype:      proto.String(mimeType),
			Title:         proto.String(fileName),
			FileName:      proto.String(fileName),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
		}
	default:
		return errors.New("unsupported media type")
	}

	sm.lastSent = time.Now()
	resp, err := sm.client.SendMessage(context.Background(), receiver, raw)
	if err != nil {
		return fmt.Errorf("failed to send media message: %v", err)
	}

	text := mediaDisplayText(kind, fileName, "")
	newMsg := sm.outgoingMessageFromSendResponse(resp, chatID, raw, kind, text, mimeType, fileName)
	if kind == MessageKindImage && config.Config.General.InlineImages {
		if mkErr := os.MkdirAll(config.Config.General.PreviewPath, 0o755); mkErr == nil {
			_ = os.WriteFile(filepath.Join(config.Config.General.PreviewPath, DownloadFileName(newMsg)), data, 0o644)
		}
	}
	sm.db.AddMessage(newMsg, false)
	if sm.currentReceiver == chatID {
		sm.uiHandler.NewMessage(newMsg)
	}
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	return nil
}

func (sm *SessionManager) outgoingMessageFromSendResponse(resp whatsmeow.SendResponse, chatID string, raw *waProto.Message, kind MessageKind, text, mimeType, fileName string) Message {
	selfID := ""
	if sm.client != nil && sm.client.Store != nil && sm.client.Store.ID != nil {
		selfID = sm.client.Store.ID.String()
	}

	contactID := chatID
	if strings.Contains(chatID, GROUPSUFFIX) {
		contactID = selfID
	}

	return Message{
		Id:           string(resp.ID),
		ChatId:       chatID,
		SenderId:     selfID,
		ContactId:    contactID,
		ContactName:  sm.db.GetIdName(contactID),
		ContactShort: sm.db.GetIdShort(contactID),
		Timestamp:    uint64(resp.Timestamp.Unix()),
		FromMe:       true,
		Text:         text,
		Kind:         kind,
		MimeType:     mimeType,
		FileName:     fileName,
		RawMessage:   raw,
	}
}

func notify(title, message string) error {
	if !config.Config.General.EnableNotifications {
		return nil
	} else if config.Config.General.UseTerminalBell {
		_, err := fmt.Printf("\a")
		return err
	}
	return beeep.Notify(title, message, "")
}

type eventHandler struct {
	sm *SessionManager
}

func (eh *eventHandler) Handle(evt interface{}) {
	eh.sm.log().Debugf("event: %T", evt)
	switch v := evt.(type) {
	case *events.Message:
		eh.sm.log().Infof("event.Message: chat=%s sender=%s id=%s ts=%v", v.Info.Chat.String(), v.Info.Sender.String(), v.Info.ID, v.Info.Timestamp)
		eh.handleLiveMessage(v)
	case *events.HistorySync:
		if v != nil && v.Data != nil {
			eh.sm.log().Infof("event.HistorySync: type=%v progress=%d conversations=%d", v.Data.GetSyncType(), v.Data.GetProgress(), len(v.Data.GetConversations()))
		}
		eh.handleHistorySync(v)
	case *events.OfflineSyncPreview:
		eh.sm.log().Infof("event.OfflineSyncPreview: total=%d messages=%d receipts=%d notifications=%d appData=%d", v.Total, v.Messages, v.Receipts, v.Notifications, v.AppDataChanges)
	case *events.OfflineSyncCompleted:
		eh.sm.log().Infof("event.OfflineSyncCompleted: count=%d", v.Count)
	case *events.AppStateSyncComplete:
		eh.sm.log().Infof("event.AppStateSyncComplete: name=%v", v.Name)
	case *events.Archive:
		if v.Action != nil {
			chatID := eh.sm.normalizeJID(v.JID).String()
			archived := v.Action.GetArchived()
			eh.sm.db.SetChatSettings(chatID, &archived, nil, nil)
			eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		}
	case *events.Pin:
		if v.Action != nil {
			chatID := eh.sm.normalizeJID(v.JID).String()
			pinned := v.Action.GetPinned()
			eh.sm.db.SetChatSettings(chatID, nil, &pinned, nil)
			eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		}
	case *events.Mute:
		if v.Action != nil {
			chatID := eh.sm.normalizeJID(v.JID).String()
			var mutedUntil int64
			if v.Action.GetMuted() {
				ts := v.Action.GetMuteEndTimestamp()
				if ts <= 0 {
					mutedUntil = -1
				} else if ts > 1e12 {
					mutedUntil = ts / 1000
				} else {
					mutedUntil = ts
				}
			}
			eh.sm.db.SetChatSettings(chatID, nil, nil, &mutedUntil)
			eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		}
	case *events.MarkChatAsRead:
		if v.Action != nil && v.Action.GetRead() {
			chatID := eh.sm.normalizeJID(v.JID).String()
			eh.sm.db.MarkChatRead(chatID)
			eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		}
	case *events.Connected:
		eh.sm.StatusChannel <- StatusMsg{true, nil}
	case *events.Disconnected:
		eh.sm.StatusChannel <- StatusMsg{false, nil}
	case *events.LoggedOut:
		eh.sm.StatusChannel <- StatusMsg{false, nil}
		eh.sm.uiHandler.PrintText("Logged out: " + fmt.Sprintf("%v", v.Reason))
	}
}

func (eh *eventHandler) handleLiveMessage(evt *events.Message) {
	msg, action, ok := eh.normalizeEventMessage(evt)
	if !ok {
		return
	}

	switch action {
	case "revoke":
		if eh.sm.db.MarkMessageRevoked(msg.Id) && eh.sm.currentReceiver == msg.ChatId {
			eh.sm.uiHandler.NewScreen(eh.sm.getMessages(msg.ChatId))
		}
		eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		return
	case "ignore":
		return
	}

	markUnread := !msg.FromMe && msg.ChatId != eh.sm.currentReceiver
	isNew := eh.sm.db.AddMessage(msg, markUnread)
	if msg.ChatId == eh.sm.currentReceiver {
		if isNew {
			eh.sm.uiHandler.NewMessage(msg)
		} else {
			eh.sm.uiHandler.NewScreen(eh.sm.getMessages(msg.ChatId))
		}
	} else if markUnread && msg.Timestamp > uint64(time.Now().Unix()-30) && !eh.sm.db.IsChatMuted(msg.ChatId) {
		if err := notify(msg.ContactShort, msg.Text); err != nil {
			eh.sm.uiHandler.PrintError(err)
		}
	}
	if msg.Kind == MessageKindSticker {
		go eh.sm.prefetchStickerPreview(msg)
	}
	eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
}

func (eh *eventHandler) handleHistorySync(evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}

	totalConvs := 0
	totalParsed := 0
	totalSkipped := 0
	lidChats := 0
	for _, conv := range evt.Data.GetConversations() {
		chatID := conv.GetID()
		if chatID == "" {
			chatID = conv.GetNewJID()
		}
		if chatID == "" {
			continue
		}

		chatJID, err := types.ParseJID(chatID)
		if err != nil {
			continue
		}
		totalConvs++
		if chatJID.Server == types.HiddenUserServer {
			lidChats++
			eh.sm.log().Debugf("handleHistorySync: @lid chat=%s pnJID=%s name=%q msgs=%d", chatID, conv.GetPnJID(), conv.GetName(), len(conv.GetMessages()))
			var altJID types.JID
			if conv.GetPnJID() != "" {
				if parsedPN, pnErr := types.ParseJID(conv.GetPnJID()); pnErr == nil {
					altJID = parsedPN
				}
			}
			chatJID = eh.sm.resolveCanonicalJID(chatJID, altJID)
			chatID = chatJID.String()
		}

		chatName := ""
		if chatJID.Server == types.DefaultUserServer {
			resolvedName := eh.sm.getChatName(chatJID)
			if resolvedName != "" && resolvedName != chatJID.User {
				chatName = resolvedName
			}
		}
		if chatName == "" {
			chatName = conv.GetName()
		}
		if chatName == "" {
			chatName = conv.GetDisplayName()
		}
		if chatName == "" {
			chatName = eh.sm.getChatName(chatJID)
		}

		lastMessage := int64(conv.GetLastMsgTimestamp())
		if lastMessage == 0 {
			lastMessage = int64(conv.GetConversationTimestamp())
		}
		eh.sm.db.AddChat(Chat{
			Id:          chatID,
			IsGroup:     chatJID.Server == types.GroupServer,
			Name:        chatName,
			Unread:      int(conv.GetUnreadCount()),
			LastMessage: lastMessage,
		})

		for _, histMsg := range conv.GetMessages() {
			webMsg := histMsg.GetMessage()
			if webMsg == nil {
				totalSkipped++
				continue
			}
			parsed, err := eh.sm.client.ParseWebMessage(chatJID, webMsg)
			if err != nil {
				totalSkipped++
				continue
			}
			msg, action, ok := eh.normalizeEventMessage(parsed)
			if !ok || action != "" {
				totalSkipped++
				continue
			}
			eh.sm.db.AddMessage(msg, false)
			totalParsed++
		}
		eh.sm.db.UpdateChatUnread(chatID, int(conv.GetUnreadCount()))
	}

	eh.sm.log().Infof("handleHistorySync: convs=%d lidChats=%d parsed=%d skipped=%d", totalConvs, lidChats, totalParsed, totalSkipped)
	eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
	if eh.sm.currentReceiver != "" {
		eh.sm.uiHandler.NewScreen(eh.sm.getMessages(eh.sm.currentReceiver))
	}
}

func (eh *eventHandler) canonicalChatJID(info types.MessageInfo) types.JID {
	if !info.IsGroup {
		alt := info.SenderAlt
		if info.IsFromMe {
			alt = info.RecipientAlt
		}
		return eh.sm.resolveCanonicalJID(info.Chat, alt)
	}
	return info.Chat.ToNonAD()
}

func (eh *eventHandler) normalizeEventMessage(evt *events.Message) (Message, string, bool) {
	if evt == nil || evt.Message == nil {
		return Message{}, "ignore", false
	}

	if protocol := evt.Message.GetProtocolMessage(); protocol != nil {
		if protocol.GetType() == waProto.ProtocolMessage_REVOKE && protocol.GetKey() != nil {
			return Message{
				Id:     protocol.GetKey().GetID(),
				ChatId: eh.canonicalChatJID(evt.Info).String(),
			}, "revoke", true
		}
		return Message{}, "ignore", false
	}

	msg, ok := eh.messageFromInfo(evt.Info, evt.Message)
	return msg, "", ok
}

func (eh *eventHandler) messageFromInfo(info types.MessageInfo, raw *waProto.Message) (Message, bool) {
	if raw == nil {
		return Message{}, false
	}

	chatJID := eh.canonicalChatJID(info)
	chatID := chatJID.String()
	if chatID == "" {
		return Message{}, false
	}
	senderJID := eh.sm.resolveCanonicalJID(info.Sender, info.SenderAlt)
	info.Chat = chatJID
	info.Sender = senderJID

	contactID, contactName, contactShort := eh.contactForMessage(info)
	msg := Message{
		Id:           string(info.ID),
		ChatId:       chatID,
		SenderId:     senderJID.String(),
		ContactId:    contactID,
		ContactName:  contactName,
		ContactShort: contactShort,
		Timestamp:    uint64(info.Timestamp.Unix()),
		FromMe:       info.IsFromMe,
		RawMessage:   raw,
	}

	switch {
	case raw.GetConversation() != "":
		msg.Kind = MessageKindText
		msg.Text = raw.GetConversation()
		return msg, true
	case raw.GetExtendedTextMessage() != nil:
		ext := raw.GetExtendedTextMessage()
		msg.Kind = MessageKindText
		msg.Text = ext.GetText()
		msg.Forwarded = ext.GetContextInfo().GetIsForwarded()
		msg.QuotedId, msg.QuotedSender, msg.QuotedText = eh.extractQuoteFromContext(ext.GetContextInfo())
		return msg, true
	case raw.GetImageMessage() != nil:
		image := raw.GetImageMessage()
		msg.Kind = MessageKindImage
		msg.MimeType = image.GetMimetype()
		msg.Text = mediaDisplayText(MessageKindImage, "", image.GetCaption())
		msg.Forwarded = image.GetContextInfo().GetIsForwarded()
		msg.QuotedId, msg.QuotedSender, msg.QuotedText = eh.extractQuoteFromContext(image.GetContextInfo())
		return msg, true
	case raw.GetVideoMessage() != nil:
		video := raw.GetVideoMessage()
		msg.Kind = MessageKindVideo
		msg.MimeType = video.GetMimetype()
		msg.Text = mediaDisplayText(MessageKindVideo, "", video.GetCaption())
		msg.Forwarded = video.GetContextInfo().GetIsForwarded()
		msg.QuotedId, msg.QuotedSender, msg.QuotedText = eh.extractQuoteFromContext(video.GetContextInfo())
		return msg, true
	case raw.GetAudioMessage() != nil:
		audio := raw.GetAudioMessage()
		msg.Kind = MessageKindAudio
		msg.MimeType = audio.GetMimetype()
		msg.Text = mediaDisplayText(MessageKindAudio, "", "")
		msg.Forwarded = audio.GetContextInfo().GetIsForwarded()
		msg.QuotedId, msg.QuotedSender, msg.QuotedText = eh.extractQuoteFromContext(audio.GetContextInfo())
		return msg, true
	case raw.GetDocumentMessage() != nil:
		doc := raw.GetDocumentMessage()
		msg.Kind = MessageKindDocument
		msg.MimeType = doc.GetMimetype()
		msg.FileName = doc.GetFileName()
		msg.Text = mediaDisplayText(MessageKindDocument, doc.GetFileName(), doc.GetCaption())
		msg.Forwarded = doc.GetContextInfo().GetIsForwarded()
		msg.QuotedId, msg.QuotedSender, msg.QuotedText = eh.extractQuoteFromContext(doc.GetContextInfo())
		return msg, true
	case raw.GetStickerMessage() != nil:
		sticker := raw.GetStickerMessage()
		msg.Kind = MessageKindSticker
		msg.MimeType = sticker.GetMimetype()
		msg.Text = mediaDisplayText(MessageKindSticker, "", "")
		msg.Forwarded = sticker.GetContextInfo().GetIsForwarded()
		msg.QuotedId, msg.QuotedSender, msg.QuotedText = eh.extractQuoteFromContext(sticker.GetContextInfo())
		return msg, true
	default:
		return Message{}, false
	}
}

func (eh *eventHandler) extractQuoteFromContext(ctxInfo *waProto.ContextInfo) (string, string, string) {
	if ctxInfo == nil || ctxInfo.GetStanzaID() == "" {
		return "", "", ""
	}
	quotedID := ctxInfo.GetStanzaID()
	quotedSender := ""
	if p := ctxInfo.GetParticipant(); p != "" {
		if pJID, err := types.ParseJID(p); err == nil {
			pJID = eh.sm.normalizeJID(pJID)
			if eh.sm.client != nil && eh.sm.client.Store != nil && eh.sm.client.Store.ID != nil &&
				pJID.User == eh.sm.client.Store.ID.User {
				quotedSender = "Me"
			} else {
				quotedSender = eh.getContactShort(pJID)
			}
		} else {
			quotedSender = p
		}
	}

	quotedText := ""
	if qm := ctxInfo.GetQuotedMessage(); qm != nil {
		switch {
		case qm.GetConversation() != "":
			quotedText = qm.GetConversation()
		case qm.GetExtendedTextMessage() != nil:
			quotedText = qm.GetExtendedTextMessage().GetText()
		case qm.GetImageMessage() != nil:
			quotedText = mediaDisplayText(MessageKindImage, "", qm.GetImageMessage().GetCaption())
		case qm.GetVideoMessage() != nil:
			quotedText = mediaDisplayText(MessageKindVideo, "", qm.GetVideoMessage().GetCaption())
		case qm.GetAudioMessage() != nil:
			quotedText = mediaDisplayText(MessageKindAudio, "", "")
		case qm.GetDocumentMessage() != nil:
			doc := qm.GetDocumentMessage()
			quotedText = mediaDisplayText(MessageKindDocument, doc.GetFileName(), doc.GetCaption())
		case qm.GetStickerMessage() != nil:
			quotedText = mediaDisplayText(MessageKindSticker, "", "")
		}
	}
	return quotedID, quotedSender, quotedText
}

func (eh *eventHandler) contactForMessage(info types.MessageInfo) (string, string, string) {
	if info.IsGroup {
		id := info.Sender.String()
		return id, eh.getContactName(info.Sender), eh.getContactShort(info.Sender)
	}
	id := info.Chat.String()
	chat := info.Chat
	return id, eh.getContactName(chat), eh.getContactShort(chat)
}

func (eh *eventHandler) getContactName(jid types.JID) string {
	jid = eh.sm.normalizeJID(jid)
	if eh.sm.client != nil && eh.sm.client.Store != nil && eh.sm.client.Store.Contacts != nil {
		contact, err := eh.sm.client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.FirstName != "" {
				return contact.FirstName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	return eh.sm.db.GetIdName(jid.String())
}

func (eh *eventHandler) getContactShort(jid types.JID) string {
	jid = eh.sm.normalizeJID(jid)
	if eh.sm.client != nil && eh.sm.client.Store != nil && eh.sm.client.Store.Contacts != nil {
		contact, err := eh.sm.client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.FirstName != "" {
				return contact.FirstName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	return eh.sm.db.GetIdShort(jid.String())
}

func (sm *SessionManager) downloadMessage(msg Message, preview bool) (string, error) {
	if sm.client == nil || !sm.client.IsConnected() {
		return "", errors.New("not connected to WhatsApp")
	}

	downloadable, err := downloadableFromMessage(msg)
	if err != nil {
		return "", err
	}

	baseDir := config.Config.General.DownloadPath
	if preview {
		baseDir = config.Config.General.PreviewPath
	}
	if err = os.MkdirAll(baseDir, 0o755); err != nil {
		return "", err
	}

	fileName := downloadFileName(msg)
	fullPath := filepath.Join(baseDir, fileName)
	if _, err = os.Stat(fullPath); err == nil {
		return fullPath, nil
	}

	data, err := sm.client.Download(context.Background(), downloadable)
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(fullPath, data, 0o644); err != nil {
		return "", err
	}
	return fullPath, nil
}

func downloadableFromMessage(msg Message) (whatsmeow.DownloadableMessage, error) {
	if msg.RawMessage == nil {
		return nil, errors.New("This is not a downloadable message")
	}
	switch msg.Kind {
	case MessageKindImage:
		if media := msg.RawMessage.GetImageMessage(); media != nil {
			return media, nil
		}
	case MessageKindVideo:
		if media := msg.RawMessage.GetVideoMessage(); media != nil {
			return media, nil
		}
	case MessageKindAudio:
		if media := msg.RawMessage.GetAudioMessage(); media != nil {
			return media, nil
		}
	case MessageKindDocument:
		if media := msg.RawMessage.GetDocumentMessage(); media != nil {
			return media, nil
		}
	case MessageKindSticker:
		if media := msg.RawMessage.GetStickerMessage(); media != nil {
			return media, nil
		}
	}
	return nil, errors.New("This is not a downloadable message")
}

func (sm *SessionManager) prefetchChatStickers(msgs []Message, limit int) {
	count := 0
	for i := len(msgs) - 1; i >= 0 && count < limit; i-- {
		if msgs[i].Kind == MessageKindSticker {
			sm.prefetchStickerPreview(msgs[i])
			count++
		}
	}
}

func (sm *SessionManager) prefetchStickerPreview(msg Message) {
	if !config.Config.General.InlineImages || msg.Kind != MessageKindSticker || msg.RawMessage == nil {
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		return
	}
	fullPath := filepath.Join(config.Config.General.PreviewPath, DownloadFileName(msg))
	if _, err := os.Stat(fullPath); err == nil {
		return
	}
	if _, err := sm.downloadMessage(msg, true); err == nil {
		if sm.currentReceiver == msg.ChatId {
			sm.uiHandler.NewScreen(sm.getMessages(msg.ChatId))
		}
	}
}

func readUploadFile(path string) ([]byte, string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", err
	}
	fileName := filepath.Base(path)
	mimeType := detectMimeType(path, data)
	return data, mimeType, fileName, nil
}

func detectMimeType(path string, data []byte) string {
	if len(data) == 0 {
		if extType := mime.TypeByExtension(filepath.Ext(path)); extType != "" {
			return stripMimeParams(extType)
		}
		return "application/octet-stream"
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	detected := stripMimeParams(http.DetectContentType(sample))
	if extType := mime.TypeByExtension(filepath.Ext(path)); extType != "" {
		extType = stripMimeParams(extType)
		if detected == "application/octet-stream" || strings.HasPrefix(extType, "audio/") || strings.HasPrefix(extType, "video/") {
			return extType
		}
	}
	return detected
}

func stripMimeParams(value string) string {
	if idx := strings.Index(value, ";"); idx >= 0 {
		return value[:idx]
	}
	return value
}

func DownloadFileName(msg Message) string {
	if msg.FileName != "" {
		safeName := path.Base(strings.ReplaceAll(msg.FileName, "\\", "/"))
		if safeName != "" && safeName != "." && safeName != ".." {
			return safeName
		}
	}
	ext := ""
	if msg.MimeType != "" {
		if exts, err := mime.ExtensionsByType(msg.MimeType); err == nil && len(exts) > 0 {
			ext = exts[0]
		}
	}
	if ext == "" && (msg.MimeType == "image/webp" || msg.Kind == MessageKindSticker) {
		ext = ".webp"
	}
	return msg.Id + ext
}

func downloadFileName(msg Message) string {
	return DownloadFileName(msg)
}

func uploadMediaType(kind MessageKind) whatsmeow.MediaType {
	switch kind {
	case MessageKindImage:
		return whatsmeow.MediaImage
	case MessageKindVideo:
		return whatsmeow.MediaVideo
	case MessageKindAudio:
		return whatsmeow.MediaAudio
	default:
		return whatsmeow.MediaDocument
	}
}

func commandNameForKind(kind MessageKind) string {
	switch kind {
	case MessageKindImage:
		return "sendimage"
	case MessageKindVideo:
		return "sendvideo"
	case MessageKindAudio:
		return "sendaudio"
	default:
		return "upload"
	}
}

func mediaDisplayText(kind MessageKind, fileName, caption string) string {
	label := "[FILE]"
	switch kind {
	case MessageKindImage:
		label = "[IMAGE]"
	case MessageKindVideo:
		label = "[VIDEO]"
	case MessageKindAudio:
		label = "[AUDIO]"
	case MessageKindDocument:
		label = "[DOCUMENT]"
	case MessageKindSticker:
		label = "[STICKER]"
	}
	parts := []string{label}
	if fileName != "" && kind == MessageKindDocument {
		parts = append(parts, fileName)
	}
	if caption != "" {
		parts = append(parts, caption)
	}
	return strings.Join(parts, " ")
}
