package main

import (
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

	textView = tview.NewTextView().SetDynamicColors(true)
	textInput = tview.NewInputField()
	sessionManager = &messages.SessionManager{
		CommandChannel: make(chan messages.Command, 16),
	}
	currentReceiver = messages.Chat{}
	treeView = MakeTree()
	LoadShortcuts()

	allChats = []messages.Chat{
		{Id: "111@s.whatsapp.net", Name: "Alice"},
		{Id: "222@s.whatsapp.net", Name: "Bob"},
		{Id: "333@g.us", Name: "Work Group", IsGroup: true},
	}
	chatSearchMode = false
	chatSearchQuery = ""
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
