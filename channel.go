package cannula

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sorcix/irc"
	"google.golang.org/api/youtube/v3"
)

type ChannelClient struct {
	Owner     bool
	Moderator bool
	Voice     bool // IRC Voice counts as a YouTube sponsor.
	LastSpoke time.Time
}

type Channel struct {
	sync.RWMutex

	Name  string
	Topic string

	clients    map[*Client]*ChannelClient
	ytClients  map[*YTClient]*ChannelClient
	names      string
	liveChatId string
	ignore     map[string]bool
	quitchan   chan interface{}
}

func (ch *Channel) Init(c *Cannula) {
	ch.Lock()

	if len(ch.Name[1:]) != 11 {
		ch.Topic = "Invalid YouTube VideoID"
		ch.broadcast(&irc.Message{c.prefix, irc.TOPIC, []string{ch.Name}, ch.Topic, false}, nil)
		return
	}

	liveChatId, events, quit := c.ytEventStream(ch.Name[1:])
	if liveChatId == "" {
		ch.Topic = "This chat has ended"
		ch.broadcast(&irc.Message{c.prefix, irc.TOPIC, []string{ch.Name}, ch.Topic, false}, nil)
		return
	}

	ch.liveChatId = liveChatId

	ch.Unlock()

	go ch.removeIdle(c, quit)

	for i := range events {
		ch.Lock()

		switch i := i.(type) {
		case *youtube.VideoSnippet:
			ch.Topic = fmt.Sprintf("%s - %s", i.ChannelTitle, i.Title)
			ch.broadcast(&irc.Message{c.prefix, irc.TOPIC, []string{ch.Name}, ch.Topic, false}, nil)
		case *youtube.LiveChatMessage:
			ch.broadcastYtMessage(c, i)
		}

		ch.Unlock()
	}

	ch.Lock()

	ch.broadcast(&irc.Message{c.prefix, irc.NOTICE, []string{ch.Name}, "This chat has ended", false}, nil)
	ch.Topic = "This chat has ended"
	ch.broadcast(&irc.Message{c.prefix, irc.TOPIC, []string{ch.Name}, ch.Topic, false}, nil)

	ch.Unlock()
}

func (ch *Channel) broadcastYtMessage(c *Cannula, m *youtube.LiveChatMessage) {
	if m == nil {
		return
	}

	if m.AuthorDetails == nil {
		return
	}

	ytClient := c.YTClient(m.AuthorDetails.DisplayName, m.AuthorDetails.ChannelId)

	cl := c.clients[ytClient.Prefix]

	var ccl *ChannelClient

	if cl == nil {
		ccl = ch.ytClients[ytClient]
		if ccl == nil {
			ccl = &ChannelClient{}
			ch.ytClients[ytClient] = ccl
		}
	} else {
		ccl = ch.clients[cl]
		if ccl == nil {
			ccl = &ChannelClient{}
			ch.clients[cl] = ccl
		}
	}

	if ccl.LastSpoke.IsZero() {
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.JOIN, []string{ch.Name}, ytClient.Prefix.Name, false}, nil)
	}
	ccl.LastSpoke = time.Now().Add(5 * time.Minute)

	if m.AuthorDetails.IsChatOwner && !ccl.Owner {
		ccl.Owner = true
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.MODE, []string{ch.Name, "+o", ytClient.Prefix.Name}, "", true}, nil)
	}

	if m.AuthorDetails.IsChatModerator && !ccl.Moderator {
		ccl.Moderator = true
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.MODE, []string{ch.Name, "+h", ytClient.Prefix.Name}, "", true}, nil)
	}

	if m.AuthorDetails.IsChatSponsor && !ccl.Voice {
		ccl.Voice = true
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.MODE, []string{ch.Name, "+v", ytClient.Prefix.Name}, "", true}, nil)
	}

	if m.Snippet.Type == "fanFundingEvent" || m.Snippet.Type == "newSponsorEvent" {
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.NOTICE, []string{ch.Name}, m.Snippet.DisplayMessage, false}, nil)
	} else if m.Snippet.HasDisplayContent {
		if ch.ignore[m.Id] {
			delete(ch.ignore, m.Id)
		} else {
			ch.broadcast(&irc.Message{ytClient.Prefix, irc.PRIVMSG, []string{ch.Name}, m.Snippet.DisplayMessage, false}, nil)
		}
	}
}

func (ch *Channel) Broadcast(m *irc.Message, ignore *irc.Prefix) {
	ch.RLock()
	defer ch.RUnlock()

	ch.broadcast(m, ignore)
}

func (ch *Channel) broadcast(m *irc.Message, ignore *irc.Prefix) {
	for cl := range ch.clients {
		if cl.Prefix == ignore {
			continue
		}
		cl.in <- m
	}
}

func (ch *Channel) name(p *irc.Prefix, ccl *ChannelClient) string {
	if ccl.Owner {
		return "@" + p.Name
	}
	if ccl.Moderator {
		return "%" + p.Name
	}
	if ccl.Voice {
		return "+" + p.Name
	}
	return p.Name
}

// Must be called in a lock
func (ch *Channel) createNames() {
	names := []string{}
	for cl, ccl := range ch.clients {
		names = append(names, ch.name(cl.Prefix, ccl))
	}
	for cl, ccl := range ch.ytClients {
		names = append(names, ch.name(cl.Prefix, ccl))
	}
	ch.names = strings.Join(names, " ")
}

func (ch *Channel) Join(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()
	defer ch.Unlock()

	if cl.YTClient != nil {
		delete(ch.ytClients, cl.YTClient)
	}
	if ch.clients[cl] == nil {
		ch.clients[cl] = &ChannelClient{}
	}
	ch.createNames()

	cl.Channels[ch.Name] = true

	ch.broadcast(m, nil)

	if ch.Topic != "" {
		cl.in <- &irc.Message{c.prefix, irc.RPL_TOPIC, []string{m.Prefix.Name, ch.Name}, ch.Topic, false}
	}
	cl.in <- &irc.Message{c.prefix, irc.RPL_NAMREPLY, []string{m.Prefix.Name, "=", ch.Name}, ch.names, false}
}

func (ch *Channel) Part(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()
	defer ch.Unlock()

	ch.broadcast(m, nil)

	delete(ch.clients, cl)
	ch.createNames()

	delete(cl.Channels, ch.Name)
}

func (ch *Channel) Quit(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()
	defer ch.Unlock()

	delete(ch.clients, cl)
	ch.createNames()

	delete(cl.Channels, ch.Name)

	ch.broadcast(m, nil)
}

func (ch *Channel) Nick(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()
	defer ch.Unlock()

	ch.createNames()

	ch.broadcast(m, nil)
}

func (ch *Channel) removeIdle(c *Cannula, quit chan interface{}) {
	empty := 0
	for {
		time.Sleep(time.Minute)

		ch.Lock()

		// If we are empty for 5 minutes, stop polling.
		if len(ch.clients) == 0 {
			empty++
			if empty > 5 {
				ch.Unlock()

				close(quit)

				c.Lock()
				defer c.Unlock()

				delete(c.channels, ch.Name)

				return
			}
		} else {
			empty = 0
		}

		r := []*YTClient{}
		now := time.Now()

		for cl, ccl := range ch.ytClients {
			if now.After(ccl.LastSpoke) {
				r = append(r, cl)
			}
		}

		for _, cl := range r {
			delete(ch.ytClients, cl)
		}

		for _, cl := range r {
			ch.broadcast(&irc.Message{cl.Prefix, irc.PART, []string{ch.Name}, "", true}, nil)
		}

		ch.Unlock()
	}
}
