// Package gotirc contains functions for connecting to Twitch.tv chat via IRC
package gotirc

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

const sendBufferSize = 512

var caps = []string{"membership", "commands", "tags"}

// Options facilitates passing desired settings to a new Client
type Options struct {
	Debug    bool
	Port     int
	Host     string
	Channels []string
}

// Client holds state and context information to maintain a connection with a server
type Client struct {
	options Options

	sendQueue   chan string
	recvChannel chan Message
	reader      *bufio.Reader
	writer      *bufio.Writer

	conn        net.Conn
	readTimeout time.Duration
	connectedMu sync.RWMutex
	connected   bool
	doneChan    chan struct{}

	callbackMu            sync.Mutex
	actionCallbacks       []func(channel string, tags map[string]string, msg string)
	chatCallbacks         []func(channel string, tags map[string]string, msg string)
	resubCallbacks        []func(channel string, tags map[string]string, msg string)
	noticeCallbacks       []func(msg string)
	userStateCallbacks    []func(channel string, tags map[string]string)
	roomStateCallbacks    []func(channel string, tags map[string]string)
	subGiftCallbacks      []func(channel string, tags map[string]string, msg string)
	subscriptionCallbacks []func(channel string, tags map[string]string, msg string)
	cheerCallbacks        []func(channel string, tags map[string]string, msg string)
	joinCallbacks         []func(channel, username string)
	partCallbacks         []func(channel, username string)
	whisperCallbacks      []func(username string, tags map[string]string, msg string)
}

// NewClient returns a new Client
func NewClient(o Options) *Client {
	return &Client{
		options:     o,
		readTimeout: 10 * time.Minute,
	}
}

// Connect connects the client to the server specified in the options and uses
// the supplied nick and pass (oauth token) to authenticate. Connect blocks and
// runs event callbacks until disconnected
func (c *Client) Connect(nick string, pass string) error {
	conn, err := c.doConnect(func() (net.Conn, error) {
		return net.Dial("tcp", fmt.Sprintf("%s:%d", c.options.Host, c.options.Port))
	})

	if err != nil {
		return err
	}

	return c.doPostConnect(nick, pass, conn, 19, 30)
}

func (c *Client) doConnect(connFactory func() (net.Conn, error)) (net.Conn, error) {
	c.connectedMu.Lock()
	defer c.connectedMu.Unlock()

	if c.connected {
		return nil, errors.New("Already connected")
	}

	conn, err := connFactory()
	if err == nil {
		c.connected = true
	}

	return conn, err
}

// Disconnect closes the client's connection with the server
func (c *Client) Disconnect() {
	c.connectedMu.Lock()
	defer c.connectedMu.Unlock()
	if c.connected {
		c.connected = false
		close(c.doneChan)
	}
}

// Connected returns true if the client is currently connected to the server,
// false otherwise
func (c *Client) Connected() bool {
	c.connectedMu.RLock()
	defer c.connectedMu.RUnlock()
	return c.connected
}

func (c *Client) doPostConnect(nick, pass string, conn net.Conn, maxMessages, perSeconds float64) error {
	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.writer = bufio.NewWriter(conn)
	c.doneChan = make(chan struct{})
	c.sendQueue = make(chan string, sendBufferSize)
	defer close(c.sendQueue)

	if err := c.authenticate(nick, pass); err != nil {
		return err
	}

	for _, channel := range c.options.Channels {
		c.Join(channel)
	}

	go c.startSendLoop(maxMessages, perSeconds)
	return c.startRecvLoop()
}

// Say sends a message to a channel
func (c *Client) Say(channel string, msg string) {
	if channel[0] != '#' {
		channel = "#" + channel
	}
	c.send(fmt.Sprintf("PRIVMSG %s :%s", channel, msg))
}

// Whisper sends a whisper to a user
func (c *Client) Whisper(user string, msg string) {
	c.Say("#jtv", "/w "+user+" "+msg)
}

// OnAction adds an event callback for action (e.g., /me) messages
func (c *Client) OnAction(callback func(channel string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.actionCallbacks = append(c.actionCallbacks, callback)
}

func (c *Client) OnUserState(callback func(channel string, tags map[string]string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.userStateCallbacks = append(c.userStateCallbacks, callback)
}

func (c *Client) OnRoomState(callback func(channel string, tags map[string]string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.roomStateCallbacks = append(c.roomStateCallbacks, callback)
}

// OnNotice adds an event callback for NOTICE server messages
func (c *Client) OnNotice(callback func(msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.noticeCallbacks = append(c.noticeCallbacks, callback)
}

// OnChat adds an event callback for when a user sends a message in a channel
func (c *Client) OnChat(callback func(channel string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.chatCallbacks = append(c.chatCallbacks, callback)
}

// OnResub adds an event callback for when a user resubs to a channel
func (c *Client) OnResub(callback func(channel string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.resubCallbacks = append(c.resubCallbacks, callback)
}

// OnSubscription adds an event callback for when a user subscribes to a channel
func (c *Client) OnSubscription(callback func(channel string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.subscriptionCallbacks = append(c.subscriptionCallbacks, callback)
}

// OnSubGift adds an event callback for when a user gifts a sub to a user in a channel
func (c *Client) OnSubGift(callback func(channel string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.subGiftCallbacks = append(c.subGiftCallbacks, callback)
}

// OnCheer adds an event callback for when a user cheers bits in a channel
func (c *Client) OnCheer(callback func(channel string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.cheerCallbacks = append(c.cheerCallbacks, callback)
}

// OnJoin adds an event callback for when a user joins a channel
func (c *Client) OnJoin(callback func(channel, username string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.joinCallbacks = append(c.joinCallbacks, callback)
}

// OnPart adds an event callback for when a user parts a channel
func (c *Client) OnPart(callback func(channel, username string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.partCallbacks = append(c.partCallbacks, callback)
}

// OnWhisper adds an event callback for when a user whispers to us
func (c *Client) OnWhisper(callback func(username string, tags map[string]string, msg string)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.whisperCallbacks = append(c.whisperCallbacks, callback)
}

// Join tells the client to join a particular channel. If the "#" prefix is missing,
// it is automatically prepended.
func (c *Client) Join(channel string) {
	if !strings.HasPrefix(channel, "#") {
		channel = "#" + channel
	}
	c.send("JOIN %s", channel)
}

// Part tells the client to part a particular channel. If the "#" prefix is missing,
// it is automatically prepended.
func (c *Client) Part(channel string) {
	if !strings.HasPrefix(channel, "#") {
		channel = "#" + channel
	}
	c.send("PART %s", channel)
}

func (c *Client) authenticate(nick, pass string) error {
	if err := c.write(fmt.Sprintf("PASS %s\r\nNICK %s\r\n", pass, nick)); err != nil {
		return err
	}

	line, err := c.read()
	if err != nil {
		return err
	}

	msg := NewMessage(line)

	if msg.Command != "001" {
		c.doCallbacks(line)
		return fmt.Errorf("Unexpected server response: %s", line)
	}

	err = c.write(fmt.Sprintf("CAP REQ :%s\r\n", strings.Join(caps, " twitch.tv/")))
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) send(format string, args ...interface{}) {
	if !c.Connected() {
		return
	}

	msg := fmt.Sprintf(format, args...)
	select {
	case c.sendQueue <- msg:
	default:
		c.log("Send queue full; discarding message: %s", msg)
	}
}

func (c *Client) write(data string) error {
	c.log("< %s", data)
	c.conn.SetWriteDeadline(time.Now().Add(1 * time.Minute))
	_, err := c.writer.WriteString(data)
	if err != nil {
		return err
	}
	return c.writer.Flush()
}

func (c *Client) read() (string, error) {
	line, err := c.reader.ReadString('\n')
	c.log("> %s", line)
	return line, err
}

func (c *Client) log(format string, v ...interface{}) {
	if c.options.Debug {
		log.Printf(format, v...)
	}
}

func (c *Client) startSendLoop(maxMessages, perSeconds float64) {
	defer c.conn.Close()
	tokens := maxMessages
	lastTick := time.Now()

	for {
		select {
		case <-c.doneChan:
			return
		case data := <-c.sendQueue:
			if !strings.HasSuffix(data, "\r\n") {
				data = data + "\r\n"
			}

			now := time.Now()
			elapsedTime := now.Sub(lastTick)
			lastTick = now
			tokens += elapsedTime.Seconds() * (maxMessages / perSeconds)

			if tokens >= maxMessages {
				tokens = maxMessages
			} else if tokens < 1 {
				required := 1 - tokens
				time.Sleep(time.Duration(required * float64(time.Second)))
			}

			if err := c.write(data); err != nil {
				c.log("ERROR sending: %s", err)
				c.Disconnect()
				return
			}

			tokens--
		}
	}
}

func (c *Client) startRecvLoop() error {
	for {
		c.conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		line, err := c.reader.ReadString('\n')
		if err != nil {
			c.Disconnect()
			return err
		}
		c.log("> %s", line)
		c.doCallbacks(line)
	}
}

func (c *Client) doCallbacks(line string) {
	msg := NewMessage(line)

	switch msg.Command {
	case "PRIVMSG":
		var m string

		if len(msg.Params) > 1 {
			m = msg.Params[1]
		}

		if strings.HasPrefix(m, "\u0001ACTION") {
			c.doActionCallbacks(&msg)
		} else {
			if _, cheered := msg.Tags["bits"]; cheered {
				c.doCheerCallbacks(&msg)
			} else {
				c.doChatCallbacks(&msg)
			}
		}
		break

	case "JOIN":
		c.doJoinCallbacks(&msg)
		break

	case "PART":
		c.doPartCallbacks(&msg)
		break

	case "NOTICE":
		c.doNoticeCallbacks(&msg)
		break

	case "USERSTATE":
		c.doUserStateCallbacks(&msg)
		break

	case "ROOMSTATE":
		c.doRoomStateCallbacks(&msg)
		break

	case "USERNOTICE":
		msgid := msg.Tags["msg-id"]
		if msgid == "resub" {
			c.doResubCallbacks(&msg)
		} else if msgid == "sub" {
			c.doSubscriptionCallbacks(&msg)
		} else if msgid == "subgift" {
			c.doSubGiftCallbacks(&msg)
		}
		break

	case "WHISPER":
		c.doWhisperCallbacks(&msg)
		break

	case "PING":
		c.send(fmt.Sprintf("PONG :%s", msg.Params[0]))
		break

	}

}

func (c *Client) doUserStateCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.userStateCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Tags)
	}
}

func (c *Client) doRoomStateCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.roomStateCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Tags)
	}
}

func (c *Client) doNoticeCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.noticeCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		m := ""
		if len(msg.Params) > 1 {
			m = msg.Params[1]
		}
		cb(m)
	}
}

func (c *Client) doResubCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.resubCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		m := ""
		if len(msg.Params) > 1 {
			m = msg.Params[1]
		}
		cb(msg.Params[0], msg.Tags, m)
	}
}

func (c *Client) doSubscriptionCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.subscriptionCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		m := ""
		if len(msg.Params) > 1 {
			m = msg.Params[1]
		}
		cb(msg.Params[0], msg.Tags, m)
	}
}

func (c *Client) doSubGiftCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.subGiftCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		m := ""
		if len(msg.Params) > 1 {
			m = msg.Params[1]
		}
		cb(msg.Params[0], msg.Tags, m)
	}
}

func (c *Client) doCheerCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.cheerCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Tags, msg.Params[1])
	}
}

func (c *Client) doActionCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.actionCallbacks
	c.callbackMu.Unlock()

	m := msg.Params[1]
	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Tags, m[7:])
	}
}

func (c *Client) doChatCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.chatCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Tags, msg.Params[1])
	}
}

func (c *Client) doJoinCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.joinCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Prefix.Nick)
	}
}

func (c *Client) doPartCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.partCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Params[0], msg.Prefix.Nick)
	}
}

func (c *Client) doWhisperCallbacks(msg *Message) {
	c.callbackMu.Lock()
	callbacks := c.whisperCallbacks
	c.callbackMu.Unlock()

	for _, cb := range callbacks {
		cb(msg.Prefix.Nick, msg.Tags, msg.Params[1])
	}
}
