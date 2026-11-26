package main

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/normen/whatscli/messages"
	"github.com/rivo/tview"
)

func setupTestUI(t *testing.T) {
	t.Helper()
	app = tview.NewApplication()
	simScreen := tcell.NewSimulationScreen("UTF-8")
	if err := simScreen.Init(); err != nil {
		t.Fatalf("failed to init simulation screen: %v", err)
	}
	app.SetScreen(simScreen)

	textView = tview.NewTextView().SetDynamicColors(true).SetRegions(true)
	attachTextViewMouseHandler()
	textInput = tview.NewInputField()
	initEmojiPicker()
	initMultilinePreview()
	setupTextInput()
	sessionManager = &messages.SessionManager{
		CommandChannel: make(chan messages.Command, 16),
	}
	currentReceiver = messages.Chat{}
	expandedImages = make(map[string]bool)
	draftLines = nil
	treeView = MakeTree()
	LoadShortcuts()

	allChats = []messages.Chat{
		{Id: "111@s.whatsapp.net", Name: "Alice"},
		{Id: "222@s.whatsapp.net", Name: "Bob"},
		{Id: "333@g.us", Name: "Work Group", IsGroup: true},
	}
	chatSearchMode = false
	chatSearchQuery = ""
	viewingArchived = false
	renderChats(allChats, "")
}

func TestTreeSearchSlashFilterAndEscape(t *testing.T) {
	setupTestUI(t)

	if got := len(chatRoot.GetChildren()); got != 3 {
		t.Fatalf("expected 3 chats initially, got %d", got)
	}

	// Press '/' on treeView to enter search mode
	capture := treeView.GetInputCapture()
	if ev := capture(tcell.NewEventKey(tcell.KeyRune, '/', tcell.ModNone)); ev != nil {
		t.Fatalf("expected '/' to be consumed in treeView")
	}
	if !chatSearchMode {
		t.Fatalf("expected chatSearchMode to be true")
	}

	// Type "bo" -> should filter to "Bob"
	capture(tcell.NewEventKey(tcell.KeyRune, 'b', tcell.ModNone))
	capture(tcell.NewEventKey(tcell.KeyRune, 'o', tcell.ModNone))
	if got := len(chatRoot.GetChildren()); got != 1 {
		t.Fatalf("expected 1 filtered chat for 'bo', got %d", got)
	}
	if chatRoot.GetChildren()[0].GetReference().(messages.Chat).Name != "Bob" {
		t.Fatalf("expected 'Bob', got %v", chatRoot.GetChildren()[0].GetReference())
	}

	// Press Esc -> should clear filter and restore all 3 chats
	capture(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if chatSearchMode || chatSearchQuery != "" {
		t.Fatalf("expected search mode cleared on Esc")
	}
	if got := len(chatRoot.GetChildren()); got != 3 {
		t.Fatalf("expected 3 chats after Esc, got %d", got)
	}
}

func TestSearchCommand(t *testing.T) {
	setupTestUI(t)

	sndTxt = "/search work"
	EnterCommand(tcell.KeyEnter)

	if !chatSearchMode {
		t.Fatalf("expected chatSearchMode to be true after /search")
	}
	if chatSearchQuery != "work" {
		t.Fatalf("expected chatSearchQuery 'work', got %q", chatSearchQuery)
	}
	if got := len(chatRoot.GetChildren()); got != 1 {
		t.Fatalf("expected 1 filtered chat for 'work', got %d", got)
	}
	if chatRoot.GetChildren()[0].GetReference().(messages.Chat).Name != "Work Group" {
		t.Fatalf("expected 'Work Group', got %v", chatRoot.GetChildren()[0].GetReference())
	}
}

func TestTreeSearchEnterSelectsChat(t *testing.T) {
	setupTestUI(t)

	sndTxt = "/search work"
	EnterCommand(tcell.KeyEnter)

	capture := treeView.GetInputCapture()
	if ev := capture(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)); ev != nil {
		t.Fatalf("expected Enter to be consumed in search mode")
	}

	if chatSearchMode || chatSearchQuery != "" {
		t.Fatalf("expected search mode and query cleared on Enter")
	}
	if currentReceiver.Id != "333@g.us" || currentReceiver.Name != "Work Group" {
		t.Fatalf("expected currentReceiver '333@g.us' ('Work Group'), got %+v", currentReceiver)
	}
	if chatRoot.GetText() != "Chats" {
		t.Fatalf("expected chatRoot title 'Chats', got %q", chatRoot.GetText())
	}
	if got := len(chatRoot.GetChildren()); got != 3 {
		t.Fatalf("expected all 3 chats restored after Enter, got %d", got)
	}

	select {
	case cmd := <-sessionManager.CommandChannel:
		if cmd.Name != "select" || len(cmd.Params) != 1 || cmd.Params[0] != "333@g.us" {
			t.Fatalf("unexpected command sent: %+v", cmd)
		}
	default:
		t.Fatalf("expected select command on CommandChannel")
	}
}

func TestQuoteReplyFlow(t *testing.T) {
	setupTestUI(t)
	currentReceiver = messages.Chat{Id: "111@s.whatsapp.net", Name: "Alice"}
	curRegions = []messages.Message{
		{Id: "orig-42", ChatId: "111@s.whatsapp.net", ContactShort: "Alice", Text: "are you free?"},
	}
	textView.SetText(getMessagesString(curRegions))
	textView.Highlight("orig-42")

	handleReplyMessage(nil)
	if replyTargetMsg == nil || replyTargetMsg.Id != "orig-42" {
		t.Fatalf("expected replyTargetMsg 'orig-42', got %+v", replyTargetMsg)
	}

	sndTxt = "yes!"
	EnterCommand(tcell.KeyEnter)

	if replyTargetMsg != nil {
		t.Fatalf("expected replyTargetMsg cleared after send")
	}
	select {
	case cmd := <-sessionManager.CommandChannel:
		if cmd.Name != "reply" || len(cmd.Params) != 3 || cmd.Params[0] != "111@s.whatsapp.net" || cmd.Params[1] != "orig-42" || cmd.Params[2] != "yes!" {
			t.Fatalf("unexpected reply command: %+v", cmd)
		}
	default:
		t.Fatalf("expected reply command on CommandChannel")
	}
}

func TestGetTextMessageStringShowsQuote(t *testing.T) {
	msg := &messages.Message{
		Id:           "m1",
		FromMe:       true,
		Timestamp:    1700000000,
		Text:         "on my way",
		QuotedSender: "Alice",
		QuotedText:   "where are you?",
	}
	got := getTextMessageString(msg)
	if !strings.Contains(got, "↱ Alice: where are you?") {
		t.Fatalf("expected quote preview in rendered message, got %q", got)
	}
}

func TestArchivedViewSwitchAndBack(t *testing.T) {
	setupTestUI(t)
	allChats = []messages.Chat{
		{Id: "1@s.whatsapp.net", Name: "Pinned Active", Pinned: true},
		{Id: "2@s.whatsapp.net", Name: "Normal Active"},
		{Id: "3@s.whatsapp.net", Name: "Old Archived", Archived: true},
		{Id: "4@s.whatsapp.net", Name: "Muted Archived", Archived: true, MutedUntil: -1},
	}
	renderChats(allChats, "")

	children := chatRoot.GetChildren()
	if len(children) != 3 {
		t.Fatalf("expected Archived(2) + 2 active chats, got %d", len(children))
	}
	if children[0].GetText() != "Archived (2)" {
		t.Fatalf("expected first node 'Archived (2)', got %q", children[0].GetText())
	}
	if children[1].GetText() != "(P) Pinned Active" {
		t.Fatalf("expected pinned prefix, got %q", children[1].GetText())
	}

	// Simulate Enter on Archived (2)
	treeView.SetCurrentNode(children[0])
	treeView.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(p tview.Primitive) {})
	if !viewingArchived || chatRoot.GetText() != "Archived" {
		t.Fatalf("expected viewingArchived=true and title 'Archived', got %v / %q", viewingArchived, chatRoot.GetText())
	}
	archChildren := chatRoot.GetChildren()
	if len(archChildren) != 3 {
		t.Fatalf("expected Back + 2 archived chats, got %d", len(archChildren))
	}
	if archChildren[0].GetText() != "<- Back to Chats" {
		t.Fatalf("expected '<- Back to Chats', got %q", archChildren[0].GetText())
	}
	if !strings.Contains(archChildren[2].GetText(), "(M)") {
		t.Fatalf("expected muted badge on muted archived chat, got %q", archChildren[2].GetText())
	}

	// Simulate Enter on <- Back to Chats
	treeView.SetCurrentNode(archChildren[0])
	treeView.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(p tview.Primitive) {})
	if viewingArchived || chatRoot.GetText() != "Chats" {
		t.Fatalf("expected viewingArchived=false and title 'Chats', got %v / %q", viewingArchived, chatRoot.GetText())
	}
	if got := len(chatRoot.GetChildren()); got != 3 {
		t.Fatalf("expected 3 nodes back in Chats view, got %d", got)
	}
}

func TestDoubleClickTogglesExpandedImage(t *testing.T) {
	setupTestUI(t)
	textView.SetRect(0, 0, 120, 40)
	cols, rows := getFullFitImageSize()
	if cols != 116 || rows != 38 {
		t.Fatalf("expected full-fit size 116x38, got %dx%d", cols, rows)
	}

	curRegions = []messages.Message{
		{Id: "img-99", ChatId: "111@s.whatsapp.net", Kind: messages.MessageKindImage, Text: "[IMAGE]"},
	}
	textView.SetText(getMessagesString(curRegions))
	textView.Highlight("img-99")

	capture := textView.GetMouseCapture()
	capture(tview.MouseLeftDoubleClick, tcell.NewEventMouse(10, 5, tcell.Button1, tcell.ModNone))
	if !expandedImages["img-99"] {
		t.Fatalf("expected img-99 to be expanded after double-click")
	}
	select {
	case cmd := <-sessionManager.CommandChannel:
		if cmd.Name != "preview" || len(cmd.Params) != 1 || cmd.Params[0] != "img-99" {
			t.Fatalf("unexpected preview command: %+v", cmd)
		}
	default:
		t.Fatalf("expected preview command queued on double-click")
	}

	capture(tview.MouseLeftDoubleClick, tcell.NewEventMouse(10, 5, tcell.Button1, tcell.ModNone))
	if expandedImages["img-99"] {
		t.Fatalf("expected img-99 to collapse on second double-click")
	}
}

func TestRenderChatsPreservesCursorPosition(t *testing.T) {
	setupTestUI(t)
	currentReceiver = messages.Chat{Id: "111@s.whatsapp.net", Name: "Alice"}

	// Hover over Work Group without pressing Enter
	children := chatRoot.GetChildren()
	treeView.SetCurrentNode(children[2])

	// Prepend a new chat at the top (simulating incoming message refresh)
	updated := append([]messages.Chat{{Id: "000@s.whatsapp.net", Name: "New Message"}}, allChats...)
	renderChats(updated, "")

	cur := treeView.GetCurrentNode()
	if cur == nil {
		t.Fatalf("expected non-nil current node after refresh")
	}
	ch, ok := cur.GetReference().(messages.Chat)
	if !ok || ch.Id != "333@g.us" {
		t.Fatalf("expected cursor preserved on Work Group (333@g.us), got %+v", cur.GetReference())
	}
}

func TestInputTypingQOLAltAndCtrlBackspace(t *testing.T) {
	setupTestUI(t)
	textInput.SetText("hello world test")

	capture := textInput.GetInputCapture()
	ih := textInput.InputHandler()
	noopFocus := func(p tview.Primitive) {}

	if ev := capture(tcell.NewEventKey(tcell.KeyBackspace2, 0, tcell.ModAlt)); ev != nil {
		ih(ev, noopFocus)
	}
	if got := textInput.GetText(); got != "hello world " {
		t.Fatalf("expected 'hello world ' after Alt+Backspace, got %q", got)
	}

	if ev := capture(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModCtrl)); ev != nil {
		ih(ev, noopFocus)
	}
	if got := textInput.GetText(); got != "hello " {
		t.Fatalf("expected 'hello ' after Ctrl+Backspace, got %q", got)
	}
}

func TestEmojiPickerAndShortcodeReplacement(t *testing.T) {
	setupTestUI(t)
	emojiPickerDismissedQuery = ""

	// Direct shortcode auto-replacement
	textInput.SetText("bro :sob:")
	if got := textInput.GetText(); got != "bro 😭" {
		t.Fatalf("expected 'bro 😭', got %q", got)
	}

	// Picker opens for trailing :so when textInput is focused
	textInput.Focus(func(p tview.Primitive) {})
	textInput.SetText("bro :so")
	if !emojiPickerVisible() {
		t.Fatalf("expected emoji picker visible for 'bro :so'")
	}
	if len(emojiPickerMatches) == 0 || emojiPickerMatches[0].Code != "sob" {
		t.Fatalf("expected 'sob' first in picker, got %+v", emojiPickerMatches)
	}
	if !acceptEmojiPickerSelection() {
		t.Fatalf("expected acceptEmojiPickerSelection to return true")
	}
	if got := textInput.GetText(); got != "bro 😭 " {
		t.Fatalf("expected 'bro 😭 ' after accepting picker, got %q", got)
	}
	if emojiPickerVisible() {
		t.Fatalf("expected emoji picker closed after selection")
	}
}

func TestMultilineAltEnterSendAndBackspace(t *testing.T) {
	setupTestUI(t)
	currentReceiver = messages.Chat{Id: "111@s.whatsapp.net", Name: "Alice"}
	capture := textInput.GetInputCapture()

	textInput.SetText("line one")
	if ev := capture(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt)); ev != nil {
		t.Fatalf("expected Alt+Enter to be consumed")
	}
	if len(draftLines) != 1 || draftLines[0] != "line one" {
		t.Fatalf("expected draftLines=['line one'], got %+v", draftLines)
	}
	if got := textInput.GetText(); got != "" {
		t.Fatalf("expected empty textInput after Alt+Enter, got %q", got)
	}

	// Plain Backspace on empty line restores previous draft line
	if ev := capture(tcell.NewEventKey(tcell.KeyBackspace2, 0, tcell.ModNone)); ev != nil {
		t.Fatalf("expected Backspace on empty draft line to be consumed")
	}
	if len(draftLines) != 0 || textInput.GetText() != "line one" {
		t.Fatalf("expected draftLines empty and textInput 'line one', got lines=%+v text=%q", draftLines, textInput.GetText())
	}

	// Queue line one again, add line two with emoji, and send with Enter
	capture(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
	textInput.SetText("line two :fire:")
	EnterCommand(tcell.KeyEnter)

	if len(draftLines) != 0 {
		t.Fatalf("expected draftLines cleared after send, got %+v", draftLines)
	}
	select {
	case cmd := <-sessionManager.CommandChannel:
		if cmd.Name != "send" || len(cmd.Params) != 2 || cmd.Params[1] != "line one\nline two 🔥" {
			t.Fatalf("unexpected multiline send command: %+v", cmd)
		}
	default:
		t.Fatalf("expected send command on CommandChannel")
	}
}

func TestGetTextMessageStringFormatsMultiline(t *testing.T) {
	msg := &messages.Message{
		Id:        "ml-1",
		FromMe:    true,
		Timestamp: 1700000000,
		Text:      "line one\nline two\n\nline four",
	}
	got := getTextMessageString(msg)
	if !strings.Contains(got, "line one\n[") || !strings.Contains(got, "  │ [-::-]line two") || !strings.Contains(got, "  │ [-::-]line four") {
		t.Fatalf("expected multiline continuation bars, got %q", got)
	}
}
