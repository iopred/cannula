package cannula

import (
	"fmt"
	"net"
	"time"

	"github.com/sorcix/irc"
	"google.golang.org/api/youtube/v3"
)

type ClientMessage struct {
	*irc.Message
}

const timeout = 10 * time.Minute

type Client struct {
	Prefix   *irc.Prefix
	Service  *youtube.Service
	YTClient *YTClient

	netconn net.Conn
	conn    *irc.Conn
	out     chan interface{}
	in      chan interface{}

	Pass       string
	Authorized bool
	Channels   map[string]bool
}

func NewClient(prefix *irc.Prefix, conn net.Conn, out chan interface{}) *Client {
	prefix.Host = getHost(conn.RemoteAddr())

	return &Client{
		Prefix:   prefix,
		netconn:  conn,
		conn:     irc.NewConn(conn),
		out:      out,
		in:       make(chan interface{}, 100),
		Channels: make(map[string]bool),
	}
}

func (cl *Client) handle() {
	go cl.read()
	go cl.write()
}

func (cl *Client) read() error {
	for {
		m, err := cl.conn.Decode()
		if m == nil {
			cl.out <- &ClientMessage{&irc.Message{cl.Prefix, "QUIT", []string{}, "Closed.", false}}
			return nil
		}
		if err != nil {
			cl.out <- &ClientMessage{&irc.Message{cl.Prefix, "QUIT", []string{}, "Read error.", false}}
			return err
		}

		fmt.Println(cl.Prefix, "<", m)

		m.Prefix = cl.Prefix
		cl.out <- &ClientMessage{m}
	}

	return nil
}

func (cl *Client) write() error {
	for i := range cl.in {
		switch i := i.(type) {
		case *ServerClose:
			cl.netconn.Close()
			return nil
		case *irc.Message:
			fmt.Println(cl.Prefix, ">", i)
			cl.netconn.SetWriteDeadline(time.Now().Add(1 * time.Minute))
			if err := cl.conn.Encode(i); err != nil {
				cl.out <- &ClientMessage{&irc.Message{cl.Prefix, "QUIT", []string{}, "Write error.", false}}
				cl.netconn.Close()
				return err
			}
		}
	}

	return nil
}
