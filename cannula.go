package cannula

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sorcix/irc"
	"golang.org/x/oauth2"
	"google.golang.org/api/youtube/v3"
)

var versionString = "v0.3"
var startTime = time.Now()

// These lines contain zero width spaces.
var YTToIRC = strings.NewReplacer(" ", " ", "!", "❢", "@", "᪤", "+", "​+", "&", "​&", "%", "​%", ":", "：")
var IRCToYT = strings.NewReplacer(" ", " ", "❢", "!", "᪤", "@", "​+", "+", "​&", "&", "​%", "%", "：", ":")
var StripColors = regexp.MustCompile("\x1f|\x02|\x12|\x0f|\x16|\x03(?:\\d{1,2}(?:,\\d{1,2})?)?")

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

func (c *Cannula) handleConn(conn net.Conn) {
	cl := NewClient(&irc.Prefix{}, conn, c.in)

	c.Lock()
	defer c.Unlock()

	c.clients[cl.Prefix] = cl

	go cl.handle()
}

func (c *Cannula) handle() {
	for i := range c.in {
		switch i := i.(type) {
		case *irc.Message:
			c.handleMessage(i)
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
	case irc.MODE:
		c.mode(cl, m)
	case irc.KICK:
		c.kick(cl, m)
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
		res, err := cl.Service.Channels.List([]string{"id","snippet"}).Mine(true).Do()
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

			// Delete the current client, as the nick and yt nick may match.
			delete(c.clients, cl.Prefix)
			delete(c.names, cl.Prefix.Name)

			// If the name for the youtube prefix already exist, we are free to kill this connection as we have already deleted it from the client list.
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
	cl.in <- &irc.Message{c.prefix, irc.RPL_MOTDSTART, []string{cl.Prefix.Name}, fmt.Sprintf("- %s Message of the day -", c.prefix.Name), false}
	cl.in <- &irc.Message{c.prefix, irc.RPL_MOTD, []string{cl.Prefix.Name}, "- If you get a lot of rate limit errors, consider adding your account as a moderator.", false}
	cl.in <- &irc.Message{c.prefix, irc.RPL_MOTD, []string{cl.Prefix.Name}, "- Moderation actions are now supported. /kick will time out a user for 5 minutes. +b (ban) will ban a user.", false}
	cl.in <- &irc.Message{c.prefix, irc.RPL_ENDOFMOTD, []string{cl.Prefix.Name}, "End of MOTD command", false}
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
	cl.in <- &irc.Message{nil, irc.PONG, m.Params, m.Trailing, m.EmptyTrailing}
}

func (c *Cannula) kick(cl *Client, m *irc.Message) {
	if !cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTREGISTERED, []string{}, "You have not registered", false}
		return
	}

	if len(m.Params) != 2 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NEEDMOREPARAMS, []string{m.Command}, "Not enough parameters", false}
		return
	}

	channels := strings.Split(m.Params[0], ",")
	targets := strings.Split(m.Params[1], ",")

	if len(channels) != len(targets) {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NEEDMOREPARAMS, []string{m.Command}, "Not enough parameters", false}
		return
	}

	for i := range channels {
		channel := channels[i]
		target := targets[i]

		if strings.Index(channel, "#") != 0 {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHCHANNEL, []string{channel}, "No such channel", false}
			continue
		}

		ch := c.channels[channel]
		if ch == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHCHANNEL, []string{channel}, "No such channel", false}
			continue
		}

		ccl := ch.clients[cl]
		if ccl == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOTONCHANNEL, []string{channel}, "You're not on that channel", false}
			continue
		}

		if !ccl.Moderator && !ccl.Owner {
			cl.in <- &irc.Message{c.prefix, irc.ERR_CHANOPRIVSNEEDED, []string{channel}, "You're not channel operator", false}
			continue
		}

		ytc := ch.YTClient(c, target)
		if ytc == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHNICK, []string{target}, "No such nick/channel", false}
			continue
		}

		c.rateLimit <- func() {
			_, err := cl.Service.LiveChatBans.Insert([]string{"snippet"}, &youtube.LiveChatBan{
				Snippet: &youtube.LiveChatBanSnippet{
					LiveChatId: ch.liveChatId,
					BannedUserDetails: &youtube.ChannelProfileDetails{
						ChannelId: ytc.ChannelID,
					},
					Type:               "temporary",
					BanDurationSeconds: 5 * 60,
				},
			}).Do()

			if err != nil {
				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{channel}, fmt.Sprintf("Error timing out user: %s (%s): %s", ytc.Prefix.Name, ytc.ChannelID, strings.Replace(err.Error(), "\n", "", -1)), false}
			} else {
				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{channel}, fmt.Sprintf("%s has been timed out.", ytc.Prefix.Name), false}
			}
		}
	}
}

var channelIDRegexp = regexp.MustCompile("@(UC.{22})\\.youtube\\.com")

func (c *Cannula) mode(cl *Client, m *irc.Message) {
	if !cl.Authorized {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTREGISTERED, []string{}, "You have not registered", false}
		return
	}

	if len(m.Params) == 0 {
		// MODE not supported
		return
	}

	target := m.Params[0]

	if c.names[target] != nil {
		// MODE <user> not supported
		return
	}

	if strings.Index(target, "#") != 0 {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHNICK, []string{target}, "No such nick/channel", false}
		return
	}

	ch := c.channels[target]
	if ch == nil {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHCHANNEL, []string{target}, "No such channel", false}
		return
	}

	ccl := ch.clients[cl]
	if ccl == nil {
		cl.in <- &irc.Message{c.prefix, irc.ERR_NOTONCHANNEL, []string{target}, "You're not on that channel", false}
		return
	}

	if len(m.Params) == 1 {
		// MODE <channel> not supported
		return
	}

	mode := m.Params[1]

	switch mode {
	case "-b":
		if !ccl.Moderator && !ccl.Owner {
			cl.in <- &irc.Message{c.prefix, irc.ERR_CHANOPRIVSNEEDED, []string{target}, "You're not channel operator", false}
			return
		}

		cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{target}, "YouTube does not provide a reliable way to remove a ban at this time, manage your bans in the address book: https://www.youtube.com/address_book", false}
	case "+b":
		if len(m.Params) < 3 {
			cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{target}, "YouTube does not provide a reliable way to list bans at this time, manage your bans in the address book: https://www.youtube.com/address_book", false}
			return
		}

		if !ccl.Moderator && !ccl.Owner {
			cl.in <- &irc.Message{c.prefix, irc.ERR_CHANOPRIVSNEEDED, []string{target}, "You're not channel operator", false}
			return
		}

		target2 := m.Params[2]
		var ytc *YTClient
		t := c.names[target2]

		if t == nil {
			// Try get the target from the hostmask.
			match := channelIDRegexp.FindStringSubmatch(target2)
			if match != nil {
				ytc = c.ytClients[match[1]]
			}
		} else {
			ytc = t.YTClient
		}

		if ytc == nil {
			cl.in <- &irc.Message{c.prefix, irc.ERR_NOSUCHNICK, []string{target2}, "No such nick/channel", false}
			return
		}

		c.rateLimit <- func() {
			_, err := cl.Service.LiveChatBans.Insert([]string{"snippet"}, &youtube.LiveChatBan{
				Snippet: &youtube.LiveChatBanSnippet{
					LiveChatId: ch.liveChatId,
					BannedUserDetails: &youtube.ChannelProfileDetails{
						ChannelId: ytc.ChannelID,
					},
					Type: "permanent",
				},
			}).Do()

			if err != nil {
				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{target}, fmt.Sprintf("Error banning user: %s (%s): %s", ytc.Prefix.Name, ytc.ChannelID, strings.Replace(err.Error(), "\n", "", -1)), false}
			} else {
				cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{target}, fmt.Sprintf("%s has been banned.", ytc.Prefix.Name), false}
			}
		}
	default:
		cl.in <- &irc.Message{c.prefix, irc.ERR_UMODEUNKNOWNFLAG, []string{}, "Unknown MODE flag", false}
	}
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
			message = StripColors.ReplaceAllString(message, "")

			// Send messages of 200 characters.
			for i := 0; i < len(message); i += 200 {
				m := i + 200
				if m > len(message) {
					m = len(message)
				}

				me := message[i:m]

				c.rateLimit <- func() {
					res, err := cl.Service.LiveChatMessages.Insert([]string{"snippet"}, &youtube.LiveChatMessage{
						Snippet: &youtube.LiveChatMessageSnippet{
							LiveChatId: ch.liveChatId,
							Type:       "textMessageEvent",
							TextMessageDetails: &youtube.LiveChatTextMessageDetails{
								MessageText: me,
							},
						},
					}).Do()

					if err != nil {
						cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{target}, fmt.Sprintf("Error sending message: %s", strings.Replace(err.Error(), "\n", "", -1)), false}
						return
					}

					ch.Lock()
					defer ch.Unlock()

					ch.ignore[res.Id] = true
				}
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

func (c *Cannula) Exit() {
	for _, cl := range c.clients {
		cl.in <- &irc.Message{c.prefix, irc.NOTICE, []string{cl.Prefix.Name}, "IRC server is being restarted.", false}
	}
}
