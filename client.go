package cannula

import (
	"fmt"
	"net"
	"time"

	"github.com/sorcix/irc"
	"google.golang.org/api/youtube/v3"
)

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
	go func() {
		for {
			cl.netconn.SetReadDeadline(time.Now().Add(10 * time.Minute))
			m, err := cl.conn.Decode()
			if err != nil {
				fmt.Println(cl.Prefix, "Read error")
				break
			}
			if m == nil {
				continue
			}

			fmt.Println(cl.Prefix, "<", m)

			m.Prefix = cl.Prefix
			cl.out <- m
		}
		cl.out <- &irc.Message{cl.Prefix, "QUIT", []string{}, "Read error.", false}
	}()

	for {
		select {
		case i := <-cl.in:
			switch i := i.(type) {
			case *ServerClose:
				break
				return
			case *irc.Message:
				fmt.Println(cl.Prefix, ">", i)
				cl.netconn.SetWriteDeadline(time.Now().Add(1 * time.Minute))
				if err := cl.conn.Encode(i); err != nil {
					fmt.Println(cl.Prefix, "Write error")
					cl.out <- &irc.Message{cl.Prefix, "QUIT", []string{}, "Write error.", false}
				}
			}
		}
	}

	cl.netconn.Close()
	close(cl.in)
}
