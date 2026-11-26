package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/tslocum/cbind"
	"github.com/gdamore/tcell/v2"
	"github.com/normen/whatscli/config"
	"github.com/normen/whatscli/messages"
	"github.com/rivo/tview"
	"github.com/skratchdot/open-golang/open"
	"github.com/zyedidia/clipboard"
)

var VERSION string = "v1.1.6"

var sndTxt string = ""
var currentReceiver messages.Chat = messages.Chat{}
var curRegions []messages.Message
var allChats []messages.Chat
var chatSearchMode bool = false
var chatSearchQuery string = ""
var viewingArchived bool = false
var replyTargetMsg *messages.Message
var expandedImages = make(map[string]bool)

type archivedNavAction int

const (
	navOpenArchived archivedNavAction = iota + 1
	navBackToChats
)

var textView *tview.TextView
var treeView *tview.TreeView
var textInput *tview.InputField
var topBar *tview.TextView
var infoBar *tview.TextView

var chatRoot *tview.TreeNode
var app *tview.Application

var sessionManager *messages.SessionManager

var keyBindings *cbind.Configuration

var uiHandler messages.UiMessageHandler

func main() {
	verbose := flag.Bool("verbose", false, "enable verbose debug logging to file")
	verboseShort := flag.Bool("v", false, "enable verbose debug logging to file (shorthand)")
	logFile := flag.String("log", "", "path to debug log file (implies --verbose)")
	pairPhone := flag.String("pair-phone", "", "link device using phone number (international format, e.g. 919876543210) instead of QR code")
	flag.Parse()

	config.InitConfig()
	uiHandler = UiHandler{}
	sessionManager = &messages.SessionManager{
		PairPhone: *pairPhone,
	}
	if *verbose || *verboseShort || *logFile != "" {
		logPath := *logFile
		if logPath == "" {
			logPath = config.GetSessionFilePath() + ".log"
		}
		if logger, closer, err := messages.NewFileLogger(logPath, "whatscli", "DEBUG"); err == nil {
			sessionManager.Logger = logger
			defer closer.Close()
		} else {
			fmt.Fprintf(os.Stderr, "failed to open log file %s: %v\n", logPath, err)
		}
	}
	sessionManager.Init(uiHandler)

	app = tview.NewApplication()

	sideBarWidth := config.Config.Ui.ChatSidebarWidth
	gridLayout := tview.NewGrid()
	gridLayout.SetRows(1, 0, 1)
	gridLayout.SetColumns(sideBarWidth, 0, sideBarWidth)
	gridLayout.SetBorders(true)
	gridLayout.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	gridLayout.SetBordersColor(tcell.ColorNames[config.Config.Colors.Borders])

	cmdPrefix := config.Config.General.CmdPrefix
	topBar = tview.NewTextView()
	topBar.SetDynamicColors(true)
	topBar.SetScrollable(false)
	topBar.SetText("[::b] WhatsCLI " + VERSION + "  [-::d]Type " + cmdPrefix + "help or press " + config.Config.Keymap.CommandHelp + " for help")
	topBar.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])

	infoBar = tview.NewTextView()
	infoBar.SetDynamicColors(true)
	UpdateStatusBar(messages.SessionStatus{})

	textView = tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	textView.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	textView.SetTextColor(tcell.ColorNames[config.Config.Colors.Text])
	attachTextViewMouseHandler()

	PrintHelp()

	textInput = tview.NewInputField()
	textInput.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	textInput.SetFieldBackgroundColor(tcell.ColorNames[config.Config.Colors.InputBackground])
	textInput.SetFieldTextColor(tcell.ColorNames[config.Config.Colors.InputText])
	textInput.SetChangedFunc(func(change string) {
		sndTxt = change
	})
	textInput.SetDoneFunc(EnterCommand)
	textInput.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyDown {
			offset, _ := textView.GetScrollOffset()
			offset += 1
			textView.ScrollTo(offset, 0)
			syncScreen()
			return nil
		}
		if event.Key() == tcell.KeyUp {
			offset, _ := textView.GetScrollOffset()
			offset -= 1
			textView.ScrollTo(offset, 0)
			syncScreen()
			return nil
		}
		if event.Key() == tcell.KeyPgDn {
			offset, _ := textView.GetScrollOffset()
			offset += 10
			textView.ScrollTo(offset, 0)
			syncScreen()
			return nil
		}
		if event.Key() == tcell.KeyPgUp {
			offset, _ := textView.GetScrollOffset()
			offset -= 10
			textView.ScrollTo(offset, 0)
			syncScreen()
			return nil
		}
		return event
	})

	gridLayout.AddItem(topBar, 0, 0, 1, 4, 0, 0, false)
	gridLayout.AddItem(infoBar, 2, 0, 1, 1, 0, 0, false)
	gridLayout.AddItem(MakeTree(), 1, 0, 1, 1, 0, 0, false)
	gridLayout.AddItem(textView, 1, 1, 1, 3, 0, 0, false)
	gridLayout.AddItem(textInput, 2, 1, 1, 3, 0, 0, false)

	app.SetRoot(gridLayout, true)
	app.EnableMouse(true)
	app.SetFocus(textInput)
	if err := sessionManager.StartManager(); err != nil {
		PrintError(err)
	}
	LoadShortcuts()
	app.Run()
}

// creates the TreeView for chats
func MakeTree() *tview.TreeView {
	rootDir := "Chats"
	chatRoot = tview.NewTreeNode(rootDir).
		SetColor(tcell.ColorNames[config.Config.Colors.ListHeader])
	treeView = tview.NewTreeView().
		SetRoot(chatRoot).
		SetCurrentNode(chatRoot)
	treeView.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])

	openTreeNode := func(node *tview.TreeNode, focusInput bool, allowNav bool) {
		if node == nil {
			return
		}
		reference := node.GetReference()
		if reference == nil {
			SetDisplayedChat(messages.Chat{"", false, "", 0, 0, false, false, 0})
			return
		}
		if nav, ok := reference.(archivedNavAction); ok {
			if !allowNav {
				return
			}
			switch nav {
			case navOpenArchived:
				viewingArchived = true
			case navBackToChats:
				viewingArchived = false
			}
			renderChats(allChats, "")
			if children := chatRoot.GetChildren(); len(children) > 0 {
				treeView.SetCurrentNode(children[0])
			} else {
				treeView.SetCurrentNode(chatRoot)
			}
			syncScreen()
			return
		}
		children := node.GetChildren()
		if len(children) == 0 {
			recv := reference.(messages.Chat)
			SetDisplayedChat(recv)
			if focusInput && app != nil {
				app.SetFocus(textInput)
			}
		} else {
			node.SetExpanded(!node.IsExpanded())
		}
	}

	treeView.SetSelectedFunc(func(node *tview.TreeNode) {
		openTreeNode(node, true, true)
	})
	treeView.SetChangedFunc(func(node *tview.TreeNode) {
		if config.Config.Ui.SelectOnEnter {
			return
		}
		openTreeNode(node, false, false)
	})
	return treeView
}

func handleFocusMessage(ev *tcell.EventKey) *tcell.EventKey {
	if !textView.HasFocus() {
		app.SetFocus(textView)
		if curRegions != nil && len(curRegions) > 0 {
			textView.Highlight(curRegions[len(curRegions)-1].Id)
		}
	}
	return nil
}

func handleFocusInput(ev *tcell.EventKey) *tcell.EventKey {
	ResetMsgSelection()
	if !textInput.HasFocus() {
		app.SetFocus(textInput)
	}
	return nil
}

func handleFocusContacts(ev *tcell.EventKey) *tcell.EventKey {
	ResetMsgSelection()
	if !treeView.HasFocus() {
		app.SetFocus(treeView)
	}
	return nil
}

func handleSwitchPanels(ev *tcell.EventKey) *tcell.EventKey {
	ResetMsgSelection()
	if !textInput.HasFocus() {
		app.SetFocus(textInput)
	} else {
		app.SetFocus(treeView)
	}
	return nil
}

func handleCommand(command string) func(ev *tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		sessionManager.CommandChannel <- messages.Command{command, nil}
		return nil
	}
}

func handleCopyUser(ev *tcell.EventKey) *tcell.EventKey {
	if hls := textView.GetHighlights(); len(hls) > 0 {
		for _, val := range curRegions {
			if val.Id == hls[0] {
				clipboard.WriteAll(val.ContactId, "clipboard")
				PrintText("copied id of " + val.ContactName + " to clipboard")
			}
		}
		ResetMsgSelection()
	} else if currentReceiver.Id != "" {
		clipboard.WriteAll(currentReceiver.Id, "clipboard")
		PrintText("copied id of " + currentReceiver.Name + " to clipboard")
	}
	return nil
}

func handlePasteUser(ev *tcell.EventKey) *tcell.EventKey {
	if clip, err := safeReadClipboard(); err == nil {
		textInput.SetText(textInput.GetText() + " " + clip)
	} else {
		PrintError(err)
	}
	return nil
}

func safeReadClipboard() (clip string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("clipboard paste is unavailable: %v", rec)
		}
	}()
	return clipboard.ReadAll("clipboard")
}

func handleQuit(ev *tcell.EventKey) *tcell.EventKey {
	sessionManager.CommandChannel <- messages.Command{"disconnect", nil}
	app.Stop()
	return nil
}

func handleHelp(ev *tcell.EventKey) *tcell.EventKey {
	PrintHelp()
	return nil
}

func handleMessageCommand(command string) func(ev *tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		hls := textView.GetHighlights()
		if len(hls) > 0 {
			sessionManager.CommandChannel <- messages.Command{command, []string{hls[0]}}
			ResetMsgSelection()
			app.SetFocus(textInput)
		}
		return nil
	}
}

func clearReplyTarget() {
	replyTargetMsg = nil
	if textInput != nil {
		textInput.SetLabel("")
	}
}

func handleReplyMessage(ev *tcell.EventKey) *tcell.EventKey {
	hls := textView.GetHighlights()
	if len(hls) == 0 {
		return nil
	}
	for _, val := range curRegions {
		if val.Id == hls[0] {
			msgCopy := val
			replyTargetMsg = &msgCopy
			label := msgCopy.ContactShort
			if msgCopy.FromMe {
				label = "Me"
			}
			if label == "" {
				label = "message"
			}
			textInput.SetLabel("Reply to " + label + ": ")
			ResetMsgSelection()
			if app != nil {
				app.SetFocus(textInput)
			}
			break
		}
	}
	return nil
}

func handleMessagesMove(amount int) func(ev *tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		if curRegions == nil || len(curRegions) == 0 {
			return nil
		}
		hls := textView.GetHighlights()
		if len(hls) > 0 {
			newId := GetOffsetMsgId(hls[0], amount)
			if newId != "" {
				textView.Highlight(newId)
			}
		} else {
			if amount < 0 {
				textView.Highlight(curRegions[0].Id)
			} else {
				textView.Highlight(curRegions[len(curRegions)-1].Id)
			}
		}
		textView.ScrollToHighlight()
		syncScreen()
		return nil
	}
}

func handleChatPanelUp(ev *tcell.EventKey) *tcell.EventKey {
	//TODO: scroll selection in treeView? or chatRoot? How?
	return ev
}

func handleChatPanelDown(ev *tcell.EventKey) *tcell.EventKey {
	return ev
}

func handleMessagesLast(ev *tcell.EventKey) *tcell.EventKey {
	if curRegions == nil || len(curRegions) == 0 {
		return nil
	}
	textView.Highlight(curRegions[len(curRegions)-1].Id)
	textView.ScrollToHighlight()
	syncScreen()
	return nil
}

func handleMessagesFirst(ev *tcell.EventKey) *tcell.EventKey {
	if curRegions == nil || len(curRegions) == 0 {
		return nil
	}
	textView.Highlight(curRegions[0].Id)
	textView.ScrollToHighlight()
	syncScreen()
	return nil
}

func handleExitMessages(ev *tcell.EventKey) *tcell.EventKey {
	if curRegions == nil || len(curRegions) == 0 {
		return nil
	}
	ResetMsgSelection()
	app.SetFocus(textInput)
	return nil
}

// load the key map
func LoadShortcuts() {
	// global bindings for app
	keyBindings = cbind.NewConfiguration()
	if err := keyBindings.Set(config.Config.Keymap.FocusMessages, handleFocusMessage); err != nil {
		PrintErrorMsg("focus_messages:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.FocusInput, handleFocusInput); err != nil {
		PrintErrorMsg("focus_input:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.FocusChats, handleFocusContacts); err != nil {
		PrintErrorMsg("focus_contacts:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.SwitchPanels, handleSwitchPanels); err != nil {
		PrintErrorMsg("switch_panels:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandRead, handleCommand("read")); err != nil {
		PrintErrorMsg("command_read:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.Copyuser, handleCopyUser); err != nil {
		PrintErrorMsg("copyuser:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.Pasteuser, handlePasteUser); err != nil {
		PrintErrorMsg("pasteuser:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandBacklog, handleCommand("backlog")); err != nil {
		PrintErrorMsg("command_backlog:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandConnect, handleCommand("login")); err != nil {
		PrintErrorMsg("command_connect:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandQuit, handleQuit); err != nil {
		PrintErrorMsg("command_quit:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandHelp, handleHelp); err != nil {
		PrintErrorMsg("command_help:", err)
	}
	app.SetInputCapture(keyBindings.Capture)
	// bindings for chat message text view
	keysMessages := cbind.NewConfiguration()
	if err := keysMessages.Set(config.Config.Keymap.MessageDownload, handleMessageCommand("download")); err != nil {
		PrintErrorMsg("message_download:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageOpen, handleMessageCommand("open")); err != nil {
		PrintErrorMsg("message_open:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.Copyuser, handleCopyUser); err != nil {
		PrintErrorMsg("copyuser:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.Pasteuser, handlePasteUser); err != nil {
		PrintErrorMsg("pasteuser:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageShow, handleMessageCommand("show")); err != nil {
		PrintErrorMsg("message_show:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageUrl, handleMessageCommand("url")); err != nil {
		PrintErrorMsg("message_url:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageInfo, handleMessageCommand("info")); err != nil {
		PrintErrorMsg("message_info:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageRevoke, handleMessageCommand("revoke")); err != nil {
		PrintErrorMsg("message_revoke:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageReply, handleReplyMessage); err != nil {
		PrintErrorMsg("message_reply:", err)
	}
	keysMessages.SetKey(tcell.ModNone, tcell.KeyEscape, handleExitMessages)
	keysMessages.SetKey(tcell.ModNone, tcell.KeyUp, handleMessagesMove(-1))
	keysMessages.SetKey(tcell.ModNone, tcell.KeyDown, handleMessagesMove(1))
	keysMessages.SetKey(tcell.ModNone, tcell.KeyPgUp, handleMessagesMove(-10))
	keysMessages.SetKey(tcell.ModNone, tcell.KeyPgDn, handleMessagesMove(10))
	keysMessages.SetRune(tcell.ModNone, 'k', handleMessagesMove(-1))
	keysMessages.SetRune(tcell.ModNone, 'j', handleMessagesMove(1))
	keysMessages.SetRune(tcell.ModNone, 'g', handleMessagesFirst)
	keysMessages.SetRune(tcell.ModNone, 'G', handleMessagesLast)
	keysMessages.SetRune(tcell.ModCtrl, 'u', handleMessagesMove(-10))
	keysMessages.SetRune(tcell.ModCtrl, 'd', handleMessagesMove(10))
	textView.SetInputCapture(keysMessages.Capture)
	keysChatPanel := cbind.NewConfiguration()
	keysChatPanel.SetRune(tcell.ModCtrl, 'u', handleChatPanelUp)
	keysChatPanel.SetRune(tcell.ModCtrl, 'd', handleChatPanelDown)
	treeView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if !chatSearchMode {
			if event.Key() == tcell.KeyEscape && viewingArchived {
				viewingArchived = false
				renderChats(allChats, "")
				syncScreen()
				return nil
			}
			if event.Key() == tcell.KeyRune && event.Rune() == '/' {
				chatSearchMode = true
				chatSearchQuery = ""
				renderChats(allChats, chatSearchQuery)
				syncScreen()
				return nil
			}
			switch event.Key() {
			case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn, tcell.KeyHome, tcell.KeyEnd:
				syncScreen()
			case tcell.KeyRune:
				if event.Rune() == 'j' || event.Rune() == 'k' {
					syncScreen()
				}
			}
			return keysChatPanel.Capture(event)
		}

		switch event.Key() {
		case tcell.KeyEscape:
			chatSearchMode = false
			chatSearchQuery = ""
			renderChats(allChats, "")
			syncScreen()
			return nil
		case tcell.KeyEnter:
			node := treeView.GetCurrentNode()
			chatSearchMode = false
			chatSearchQuery = ""
			if node != nil {
				if recv, ok := node.GetReference().(messages.Chat); ok && recv.Id != "" {
					SetDisplayedChat(recv)
				}
			}
			renderChats(allChats, "")
			syncScreen()
			app.SetFocus(textInput)
			return nil
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			runes := []rune(chatSearchQuery)
			if len(runes) > 0 {
				chatSearchQuery = string(runes[:len(runes)-1])
				renderChats(allChats, chatSearchQuery)
				syncScreen()
			}
			return nil
		case tcell.KeyRune:
			chatSearchQuery += string(event.Rune())
			renderChats(allChats, chatSearchQuery)
			syncScreen()
			return nil
		}
		return event
	})
}

// prints help to chat view
func PrintHelp() {
	cmdPrefix := config.Config.General.CmdPrefix
	fmt.Fprintln(textView, "[-::u]Keys:[-::-]")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Global")
	fmt.Fprintln(textView, "[::b] Up/Down[::-] = Scroll history/chats")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.SwitchPanels, "[::-] = Switch input/chats")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.FocusMessages, "[::-] = Focus message panel")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.CommandQuit, "[::-] = Exit app")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Message panel[-::-]")
	fmt.Fprintln(textView, "[::b] Up/Down[::-] = select message")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageDownload, "[::-] = Download attachment")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageOpen, "[::-] = Download & open attachment")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageShow, "[::-] = Show image / sticker in color")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageUrl, "[::-] = Find URL in message and open it")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageRevoke, "[::-] = Revoke message")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageInfo, "[::-] = Info about message")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageReply, "[::-] = Reply / quote message")
	fmt.Fprintln(textView, "[::b] Double-click image/sticker [::-] = Toggle full-size view")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Chats panel[-::-]")
	fmt.Fprintln(textView, "[::b] Enter [::-] = Open chat")
	fmt.Fprintln(textView, "[::b] / [::-] = Search chats (Esc to clear)")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Config file in ->", config.GetConfigFilePath())
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Type [::b]"+cmdPrefix+"commands[::-] to see all commands")
	fmt.Fprintln(textView, "")
}

func PrintCommands() {
	cmdPrefix := config.Config.General.CmdPrefix
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::u]Commands:[-::-]")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Global[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"connect [::-]or[::b]", config.Config.Keymap.CommandConnect, "[::-] = (Re)Connect to server")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"disconnect[::-]  = Close the connection")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"logout[::-]  = Remove login data from computer")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"reset[::-]  = Remove stored session and reconnect cleanly")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"quit [::-]or[::b]", config.Config.Keymap.CommandQuit, "[::-] = Exit app")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"search[::-] [query]  = Search/filter chats (or press / in chat list)")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"archived[::-]  = Toggle Archived chats view")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Chat[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"backlog [::-]or[::b]", config.Config.Keymap.CommandBacklog, "[::-] = load next", config.Config.General.BacklogMsgQuantity, "previous messages")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"read [::-]or[::b]", config.Config.Keymap.CommandRead, "[::-] = mark new messages in chat as read")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"archive / "+cmdPrefix+"unarchive[::-]  = Archive or unarchive current chat")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"pin / "+cmdPrefix+"unpin[::-]  = Pin or unpin current chat")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"mute [hours|forever] / "+cmdPrefix+"unmute[::-]  = Mute or unmute current chat")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"upload[::-] /path/to/file  = Upload any file as document")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"sendimage[::-] /path/to/file  = Send image message")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"sendvideo[::-] /path/to/file  = Send video message")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"sendaudio[::-] /path/to/file  = Send audio message")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Groups[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"leave[::-]  = Leave group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"create[::-] [user-id[] [user-id[] Group Subject  = Create group with users")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"subject[::-] New Subject  = Change subject of group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"add[::-] [user-id[]  = Add user to group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"remove[::-] [user-id[]  = Remove user from group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"admin[::-] [user-id[]  = Set admin role for user in group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"removeadmin[::-] [user-id[]  = Remove admin role for user in group")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Use[::b]", config.Config.Keymap.Copyuser, "[::-]to copy a selected user id to clipboard")
	fmt.Fprintln(textView, "Use[::b]", config.Config.Keymap.Pasteuser, "[::-]to paste clipboard to text input")
	fmt.Fprintln(textView, "")
}

// called when text is entered by the user
func EnterCommand(key tcell.Key) {
	if key == tcell.KeyEsc {
		textInput.SetText("")
		clearReplyTarget()
		return
	}
	if sndTxt == "" {
		return
	}
	cmdPrefix := config.Config.General.CmdPrefix
	if sndTxt == cmdPrefix+"help" {
		PrintHelp()
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"search" || strings.HasPrefix(sndTxt, cmdPrefix+"search ") {
		query := strings.TrimSpace(strings.TrimPrefix(sndTxt, cmdPrefix+"search"))
		chatSearchMode = true
		chatSearchQuery = query
		renderChats(allChats, chatSearchQuery)
		app.SetFocus(treeView)
		syncScreen()
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"archived" {
		viewingArchived = !viewingArchived
		renderChats(allChats, "")
		if children := chatRoot.GetChildren(); len(children) > 0 {
			treeView.SetCurrentNode(children[0])
		} else {
			treeView.SetCurrentNode(chatRoot)
		}
		app.SetFocus(treeView)
		syncScreen()
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"commands" {
		PrintCommands()
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"quit" {
		sessionManager.CommandChannel <- messages.Command{"disconnect", nil}
		app.Stop()
		return
	}
	if strings.HasPrefix(sndTxt, cmdPrefix) {
		cmd := strings.TrimPrefix(sndTxt, cmdPrefix)
		var params []string
		if strings.Index(cmd, " ") >= 0 {
			cmdParts := strings.Split(cmd, " ")
			cmd = cmdParts[0]
			params = cmdParts[1:]
		}
		sessionManager.CommandChannel <- messages.Command{cmd, params}
		textInput.SetText("")
		return
	}
	if currentReceiver.Id == "" {
		PrintText("no receiver")
		textInput.SetText("")
		return
	}
	// no command, send as message (or reply if quoting)
	var msg messages.Command
	if replyTargetMsg != nil {
		msg = messages.Command{
			Name:   "reply",
			Params: []string{currentReceiver.Id, replyTargetMsg.Id, sndTxt},
		}
		clearReplyTarget()
	} else {
		msg = messages.Command{
			Name:   "send",
			Params: []string{currentReceiver.Id, sndTxt},
		}
	}
	sessionManager.CommandChannel <- msg
	textInput.SetText("")
}

// get the next message id to select (highlighted + offset)
func GetOffsetMsgId(curId string, offset int) string {
	if curRegions == nil || len(curRegions) == 0 {
		return ""
	}
	for idx, val := range curRegions {
		if val.Id == curId {
			arrPos := idx + offset
			if len(curRegions) > arrPos && arrPos >= 0 {
				return curRegions[arrPos].Id
			}
		}
	}
	if offset > 0 {
		return curRegions[0].Id
	} else {
		return curRegions[len(curRegions)-1].Id
	}
}

func syncScreen() {
	if app != nil {
		go app.Sync()
	}
}

// resets the selection in the textView and scrolls it down
func ResetMsgSelection() {
	if len(textView.GetHighlights()) > 0 {
		textView.Highlight("")
	}
	textView.ScrollToEnd()
	syncScreen()
}

// prints text to the TextView
func PrintText(txt string) {
	fmt.Fprintln(textView, txt)
}

// prints an error to the TextView
func PrintError(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(textView, "["+config.Config.Colors.Negative+"]", err.Error(), "[-]")
}

// prints an error to the TextView
func PrintErrorMsg(text string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(textView, "["+config.Config.Colors.Negative+"]", text, err.Error(), "[-]")
}

// prints an image or sticker attachment to the TextView
func PrintImage(path string) {
	showCmd := strings.TrimSpace(config.Config.General.ShowCommand)
	cmdParts := strings.Fields(showCmd)
	useBuiltin := showCmd == "" || showCmd == "builtin"
	if !useBuiltin && len(cmdParts) > 0 {
		if _, lookErr := exec.LookPath(cmdParts[0]); lookErr != nil {
			useBuiltin = true
		}
	}

	if useBuiltin {
		maxCols, maxRows := getFullFitImageSize()
		art, err := renderFileToTviewCached(path, maxCols, maxRows)
		if err != nil {
			PrintError(err)
			return
		}
		fmt.Fprintln(textView, art)
		syncScreen()
		return
	}

	var err error
	cmdParts = append(cmdParts, path)
	var cmd *exec.Cmd
	size := len(cmdParts)
	if size > 1 {
		cmd = exec.Command(cmdParts[0], cmdParts[1:]...)
	} else if size > 0 {
		cmd = exec.Command(cmdParts[0])
	}
	var stdout io.ReadCloser
	if stdout, err = cmd.StdoutPipe(); err == nil {
		if err = cmd.Start(); err == nil {
			reader := bufio.NewReader(stdout)
			io.Copy(tview.ANSIWriter(textView), reader)
			return
		}
	}
	PrintError(err)
}

// updates the status bar
func UpdateStatusBar(statusInfo messages.SessionStatus) {
	out := " "
	if statusInfo.Connected {
		out += "[" + config.Config.Colors.Positive + "]online[-]"
	} else {
		out += "[" + config.Config.Colors.Negative + "]offline[-]"
	}
	out += " "
	out += "[::d] ("
	out += fmt.Sprint(statusInfo.BatteryCharge)
	out += "%"
	if statusInfo.BatteryLoading {
		out += " [" + config.Config.Colors.Positive + "]L[-]"
	} else {
		out += " [" + config.Config.Colors.Negative + "]l[-]"
	}
	if statusInfo.BatteryPowersave {
		out += " [" + config.Config.Colors.Negative + "]S[-]"
	} else {
		out += " [" + config.Config.Colors.Positive + "]s[-]"
	}
	out += ")[::-] "
	out += statusInfo.LastSeen
	infoBar.SetText(out)
	//infoBar.SetText("🔋: ??%")
}

func getFullFitImageSize() (int, int) {
	maxCols, maxRows := 90, 32
	if textView != nil {
		_, _, w, h := textView.GetInnerRect()
		if w > 6 {
			maxCols = w - 4
		}
		if h > 4 {
			maxRows = h - 2
		}
	}
	if maxCols < 20 {
		maxCols = 20
	}
	if maxRows < 10 {
		maxRows = 10
	}
	return maxCols, maxRows
}

func toggleExpandImageMessage(msgID string) bool {
	var target *messages.Message
	for idx := range curRegions {
		if curRegions[idx].Id == msgID {
			target = &curRegions[idx]
			break
		}
	}
	if target == nil || (target.Kind != messages.MessageKindImage && target.Kind != messages.MessageKindSticker) {
		return false
	}

	if expandedImages[target.Id] {
		delete(expandedImages, target.Id)
	} else {
		expandedImages[target.Id] = true
		previewFile := filepath.Join(config.Config.General.PreviewPath, messages.DownloadFileName(*target))
		if _, err := os.Stat(previewFile); err != nil && sessionManager != nil && sessionManager.CommandChannel != nil {
			cmd := messages.Command{Name: "preview", Params: []string{target.Id}}
			select {
			case sessionManager.CommandChannel <- cmd:
			default:
				go func() {
					sessionManager.CommandChannel <- cmd
				}()
			}
		}
	}

	screen := getMessagesString(curRegions)
	textView.SetText(screen)
	textView.Highlight(target.Id)
	textView.ScrollToHighlight()
	syncScreen()
	return true
}

func attachTextViewMouseHandler() {
	textView.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		if action == tview.MouseLeftDoubleClick && event != nil {
			x, y := event.Position()
			if textView.InRect(x, y) {
				if hls := textView.GetHighlights(); len(hls) > 0 {
					if toggleExpandImageMessage(hls[0]) {
						return action, nil
					}
				}
			}
		}
		return action, event
	})
}

// sets the current chat, loads text from storage to TextView
func SetDisplayedChat(wid messages.Chat) {
	//TODO: how to get chat to set
	clearReplyTarget()
	expandedImages = make(map[string]bool)
	currentReceiver = wid
	textView.Clear()
	textView.SetTitle(wid.Name)
	sessionManager.CommandChannel <- messages.Command{"select", []string{currentReceiver.Id}}
}

// get a string representation of all messages for chat
func getMessagesString(msgs []messages.Message) string {
	out := ""
	for _, msg := range msgs {
		out += getTextMessageString(&msg)
		out += "\n"
	}
	return out
}

// create a formatted string with regions based on message ID from a text message
//TODO: optimize, use Sprintf etc
func getTextMessageString(msg *messages.Message) string {
	colorMe := config.Config.Colors.ChatMe
	colorContact := config.Config.Colors.ChatContact
	out := ""
	text := tview.Escape(msg.Text)
	if msg.Forwarded {
		text = "[" + config.Config.Colors.ForwardedText + "]" + text + "[-]"
	}
	tim := time.Unix(int64(msg.Timestamp), 0)
	time := tim.Format("02-01-06 15:04:05")
	out += "[\""
	out += msg.Id
	out += "\"]"
	if msg.QuotedText != "" {
		qPreview := strings.ReplaceAll(msg.QuotedText, "\n", " ")
		if runes := []rune(qPreview); len(runes) > 80 {
			qPreview = string(runes[:77]) + "..."
		}
		qSender := msg.QuotedSender
		if qSender == "" {
			qSender = "message"
		}
		out += "[-::d]  ↱ " + tview.Escape(qSender) + ": " + tview.Escape(qPreview) + "[-::-]\n"
	}
	if msg.FromMe { //msg from me
		out += "[-::d](" + time + ") [" + colorMe + "::b]Me: [-::-]" + text
	} else { // message from others
		out += "[-::d](" + time + ") [" + colorContact + "::b]" + msg.ContactShort + ": [-::-]" + text
	}
	if config.Config.General.InlineImages {
		expanded := expandedImages[msg.Id]
		if msg.Kind == messages.MessageKindImage {
			maxCols, maxRows := 34, 12
			if expanded {
				maxCols, maxRows = getFullFitImageSize()
			}
			var art string
			var err error
			previewFile := filepath.Join(config.Config.General.PreviewPath, messages.DownloadFileName(*msg))
			if expanded {
				art, err = renderFileToTviewCached(previewFile, maxCols, maxRows)
			}
			if (!expanded || err != nil) && msg.RawMessage != nil {
				if thumb := msg.RawMessage.GetImageMessage().GetJPEGThumbnail(); len(thumb) > 0 {
					art, err = renderBytesToTviewCached("thumb:"+msg.Id, thumb, maxCols, maxRows)
				} else if !expanded {
					art, err = renderFileToTviewCached(previewFile, maxCols, maxRows)
				}
			} else if !expanded && err == nil && art == "" {
				art, err = renderFileToTviewCached(previewFile, maxCols, maxRows)
			}
			if err == nil && art != "" {
				out += "\n" + indentBlock(art, "  ")
			}
		} else if msg.Kind == messages.MessageKindSticker {
			maxCols, maxRows := 24, 12
			if expanded {
				maxCols, maxRows = getFullFitImageSize()
			}
			previewFile := filepath.Join(config.Config.General.PreviewPath, messages.DownloadFileName(*msg))
			if art, err := renderFileToTviewCached(previewFile, maxCols, maxRows); err == nil && art != "" {
				out += "\n" + indentBlock(art, "  ")
			}
		}
	}
	out += "[\"\"]"
	return out
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

type UiHandler struct{}

func (u UiHandler) NewMessage(msg messages.Message) {
	//TODO: its stupid to "go" this as its supposed to run
	//on the ui thread anyway. But QueueUpdate blocks...?
	go app.QueueUpdateDraw(func() {
		curRegions = append(curRegions, msg)
		PrintText(getTextMessageString(&msg))
	})
}

func (u UiHandler) NewScreen(msgs []messages.Message) {
	go app.QueueUpdateDraw(func() {
		textView.Clear()
		screen := getMessagesString(msgs)
		textView.SetText(screen)
		curRegions = msgs
		if screen == "" {
			if currentReceiver.Id == "" {
				PrintHelp()
			} else {
				PrintText("[::d] ~~~ no messages, press " + config.Config.Keymap.CommandBacklog + " to load backlog if available ~~~[::-]")
			}
		}
		syncScreen()
	})
}

func renderChats(ids []messages.Chat, query string) {
	title := "Chats"
	if viewingArchived {
		title = "Archived"
	}
	if chatSearchMode {
		chatRoot.SetText(title + " (/" + query + ")")
	} else {
		chatRoot.SetText(title)
	}
	chatRoot.ClearChildren()
	oldId := currentReceiver.Id
	q := strings.ToLower(query)

	archivedCount := 0
	for _, element := range ids {
		if element.Archived {
			archivedCount++
		}
	}

	if q == "" {
		if !viewingArchived && archivedCount > 0 {
			archNode := tview.NewTreeNode(fmt.Sprintf("Archived (%d)", archivedCount)).
				SetReference(navOpenArchived).
				SetSelectable(true).
				SetColor(tcell.ColorNames[config.Config.Colors.ListHeader])
			chatRoot.AddChild(archNode)
		} else if viewingArchived {
			backNode := tview.NewTreeNode("<- Back to Chats").
				SetReference(navBackToChats).
				SetSelectable(true).
				SetColor(tcell.ColorNames[config.Config.Colors.ListHeader])
			chatRoot.AddChild(backNode)
		}
	}

	var firstMatch *tview.TreeNode
	for _, element := range ids {
		if q == "" {
			if viewingArchived && !element.Archived {
				continue
			}
			if !viewingArchived && element.Archived {
				continue
			}
		}

		name := element.Name
		if name == "" {
			name = strings.TrimSuffix(strings.TrimSuffix(element.Id, messages.GROUPSUFFIX), messages.CONTACTSUFFIX)
		}
		if q != "" && !strings.Contains(strings.ToLower(name), q) && !strings.Contains(strings.ToLower(element.Id), q) {
			continue
		}

		label := name
		if element.Pinned && !element.Archived {
			label = "(P) " + label
		}
		if element.IsMuted() {
			label += " (M)"
		}
		if q != "" && element.Archived {
			label += " (A)"
		}
		if element.Unread > 0 {
			label += " ([" + config.Config.Colors.UnreadCount + "]" + fmt.Sprint(element.Unread) + "[-])"
		}
		node := tview.NewTreeNode(label).
			SetReference(element).
			SetSelectable(true)
		if element.IsGroup {
			node.SetColor(tcell.ColorNames[config.Config.Colors.ListGroup])
		} else {
			node.SetColor(tcell.ColorNames[config.Config.Colors.ListContact])
		}
		if element.Id == oldId {
			currentReceiver = element
		}
		chatRoot.AddChild(node)
		if firstMatch == nil {
			firstMatch = node
		}
		if element.Id == currentReceiver.Id && q == "" {
			treeView.SetCurrentNode(node)
		}
	}
	if q != "" && firstMatch != nil {
		treeView.SetCurrentNode(firstMatch)
	}
}

// loads the chat data from storage to the TreeView
func (u UiHandler) SetChats(ids []messages.Chat) {
	go app.QueueUpdateDraw(func() {
		allChats = ids
		renderChats(allChats, chatSearchQuery)
	})
}

func (u UiHandler) PrintError(err error) {
	PrintError(err)
}

func (u UiHandler) PrintText(msg string) {
	PrintText(msg)
}

func (u UiHandler) PrintFile(path string) {
	go app.QueueUpdateDraw(func() {
		PrintImage(path)
	})
}

func (u UiHandler) OpenFile(path string) {
	open.Run(path)
}

func (u UiHandler) SetStatus(status messages.SessionStatus) {
	go app.QueueUpdateDraw(func() {
		UpdateStatusBar(status)
	})
}

func (u UiHandler) GetWriter() io.Writer {
	return textView
}
