package cannula

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sorcix/irc"
	"golang.org/x/oauth2"
	"google.golang.org/api/youtube/v3"
)

var versionString = "v0.2"
var startTime = time.Now()

// These lines contain zero width spaces.
var YTToIRC = strings.NewReplacer(" ", " ", "!", "❢", "@", "᪤", "+", "​+", "&", "​&", "%", "​%", ":", "：")
var IRCToYT = strings.NewReplacer(" ", " ", "❢", "!", "᪤", "@", "​+", "+", "​&", "&", "​%", "%", "：", ":")

type ServerClose struct {
}

type Cannula struct {
	sync.RWMutex

	config  *oauth2.Config
	token   *oauth2.Token
	tokens  map[string]*oauth2.Token
	service *youtube.Service

	listener net.Listener

	prefix *irc.Prefix

	in        chan interface{}
	clients   map[*irc.Prefix]*Client
	ytClients map[string]*YTClient

	names    map[string]*Client
	channels map[string]*Channel

	rateLimit chan func()
}

func New() (*Cannula, error) {
	c := &Cannula{}

	err := c.ytAuth()
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Cannula) handleConn(conn net.Conn) error {
	cl := NewClient(&irc.Prefix{}, conn, c.in)

	c.Lock()
	defer c.Unlock()

	c.clients[cl.Prefix] = cl

	go cl.handle()

	return nil
}

func (c *Cannula) handle() {
	for i := range c.in {
		switch i := i.(type) {
		case *ClientMessage:
			c.handleMessage(i.Message)
		}
	}
}

func (c *Cannula) handleMessage(m *irc.Message) {
	c.Lock()
	defer c.Unlock()

	cl := c.clients[m.Prefix]
	if cl == nil {
		return
	}

	switch m.Command {
	case irc.QUIT:
		c.quit(cl, m)
	case irc.PASS:
		c.pass(cl, m)
	case irc.NICK:
		c.nick(cl, m)
	case irc.USER:
		c.user(cl, m)
	case irc.JOIN:
		c.join(cl, m)
	case irc.PART:
		c.part(cl, m)
	case irc.PRIVMSG:
		c.privmsg(cl, m)
	case irc.NOTICE:
		c.notice(cl, m)
	case irc.PING:
		c.ping(cl, m)
	}
}

func (c *Cannula) verifyChannelTarget(cl *Client, m *irc.Message, command string) string {
	if !cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTREGISTERED, []string{}, "You have not registered", false}
		return ""
	}

	if len(m.Params) == 0 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NEEDMOREPARAMS, []string{command}, "Not enough parameters", false}
		return ""
	}

	verifiedTargets := []string{}

	target := m.Params[0]

	targets := strings.Split(target, ",")

	for _, target := range targets {
		if strings.Index(target, "#") != 0 {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHCHANNEL, []string{target}, "No such channel", false}
			continue
		}

		if c.channels[target] == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHCHANNEL, []string{target}, "No such channel", false}
			continue
		}

		if c.channels[target].clients[cl] == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOTONCHANNEL, []string{target}, "You're not on that channel", false}
			continue
		}

		verifiedTargets = append(verifiedTargets, target)
	}

	return strings.Join(verifiedTargets, ",")
}

func (c *Cannula) verifyTarget(cl *Client, m *irc.Message, command string) string {
	if !cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTREGISTERED, []string{}, "You have not registered", false}
		return ""
	}

	if len(m.Params) == 0 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NORECIPIENT, []string{command}, "No recipient given", false}
		return ""
	}

	verifiedTargets := []string{}

	target := m.Params[0]

	targets := strings.Split(target, ",")

	for _, target := range targets {
		if c.names[target] == nil && c.channels[target] == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHNICK, []string{target}, "No such nick/channel", false}
			continue
		}

		if strings.Index(target, "#") == 0 && c.channels[target].clients[cl] == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOTONCHANNEL, []string{target}, "You're not on that channel", false}
			continue
		}

		verifiedTargets = append(verifiedTargets, target)
	}

	return strings.Join(verifiedTargets, ",")
}

func (c *Cannula) quit(cl *Client, m *irc.Message) {
	delete(c.clients, cl.Prefix)
	delete(c.names, cl.Prefix.Name)

	for ch := range cl.Channels {
		c.channels[ch].Quit(c, cl, m)
	}

	cl.in <- &ServerClose{}
}

func (c *Cannula) checkAuth(cl *Client) {
	if cl.Prefix.User == "" || cl.Prefix.Host == "" || cl.Prefix.Name == "" {
		return
	}

	firstTime := false

	if cl.Pass != "" {
		if c.tokens[cl.Pass] == nil {
			tok, err := c.config.Exchange(oauth2.NoContext, cl.Pass)
			if err != nil {
				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{cl.Prefix.Name}, fmt.Sprintf("Error linking YouTube account. %s", strings.Replace(err.Error(), "\n", "", -1)), false}
			} else {
				c.tokens[cl.Pass] = tok

				c.ytSaveTokens()

				firstTime = true
			}
		}

		if c.tokens[cl.Pass] != nil {
			cl.Service, _ = c.ytCreateService(c.tokens[cl.Pass])
		}
	}

	if cl.Service == nil {
		cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{cl.Prefix.Name}, fmt.Sprintf("You have not linked your YouTube account properly. Visit: %s and follow the instructions to link your account.", c.ytGenerateAuthURL()), false}
	} else {
		res, err := cl.Service.Channels.List("id,snippet").Mine(true).Do()
		if err != nil || len(res.Items) == 0 {
			if err == nil {
				err = errors.New("Could not find your YouTube Channel, have you created one?")
			}
			cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{cl.Prefix.Name}, fmt.Sprintf("Error linking YouTube account. %s", strings.Replace(err.Error(), "\n", "", -1)), false}
			cl.Service = nil
		} else {
			ytc := res.Items[0]
			ytcl := c.ytClient(ytc.Snippet.Title, ytc.Id)

			old := cl.Prefix.Name

			delete(c.clients, cl.Prefix)
			delete(c.names, cl.Prefix.Name)

			if c.names[ytcl.Prefix.Name] != nil {

				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{cl.Prefix.Name}, "You are already logged in.", false}
				cl.in <- &ServerClose{}
				return
			}

			cl.Prefix = ytcl.Prefix
			cl.YTClient = ytcl

			c.clients[cl.Prefix] = cl
			c.names[cl.Prefix.Name] = cl

			cl.in <- &irc.Message{c.prefix, irc.NICK, []string{old}, cl.Prefix.Name, false}

			if firstTime {
				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{cl.Prefix.Name}, "YouTube account linked!", false}
			}
		}
	}

	cl.Authorized = true

	cl.in <- &irc.Message{c.prefix, irc.RPL_WELCOME, []string{cl.Prefix.Name}, fmt.Sprintf("Welcome to the IRC Network %s", cl.Prefix), false}
	cl.in <- &irc.Message{c.prefix, irc.RPL_YOURHOST, []string{cl.Prefix.Name}, fmt.Sprintf("Your host is %s running Cannula %s", c.prefix.Name, versionString), false}
	cl.in <- &irc.Message{c.prefix, irc.RPL_CREATED, []string{cl.Prefix.Name}, fmt.Sprintf("This server was created %s at %s", startTime.Format("Mon Jan 2 2006"), startTime.Format("15:04:05 MST")), false}
	cl.in <- &irc.Message{c.prefix, irc.RPL_BOUNCE, []string{cl.Prefix.Name}, "PREFIX=&@\\%+ STATUSMSG=&@\\%+ CHANTYPES=# CHANMODES=,,,m :are supported on this server", false}

	cl.in <- &irc.Message{c.prefix, irc.RPL_MYINFO, []string{cl.Prefix.Name}, fmt.Sprintf("%s %s", c.prefix.Name, versionString), false}
}

func (c *Cannula) pass(cl *Client, m *irc.Message) {
	if len(m.Params) == 0 && m.Trailing == "" {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NEEDMOREPARAMS, []string{irc.PASS}, "Not enough parameters", false}
		return
	}

	if cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_ALREADYREGISTRED, []string{}, "You may not reregister", false}
		return
	}

	if len(m.Params) == 1 {
		cl.Pass = m.Params[0]
	} else {
		cl.Pass = m.Trailing
	}
}

func (c *Cannula) user(cl *Client, m *irc.Message) {
	if len(m.Params) != 3 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NEEDMOREPARAMS, []string{irc.USER}, "Not enough parameters", false}
		return
	}

	if cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_ALREADYREGISTRED, []string{}, "You may not reregister", false}
		return
	}

	cl.Prefix.User = m.Params[0]

	c.checkAuth(cl)
}

func (c *Cannula) nick(cl *Client, m *irc.Message) {
	if cl.Service != nil {
		cl.in <- &irc.Message{c.prefix, irc.ERR_ERRONEUSNICKNAME, []string{}, "Linked accounts can't change their nick", false}
		return
	}

	name := ""

	if len(m.Params) == 0 && m.Trailing == "" {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NONICKNAMEGIVEN, []string{}, "No nickname given", false}
		return
	}

	if len(m.Params) == 1 {
		name = m.Params[0]
	} else {
		name = m.Trailing
	}

	if len(name) == 0 || strings.Index(name, " ") != -1 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_ERRONEUSNICKNAME, []string{name}, "Erroneous nickname", false}
		return
	}

	if c.names[name] != nil {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NICKNAMEINUSE, []string{name}, "Nickname is already in use", false}
		return
	}

	if cl.Prefix.Name != "" {
		delete(c.names, cl.Prefix.Name)
	}
	c.names[name] = cl

	m.Prefix = &irc.Prefix{cl.Prefix.Name, cl.Prefix.User, cl.Prefix.Host}

	for ch := range cl.Channels {
		c.channels[ch].Nick(c, cl, m)
	}

	cl.Prefix.Name = name

	if !cl.Authorized {
		c.checkAuth(cl)
	}
}

func (c *Cannula) join(cl *Client, m *irc.Message) {
	if !cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTREGISTERED, []string{}, "You have not registered", false}
		return
	}

	if len(m.Params) < 1 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NEEDMOREPARAMS, []string{irc.JOIN}, "Not enough parameters", false}
		return
	}

	target := m.Params[0]

	targets := strings.Split(target, ",")

	for _, target := range targets {
		if strings.Index(target, "#") != 0 {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHCHANNEL, []string{target}, "No such channel", false}
			continue
		}

		ch := c.channels[target]
		if ch == nil {
			ch = &Channel{
				Name:      target,
				clients:   make(map[*Client]*ChannelClient),
				ytClients: make(map[*YTClient]*ChannelClient),
				ignore:    make(map[string]bool),
			}
			c.channels[target] = ch
			go ch.Init(c)
		}

		ch.Join(c, cl, &irc.Message{cl.Prefix, m.Command, []string{target}, m.Trailing, m.EmptyTrailing})
	}
}

func (c *Cannula) part(cl *Client, m *irc.Message) {
	target := c.verifyChannelTarget(cl, m, m.Command)
	if target == "" {
		return
	}

	targets := strings.Split(target, ",")

	for _, target := range targets {
		ch := c.channels[target]
		ch.Part(c, cl, &irc.Message{cl.Prefix, m.Command, []string{target}, m.Trailing, m.EmptyTrailing})
	}
}

func (c *Cannula) privmsg(cl *Client, m *irc.Message) {
	target := c.verifyTarget(cl, m, m.Command)
	if target == "" {
		return
	}

	if m.Trailing == "" {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTEXTTOSEND, []string{}, "No text to send", false}
		return
	}

	targets := strings.Split(target, ",")

	ignore := cl.Prefix
	if len(targets) > 1 {
		ignore = nil
	}

	for _, target := range targets {
		c.broadcast(&irc.Message{cl.Prefix, m.Command, []string{target}, m.Trailing, m.EmptyTrailing}, ignore)
	}
}

func (c *Cannula) notice(cl *Client, m *irc.Message) {
	target := c.verifyTarget(cl, m, m.Command)
	if target == "" {
		return
	}

	if m.Trailing == "" {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTEXTTOSEND, []string{}, "No text to send", false}
		return
	}

	targets := strings.Split(target, ",")

	ignore := cl.Prefix
	if len(targets) > 1 {
		ignore = nil
	}

	for _, target := range targets {
		c.broadcast(&irc.Message{cl.Prefix, m.Command, []string{target}, m.Trailing, m.EmptyTrailing}, ignore)
	}
}

func (c *Cannula) ping(cl *Client, m *irc.Message) {
	cl.in <- &irc.Message{c.prefix, irc.PONG, []string{}, m.Trailing, m.EmptyTrailing}
}

func (c *Cannula) broadcast(m *irc.Message, ignore *irc.Prefix) {
	if len(m.Params) != 1 {
		return
	}

	target := m.Params[0]

	ch := c.channels[target]
	if ch != nil {
		cl := c.clients[m.Prefix]

		ch.Broadcast(m, ignore)

		if m.Command == irc.PRIVMSG && ch.liveChatId != "" && cl != nil && cl.Service != nil {
			message := m.Trailing
			if strings.Index(message, "") == 0 {
				message = strings.Trim(message, "")
				if strings.Index(message, "ACTION ") == 0 {
					message = message[7:]
				} else {
					return
				}
			}
			message = IRCToYT.Replace(message)

			c.rateLimit <- func() {
				res, err := cl.Service.LiveChatMessages.Insert("snippet", &youtube.LiveChatMessage{
					Snippet: &youtube.LiveChatMessageSnippet{
						LiveChatId: ch.liveChatId,
						Type:       "textMessageEvent",
						TextMessageDetails: &youtube.LiveChatTextMessageDetails{
							MessageText: message,
						},
					},
				}).Do()

				if err != nil {
					return
				}

				ch.Lock()
				defer ch.Unlock()

				ch.ignore[res.Id] = true
			}
		}

	} else {
		cl := c.names[target]

		if cl != nil {
			cl.in <- m
		}
	}
}

func getHost(a net.Addr) string {
	addr := a.String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	if domains, err := net.LookupAddr(addr); err == nil {
		if len(domains) > 0 {
			return strings.TrimSuffix(domains[0], ".")
		}
	}
	return addr
}

func (c *Cannula) handleRateLimit() {
	for f := range c.rateLimit {
		go f()

		time.Sleep(time.Second)
	}
}

func (c *Cannula) Listen(laddr string) error {
	l, err := net.Listen("tcp", laddr)
	if err != nil {
		return err
	}

	c.listener = l
	defer l.Close()

	c.prefix = &irc.Prefix{
		Name: "irc.septapus.com",
	}
	c.in = make(chan interface{}, 1000)
	c.clients = make(map[*irc.Prefix]*Client)
	c.ytClients = make(map[string]*YTClient)
	c.channels = make(map[string]*Channel)
	c.names = make(map[string]*Client)
	c.rateLimit = make(chan func(), 1000)

	go c.handle()
	go c.handleRateLimit()

	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}

		go c.handleConn(conn)
	}
}
