package cannula

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"google.golang.org/api/youtube/v3"

	"github.com/atotto/clipboard"
	"github.com/sorcix/irc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var reauth bool
var auth string
var configFilename string
var tokenFilename string
var tokensFilename string

func init() {
	flag.BoolVar(&reauth, "reauth", false, "Generates a URL that provides an auth code.")
	flag.StringVar(&auth, "auth", "", "Exchanges the provided auth code for an oauth2 token.")
	flag.StringVar(&configFilename, "config", "cannulaoauth2config.json", "The filename that contains the oauth2 config.")
	flag.StringVar(&tokenFilename, "token", "cannulaoauth2token.json", "The filename to store the primary oauth2 token.")
	flag.StringVar(&tokensFilename, "tokens", "cannulaoauth2tokens.json", "The filename to store user oauth2 tokens.")
	flag.Parse()
}

type YTClient struct {
	Prefix *irc.Prefix

	Name      string
	ChannelID string
}

func NewYTClient(name string, channelID string) *YTClient {
	return &YTClient{
		// Replace spaces in names with a non breaking space.
		Prefix: &irc.Prefix{strings.Replace(name, " ", " ", -1), channelID, "youtube.com"},

		Name:      name,
		ChannelID: channelID,
	}
}

func ytCreateToken(config *oauth2.Config) (*oauth2.Token, error) {
	if auth != "" {
		tok, err := config.Exchange(oauth2.NoContext, auth)
		if err != nil {
			return nil, err
		}

		b, err := json.Marshal(tok)
		if err != nil {
			return nil, err
		}

		err = ioutil.WriteFile(tokenFilename, b, 0777)
		if err != nil {
			return nil, err
		}

		return tok, nil
	}

	b, err := ioutil.ReadFile(tokenFilename)
	if err != nil {
		return nil, err
	}

	tok := &oauth2.Token{}

	err = json.Unmarshal(b, tok)
	if err != nil {
		return nil, err
	}

	return tok, nil
}

func (c *Cannula) ytGenerateAuthURL() string {
	return c.config.AuthCodeURL("state", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (c *Cannula) ytAuth() error {
	b, err := ioutil.ReadFile(configFilename)
	if err != nil {
		return err
	}

	c.config, err = google.ConfigFromJSON(b, "https://www.googleapis.com/auth/youtube")
	if err != nil {
		return err
	}

	c.token, err = ytCreateToken(c.config)
	if reauth || err != nil {
		url := c.ytGenerateAuthURL()
		clipboard.WriteAll(url)
		return fmt.Errorf("Visit the following URL to generate an auth code, then rerun with -auth=<code> (It has also been copied to your clipboard):\n%s", url)
	}
	if err != nil {
		return err
	}

	c.service, err = c.ytCreateService(c.token)

	if err := c.ytLoadTokens(); err != nil {
		c.tokens = make(map[string]*oauth2.Token)
		c.ytSaveTokens()
	}

	return nil
}

func (c *Cannula) ytLoadTokens() error {
	b, err := ioutil.ReadFile(tokensFilename)
	if err != nil {
		return err
	}

	tokens := make(map[string]*oauth2.Token)

	err = json.Unmarshal(b, &tokens)
	if err != nil {
		return err
	}

	c.tokens = tokens

	return nil
}

func (c *Cannula) ytSaveTokens() error {
	b, err := json.Marshal(c.tokens)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(tokensFilename, b, 0777)
	if err != nil {
		return err
	}

	return nil
}

func (c *Cannula) ytCreateService(token *oauth2.Token) (*youtube.Service, error) {
	return youtube.New(c.config.Client(oauth2.NoContext, token))
}

func (c *Cannula) ytPollMessages(liveChatId string, events chan interface{}, quit chan interface{}) {
	errors := 0

	pageToken := ""
	for {
		select {
		case <-quit:
			return
		default:

		}
		list := c.service.LiveChatMessages.List(liveChatId, "id,snippet,authorDetails").MaxResults(200)
		if pageToken != "" {
			list.PageToken(pageToken)
		}

		liveChatMessageListResponse, err := list.Do()

		if err != nil {
			errors++
			if errors > 10 {
				close(events)
				return
			}
		} else {
			errors = 0
			// Ignore the first results, we only want new chats.
			if pageToken != "" {
				for _, message := range liveChatMessageListResponse.Items {
					events <- message
				}
			}
			pageToken = liveChatMessageListResponse.NextPageToken
		}

		if liveChatMessageListResponse != nil && liveChatMessageListResponse.PollingIntervalMillis != 0 {
			time.Sleep(time.Duration(liveChatMessageListResponse.PollingIntervalMillis) * time.Millisecond)
		} else {
			time.Sleep(10 * time.Second)
		}
	}
}

func (c *Cannula) ytEventStream(videoID string) (string, chan interface{}, chan interface{}) {

	r, err := c.service.Videos.List("snippet,liveStreamingDetails").Id(videoID).Do()
	if err != nil {
		return "", nil, nil
	}

	if len(r.Items) != 1 {
		return "", nil, nil
	}

	v := r.Items[0]

	if v.LiveStreamingDetails.ActiveLiveChatId == "" {
		return "", nil, nil
	}

	events := make(chan interface{}, 100)
	events <- v.Snippet

	quit := make(chan interface{})

	go c.ytPollMessages(v.LiveStreamingDetails.ActiveLiveChatId, events, quit)

	return v.LiveStreamingDetails.ActiveLiveChatId, events, quit
}

func (c *Cannula) ytClient(name string, channelID string) *YTClient {
	c.RLock()

	cl := c.ytClients[channelID]

	c.RUnlock()

	if cl == nil {
		c.Lock()

		cl = NewYTClient(name, channelID)
		c.ytClients[channelID] = cl

		c.Unlock()
	}

	return cl
}
