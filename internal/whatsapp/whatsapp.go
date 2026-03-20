package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	apptypes "next/app/types"
	"next/internal/config"
	"next/internal/logger"
)

// WhatsApp manages the whatsmeow connection.
type WhatsApp struct {
	mu             sync.RWMutex
	client         *whatsmeow.Client
	container      *sqlstore.Container
	handler        func(chatID, text, pushName string)
	receiptHandler func(waMsgID string)
	qrCode         string
	logger         *logger.Logger
	cfg            *config.Config
	sentIDs        sync.Map // tracks message IDs sent by the bot to avoid loops
}

func NewWhatsApp(dbPath string, l *logger.Logger, cfg *config.Config) (*WhatsApp, error) {
	// Identify as "Next" instead of "whatsmeow" to the WhatsApp servers
	store.DeviceProps.Os = proto.String("Next")

	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New(context.Background(), "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=1&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath), dbLog)
	if err != nil {
		return nil, fmt.Errorf("whatsapp store: %w", err)
	}
	return &WhatsApp{container: container, logger: l, cfg: cfg}, nil
}

// OnMessage sets the callback for incoming private text messages.
func (w *WhatsApp) OnMessage(handler func(chatID, text, pushName string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handler = handler
}

// OnReceipt sets the callback for read receipt events (called with the WA message ID).
func (w *WhatsApp) OnReceipt(handler func(waMsgID string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.receiptHandler = handler
}

// Connect starts the WhatsApp client. Returns a channel that emits QR code strings
// if pairing is needed, or nil if already paired.
func (w *WhatsApp) Connect() (<-chan string, error) {
	deviceStore, err := w.container.GetFirstDevice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, waLog.Noop)
	w.mu.Lock()
	w.client = client
	w.mu.Unlock()

	// Register event handler
	client.AddEventHandler(func(evt interface{}) {
		w.handleEvent(evt)
	})

	qrChan := make(chan string, 10)

	if client.Store.ID == nil {
		// Need to pair - get QR code
		qrChanInternal, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			return nil, fmt.Errorf("whatsapp connect: %w", err)
		}

		go func() {
			for evt := range qrChanInternal {
				if evt.Event == "code" {
					w.mu.Lock()
					w.qrCode = evt.Code
					w.mu.Unlock()
					qrChan <- evt.Code
				} else if evt.Event == "success" {
					w.mu.Lock()
					w.qrCode = ""
					w.mu.Unlock()
					close(qrChan)
					return
				} else if evt.Event == "timeout" {
					w.mu.Lock()
					w.qrCode = ""
					w.mu.Unlock()
					close(qrChan)
					return
				}
			}
		}()
		return qrChan, nil
	}

	// Already paired
	err = client.Connect()
	if err != nil {
		return nil, fmt.Errorf("whatsapp connect: %w", err)
	}
	close(qrChan)
	return qrChan, nil
}

func (w *WhatsApp) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		w.handleMessage(v)
	case *events.Connected:
		if w.logger != nil {
			botPhone := ""
			w.mu.RLock()
			if w.client != nil && w.client.Store.ID != nil {
				botPhone = w.client.Store.ID.User
			}
			w.mu.RUnlock()
			w.logger.Log("whatsapp_connected", "", map[string]any{"phone_number": botPhone})
		}
	case *events.Disconnected:
		if w.logger != nil {
			w.logger.Log("whatsapp_disconnected", "", map[string]any{"reason": "disconnected"})
		}
	case *events.Receipt:
		if v.Type == types.ReceiptTypeRead || v.Type == types.ReceiptTypeReadSelf {
			w.mu.RLock()
			handler := w.receiptHandler
			w.mu.RUnlock()
			if handler != nil {
				for _, id := range v.MessageIDs {
					handler(string(id))
				}
			}
		}
	}
}

func (w *WhatsApp) handleMessage(msg *events.Message) {
	// Ignore status broadcasts
	if msg.Info.Chat.Server == "broadcast" {
		return
	}

	// Group messages
	if msg.Info.IsGroup {
		w.cfg.Mu.RLock()
		groupsEnabled := w.cfg.GroupsEnabled
		groupList := w.cfg.GroupList
		w.cfg.Mu.RUnlock()

		if !groupsEnabled {
			return
		}

		groupJID := msg.Info.Chat.String()
		if groupList != "" && !isGroupAllowed(groupJID, groupList) {
			return
		}

		// Bot's own automated replies — ignore
		if msg.Info.IsFromMe {
			if _, sent := w.sentIDs.Load(msg.Info.ID); sent {
				return
			}
		}

		text := extractText(msg)
		if text == "" {
			return
		}

		// Mark message as read
		w.mu.RLock()
		client := w.client
		handler := w.handler
		w.mu.RUnlock()
		if client != nil {
			client.MarkRead(context.Background(), []types.MessageID{msg.Info.ID}, time.Now(), msg.Info.Chat, msg.Info.Sender)
		}
		if handler != nil {
			handler(groupJID, text, msg.Info.PushName)
		}
		return
	}

	// Private messages
	// Ignore bot's own replies (but allow owner typing manually)
	if msg.Info.IsFromMe {
		if _, sent := w.sentIDs.Load(msg.Info.ID); sent {
			return
		}
	}

	// Response mode: filter private messages
	w.cfg.Mu.RLock()
	responseMode := w.cfg.ResponseMode
	w.cfg.Mu.RUnlock()

	switch responseMode {
	case "owner":
		if !msg.Info.IsFromMe {
			return
		}
	case "contacts":
		if msg.Info.IsFromMe {
			return
		}
	}

	// Extract text content
	text := extractText(msg)
	if text == "" {
		return
	}

	// Resolve phone number: Chat may be a LID (Linked ID), not a phone number.
	chatID := ""
	if msg.Info.Chat.Server == types.DefaultUserServer {
		chatID = msg.Info.Chat.User
	} else if !msg.Info.SenderAlt.IsEmpty() && msg.Info.SenderAlt.Server == types.DefaultUserServer {
		chatID = msg.Info.SenderAlt.User
	} else {
		// Resolve LID → phone via store
		w.mu.RLock()
		client := w.client
		w.mu.RUnlock()
		if client != nil && client.Store.LIDs != nil {
			pn, err := client.Store.LIDs.GetPNForLID(context.Background(), msg.Info.Chat)
			if err == nil && !pn.IsEmpty() {
				chatID = pn.User
			}
		}
	}
	if chatID == "" {
		return
	}

	// Mark message as read
	w.mu.RLock()
	client := w.client
	handler := w.handler
	w.mu.RUnlock()
	if client != nil {
		client.MarkRead(context.Background(), []types.MessageID{msg.Info.ID}, time.Now(), msg.Info.Chat, msg.Info.Sender)
	}

	if handler != nil {
		handler(chatID, text, msg.Info.PushName)
	}
}

// isGroupAllowed checks if a group JID is in the comma-separated allow list.
func isGroupAllowed(jid, list string) bool {
	for _, g := range strings.Split(list, ",") {
		if strings.TrimSpace(g) == jid {
			return true
		}
	}
	return false
}

// resolveJID returns the correct JID for a phone number or group.
func resolveJID(id string) types.JID {
	if strings.Contains(id, "@g.us") {
		parts := strings.SplitN(id, "@", 2)
		return types.NewJID(parts[0], types.GroupServer)
	}
	return types.NewJID(id, types.DefaultUserServer)
}

func extractText(msg *events.Message) string {
	if msg.Message == nil {
		return ""
	}
	// Regular text message
	if msg.Message.GetConversation() != "" {
		return msg.Message.GetConversation()
	}
	// Extended text message (with link preview, etc.)
	if msg.Message.GetExtendedTextMessage() != nil {
		return msg.Message.GetExtendedTextMessage().GetText()
	}
	return ""
}

// IsConnected returns whether the WhatsApp client is authenticated and connected.
func (w *WhatsApp) IsConnected() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.client != nil && w.client.IsLoggedIn()
}

// GetPhoneNumber returns the connected phone number.
func (w *WhatsApp) GetPhoneNumber() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.client != nil && w.client.Store.ID != nil {
		return w.client.Store.ID.User
	}
	return ""
}

// GetQRCode returns the current QR code string (empty if connected).
func (w *WhatsApp) GetQRCode() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.qrCode
}

// Reconnect disconnects and reconnects to generate a new QR code.
func (w *WhatsApp) Reconnect(l *logger.Logger) {
	w.mu.Lock()
	if w.client != nil {
		w.client.Disconnect()
		if w.client.Store != nil {
			w.client.Store.Delete(context.Background())
		}
		w.client = nil
	}
	w.qrCode = ""
	w.mu.Unlock()

	go func() {
		if _, err := w.Connect(); err != nil && l != nil {
			l.Log("error", "", map[string]any{"source": "whatsapp", "message": "reconnect: " + err.Error()})
		}
	}()
}

// SendComposing sends a "typing..." presence to a phone number or group.
func (w *WhatsApp) SendComposing(chatID string) {
	w.mu.RLock()
	client := w.client
	w.mu.RUnlock()
	if client == nil {
		return
	}
	jid := resolveJID(chatID)
	_ = client.SendChatPresence(context.Background(), jid, types.ChatPresenceComposing, "")
}

// SendPaused stops the "typing..." presence.
func (w *WhatsApp) SendPaused(chatID string) {
	w.mu.RLock()
	client := w.client
	w.mu.RUnlock()
	if client == nil {
		return
	}
	jid := resolveJID(chatID)
	_ = client.SendChatPresence(context.Background(), jid, types.ChatPresencePaused, "")
}

// SendText sends a text message to a phone number or group.
// Returns the WhatsApp message ID on success (used for read receipt tracking).
func (w *WhatsApp) SendText(chatID, text string) (string, error) {
	w.mu.RLock()
	client := w.client
	w.mu.RUnlock()

	if client == nil {
		return "", fmt.Errorf("whatsapp not connected")
	}

	jid := resolveJID(chatID)
	resp, err := client.SendMessage(context.Background(), jid, &waE2E.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		return "", err
	}
	// Track sent message ID to avoid loop on self-chat
	w.sentIDs.Store(resp.ID, true)
	time.AfterFunc(30*time.Second, func() {
		w.sentIDs.Delete(resp.ID)
	})
	return resp.ID, nil
}

// GetGroups returns the list of WhatsApp groups the bot is participating in.
func (w *WhatsApp) GetGroups() ([]apptypes.GroupBasic, error) {
	w.mu.RLock()
	client := w.client
	w.mu.RUnlock()
	if client == nil || !client.IsLoggedIn() {
		return nil, fmt.Errorf("whatsapp not connected")
	}

	groups, err := client.GetJoinedGroups(context.Background())
	if err != nil {
		return nil, err
	}

	result := make([]apptypes.GroupBasic, 0, len(groups))
	for _, g := range groups {
		result = append(result, apptypes.GroupBasic{
			JID:  g.JID.String(),
			Name: g.Name,
		})
	}
	return result, nil
}

// Disconnect cleanly disconnects the WhatsApp client (keeps session for reconnect).
func (w *WhatsApp) Disconnect() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil {
		w.client.Disconnect()
	}
}

// Logout disconnects and removes the session (unlinks from the phone app too).
func (w *WhatsApp) Logout() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return nil
	}
	err := w.client.Logout(context.Background())
	if err != nil {
		// Fallback: at least disconnect and delete local store
		w.client.Disconnect()
		if w.client.Store != nil {
			_ = w.client.Store.Delete(context.Background())
		}
	}
	w.client = nil
	w.qrCode = ""
	return err
}
