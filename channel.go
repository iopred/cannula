package cannula

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sorcix/irc"
	"google.golang.org/api/youtube/v3"
)

type Channel struct {
	sync.RWMutex

	Name  string
	Topic string

	clients    map[*Client]bool
	ytClients  map[*YTClient]time.Time
	names      string
	liveChatId string
	ignore     map[string]bool
	quitchan   chan interface{}
}

func (ch *Channel) init(c *Cannula) {
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

	go ch.removeIdle(c, quit)

	for i := range events {
		switch i := i.(type) {
		case *youtube.VideoSnippet:
			ch.Topic = fmt.Sprintf("%s - %s", i.ChannelTitle, i.Title)
			ch.broadcast(&irc.Message{c.prefix, irc.TOPIC, []string{ch.Name}, ch.Topic, false}, nil)
		case *youtube.LiveChatMessage:
			ch.broadcastYtMessage(c, i)
		}
	}

	ch.broadcast(&irc.Message{c.prefix, irc.NOTICE, []string{ch.Name}, "This chat has ended", false}, nil)
	ch.Topic = "This chat has ended"
	ch.broadcast(&irc.Message{c.prefix, irc.TOPIC, []string{ch.Name}, ch.Topic, false}, nil)
}

func (ch *Channel) broadcastYtMessage(c *Cannula, m *youtube.LiveChatMessage) {
	if m == nil || m.AuthorDetails == nil {
		return
	}

	ytClient := c.ytClient(m.AuthorDetails.DisplayName, m.AuthorDetails.ChannelId)

	ch.Lock()

	if ch.ignore[m.Id] {
		delete(ch.ignore, m.Id)

		ch.Unlock()

		return
	}

	joined := false

	if c.clients[ytClient.Prefix] == nil {
		joined = ch.ytClients[ytClient].IsZero()
		ch.ytClients[ytClient] = time.Now().Add(5 * time.Minute)
	}

	ch.Unlock()

	if joined {
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.JOIN, []string{ch.Name}, ytClient.Prefix.Name, false}, nil)
	}

	if m.Snippet.FanFundingEventDetails != nil {
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.NOTICE, []string{ch.Name}, m.Snippet.DisplayMessage, false}, nil)
	} else if m.Snippet.HasDisplayContent {
		ch.broadcast(&irc.Message{ytClient.Prefix, irc.PRIVMSG, []string{ch.Name}, m.Snippet.DisplayMessage, false}, nil)
	}
}

func (ch *Channel) broadcast(m *irc.Message, ignore *irc.Prefix) {
	for cl := range ch.clients {
		if cl.Prefix == ignore {
			continue
		}
		cl.in <- m
	}
}

// Must be called in a lock
func (ch *Channel) createNames() {
	names := []string{}
	for cl := range ch.clients {
		names = append(names, cl.Prefix.Name)
	}
	for cl := range ch.ytClients {
		names = append(names, cl.Prefix.Name)
	}
	ch.names = strings.Join(names, " ")
}

func (ch *Channel) join(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()

	if !ch.clients[cl] {
		ch.clients[cl] = true
	}
	ch.createNames()

	ch.Unlock()

	cl.Channels[ch.Name] = true

	ch.broadcast(m, nil)

	if ch.Topic != "" {
		cl.in <- &irc.Message{c.prefix, irc.RPL_TOPIC, []string{m.Prefix.Name, ch.Name}, ch.Topic, false}
	}
	cl.in <- &irc.Message{c.prefix, irc.RPL_NAMREPLY, []string{m.Prefix.Name, "=", ch.Name}, ch.names, false}
}

func (ch *Channel) part(c *Cannula, cl *Client, m *irc.Message) {
	ch.broadcast(m, nil)

	ch.Lock()

	delete(ch.clients, cl)
	ch.createNames()

	delete(cl.Channels, ch.Name)

	ch.Unlock()
}

func (ch *Channel) quit(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()

	delete(ch.clients, cl)
	ch.createNames()

	delete(cl.Channels, ch.Name)

	ch.Unlock()

	ch.broadcast(m, nil)
}

func (ch *Channel) nick(c *Cannula, cl *Client, m *irc.Message) {
	ch.Lock()

	ch.createNames()

	ch.Unlock()

	ch.broadcast(m, nil)
}

func (ch *Channel) removeIdle(c *Cannula, quit chan interface{}) {
	empty := 0
	for {
		time.Sleep(time.Minute)

		c.Lock()

		// If we are empty for 5 minutes, stop polling.
		if len(ch.clients) == 0 {
			empty++
			if empty > 5 {
				close(quit)

				delete(c.channels, ch.Name)

				c.Unlock()

				return
			}
		} else {
			empty = 0
		}

		c.Unlock()

		r := []*YTClient{}
		now := time.Now()

		ch.Lock()

		for cl, t := range ch.ytClients {
			if now.After(t) {
				r = append(r, cl)
			}
		}

		for _, cl := range r {
			delete(ch.ytClients, cl)
		}

		ch.Unlock()

		for _, cl := range r {
			ch.broadcast(&irc.Message{cl.Prefix, irc.PART, []string{ch.Name}, "", true}, nil)
		}
	}
}
