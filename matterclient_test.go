package matterclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mattermost/mattermost/server/public/model"
)

// These tests run the client against a fake Mattermost server. Most of them
// only prove something when run with the race detector (go test -race).

const (
	testUserID    = "uuuuuuuuuuuuuuuuuuuuuuuuuu"
	testOtherID   = "oooooooooooooooooooooooooo"
	testTeamID    = "tttttttttttttttttttttttttt"
	testChannelID = "cccccccccccccccccccccccccc"
	testGroupID   = "gggggggggggggggggggggggggg"

	testTimeout = 3 * time.Second

	okBody = `{"status":"OK"}`
)

// wsConn serializes the writes to a websocket, gorilla/websocket allows only one writer.
type wsConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsConn) write(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	_ = w.conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

type fakeServer struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader
	done     chan struct{}

	mu      sync.Mutex
	wsConns []*wsConn
	wsCount atomic.Int32

	// failWS makes the websocket endpoint return an error,
	// hangWS makes it accept the connection but never answer the upgrade request.
	failWS atomic.Bool
	hangWS atomic.Bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()

	f := &fakeServer{done: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))

	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.done) })

	return f
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("X-Version-Id", "9.0.0.test")
	w.Header().Set("Content-Type", "application/json")

	_, _ = w.Write([]byte(body))
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v4/websocket":
		f.handleWS(w, r)
	case "/api/v4/users/logout", "/api/v4/system/ping":
		writeJSON(w, okBody)
	case "/api/v4/users/me":
		writeJSON(w, `{"id":"`+testUserID+`","username":"me","roles":"system_user"}`)
	case "/api/v4/users/" + testUserID + "/teams":
		writeJSON(w, `[{"id":"`+testTeamID+`","name":"team","display_name":"Team"}]`)
	case "/api/v4/users/" + testUserID + "/teams/" + testTeamID + "/channels":
		writeJSON(w, `[`+
			`{"id":"`+testChannelID+`","team_id":"`+testTeamID+`","type":"O","name":"town-square","display_name":"Town Square"},`+
			`{"id":"`+testGroupID+`","team_id":"","type":"G","name":"group","display_name":""}]`)
	case "/api/v4/teams/" + testTeamID + "/channels":
		writeJSON(w, `[]`)
	case "/api/v4/users":
		writeJSON(w, `[{"id":"`+testUserID+`","username":"me"},{"id":"`+testOtherID+`","username":"other"}]`)
	case "/api/v4/users/status/ids":
		writeJSON(w, `[{"user_id":"`+testUserID+`","status":"online"},{"user_id":"`+testOtherID+`","status":"away"}]`)
	default:
		http.Error(w, `{"id":"not.found","message":"not found","status_code":404}`, http.StatusNotFound)
	}
}

func (f *fakeServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if f.failWS.Load() {
		http.Error(w, "websocket unavailable", http.StatusInternalServerError)

		return
	}

	if f.hangWS.Load() {
		select {
		case <-r.Context().Done():
		case <-f.done:
		}

		return
	}

	conn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	wc := &wsConn{conn: conn}

	f.mu.Lock()
	f.wsConns = append(f.wsConns, wc)
	f.mu.Unlock()
	f.wsCount.Add(1)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var req struct {
			Seq    int64  `json:"seq"`
			Action string `json:"action"`
		}

		err = json.Unmarshal(data, &req)
		if err != nil || req.Action != "authentication_challenge" {
			continue
		}

		wc.write(fmt.Sprintf(`{"status":"OK","seq_reply":%d}`, req.Seq))
		wc.write(`{"event":"hello","data":{"server_version":"9.0.0"},"broadcast":{},"seq":0}`)
	}
}

// send pushes a raw JSON frame to the newest websocket connection.
func (f *fakeServer) send(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.wsConns) > 0 {
		f.wsConns[len(f.wsConns)-1].write(raw)
	}
}

// dropConnections abruptly closes the TCP connection of all websockets, like a network failure.
func (f *fakeServer) dropConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.wsConns {
		_ = c.conn.UnderlyingConn().Close()
	}

	f.wsConns = nil
}

func typingEvent() string {
	return `{"event":"typing","data":{"user_id":"` + testOtherID + `"},"broadcast":{"channel_id":"` + testChannelID + `"},"seq":1}`
}

func postedEvent(i int) string {
	post := fmt.Sprintf(`{\"id\":\"%026d\",\"user_id\":\"%s\",\"channel_id\":\"%s\",\"message\":\"hi\",\"create_at\":%d}`,
		i, testOtherID, testChannelID, i+1)

	return fmt.Sprintf(`{"event":"posted","data":{"post":"%s"},"broadcast":{"channel_id":"%s"},"seq":%d}`,
		post, testChannelID, i+10)
}

func newTestClient(f *fakeServer) *Client {
	m := New("me", "token=testtoken", "team", strings.TrimPrefix(f.srv.URL, "http://"), "")
	m.NoTLS = true
	m.Timeout = 3
	m.SetLogLevel("fatal")

	return m
}

// login returns a logged in client, cancel stops its goroutines (like the parent context of an application would).
func login(t *testing.T, f *fakeServer) (*Client, context.CancelFunc) {
	t.Helper()

	m := newTestClient(f)

	ctx, cancel := context.WithCancel(t.Context())
	m.Ctx = ctx

	err := m.Login(ctx)
	if err != nil {
		cancel()
		t.Fatalf("login failed: %v", err)
	}

	waitFor(t, "the websocket to be connected", func() bool { return f.wsCount.Load() > 0 })

	return m, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timeout waiting for %s", what)
}

func mustEvent(t *testing.T, raw string) *model.WebSocketEvent {
	t.Helper()

	ev, err := model.WebSocketEventFromJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("bad event json: %v", err)
	}

	return ev
}

// A websocket connection that can not be established must make Login fail, it
// used to carry on with a nil m.WsClient and crash in WsReceiver.
func TestLoginWebsocketConnectAborted(t *testing.T) {
	f := newFakeServer(t)
	f.failWS.Store(true)

	m := newTestClient(f)

	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()

	err := m.Login(ctx)
	if err == nil {
		t.Error("expected Login to fail when the websocket can not be connected")
	}

	if m.WsClient != nil {
		t.Errorf("expected no websocket client, got %v", m.WsClient)
	}

	// give a (wrongly) started WsReceiver the time to crash
	time.Sleep(300 * time.Millisecond)

	_ = m.Logout(t.Context())
}

// A handshake that is never answered must not block Login forever, it has to give up when its context is done.
func TestLoginWebsocketHandshakeHangs(t *testing.T) {
	f := newFakeServer(t)
	f.hangWS.Store(true)

	m := newTestClient(f)
	m.Timeout = 1

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	result := make(chan error, 1)

	go func() { result <- m.Login(ctx) }()

	select {
	case err := <-result:
		if err == nil {
			t.Error("expected Login to fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Login did not return, the websocket handshake has no timeout")
	}

	_ = m.Logout(t.Context())
}

// The Mattermost websocket client panics ("send on closed channel") when it is used after its connection dropped.
func TestWebsocketSendAfterConnectionDropped(t *testing.T) {
	f := newFakeServer(t)
	m, cancel := login(t, f)

	defer cancel()
	defer func() { _ = m.Logout(t.Context()) }()

	f.dropConnections()
	waitFor(t, "the dropped connection to be noticed", func() bool { return m.wsClosed.Load() != nil })

	m.Users.mu.Lock()
	m.Users.joinedChannels[testChannelID] = struct{}{}
	m.Users.channels[testChannelID] = map[string]struct{}{testOtherID: {}}
	m.Users.mu.Unlock()

	calls := map[string]func(){
		"WsSendTyping":              func() { _ = m.WsSendTyping(testChannelID, "") },
		"WsGetStatuses":             m.WsGetStatuses,
		"WsGetStatusesByIds":        func() { m.WsGetStatusesByIds([]string{testUserID}) },
		"SyncActiveContextStatuses": m.SyncActiveContextStatuses,
		"WsPing": func() {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()

			_ = m.WsPing(ctx)
		},
	}

	for name, call := range calls {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", name, r)
				}
			}()

			call()
		}()
	}
}

// Events do not necessarily have a broadcast, this used to be a nil pointer dereference.
func TestEventsWithoutBroadcast(t *testing.T) {
	f := newFakeServer(t)
	m := newTestClient(f)

	events := map[string]string{
		"user_removed": `{"event":"user_removed","data":{"user_id":"` + testUserID + `"},"seq":1}`,
		"posted":       `{"event":"posted","data":{"post":"{\"id\":\"pppppppppppppppppppppppppp\"}"},"seq":2}`,
	}

	for name, raw := range events {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", name, r)
				}
			}()

			m.maintainUsersCache(t.Context(), mustEvent(t, raw))
			m.syncJoinedChannelsCache(mustEvent(t, raw))
		}()
	}
}

// Naming a group channel reads the users and channels caches while events update them.
func TestGroupChannelNameWhileCachesChange(t *testing.T) {
	f := newFakeServer(t)
	m := newTestClient(f)

	m.Users.mu.Lock()
	m.Users.channelData[testGroupID] = &model.Channel{Id: testGroupID, Type: model.ChannelTypeGroup, Name: "group"}
	m.Users.channels[testGroupID] = map[string]struct{}{testUserID: {}, testOtherID: {}}
	m.Users.users[testUserID] = &model.User{Id: testUserID, Username: "me"}
	m.Users.users[testOtherID] = &model.User{Id: testOtherID, Username: "other"}
	m.Users.mu.Unlock()

	stop := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}

			id := fmt.Sprintf("%026d", i%50)
			m.UpdateUser(&model.User{Id: id, Username: "u" + id})
			m.UpdateChannelUsersCache(testGroupID, &model.User{Id: id, Username: "u" + id})
		}
	})

	end := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(end) {
		_ = m.GetCachedChannelName(testGroupID)
		_ = m.GetChannelName(t.Context(), testGroupID)
	}

	close(stop)
	wg.Wait()
}

// parseMessage adds fields to the data of an event, which maintainUsersCache reads.
func TestPostedEventsHandledConcurrently(t *testing.T) {
	f := newFakeServer(t)
	m, cancel := login(t, f)

	defer cancel()
	defer func() { _ = m.Logout(t.Context()) }()

	const posts = 30

	for i := range posts {
		f.send(postedEvent(i))
	}

	timeout := time.After(testTimeout)

	for received := range posts {
		select {
		case <-m.MessageChan:
		case <-timeout:
			t.Fatalf("received %d of %d messages", received, posts)
		}
	}

	time.Sleep(200 * time.Millisecond)
}

// clientGoroutines returns the stacks of the long running goroutines of a
// logged in client: its own and those of the Mattermost websocket client.
func clientGoroutines() []string {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]

	matcher := regexp.MustCompile(`matterclient\.\(\*Client\)\.(WsReceiver|checkAlive|checkConnection|syncPeriodicStatus)\(` +
		`|model\.\(\*WebSocketClient\)\.`)

	var running []string

	for stack := range strings.SplitSeq(string(buf), "\n\n") {
		if matcher.MatchString(stack) {
			running = append(running, stack)
		}
	}

	return running
}

func requireNoClientGoroutines(t *testing.T) {
	t.Helper()

	var running []string

	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		running = clientGoroutines()
		if len(running) == 0 {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("%d goroutines still running:\n%s", len(running), strings.Join(running, "\n\n"))
}

// The websocket client blocks on its full event channel when nobody receives
// from it anymore, which kept its goroutines running after Logout.
func TestLogoutWithUnreceivedEvents(t *testing.T) {
	f := newFakeServer(t)

	for range 3 {
		m, cancel := login(t, f)

		// stop the receiver, then queue more events than the websocket client buffers
		cancel()
		time.Sleep(100 * time.Millisecond)

		for range 400 {
			f.send(typingEvent())
		}

		time.Sleep(200 * time.Millisecond)

		_ = m.Logout(t.Context())
	}

	requireNoClientGoroutines(t)
}

// Logging in again without a logout must not leave the goroutines of the first login running.
func TestLoginTwice(t *testing.T) {
	f := newFakeServer(t)

	m := newTestClient(f)
	m.Ctx = t.Context()

	for range 3 {
		err := m.Login(t.Context())
		if err != nil {
			t.Fatalf("login failed: %v", err)
		}

		time.Sleep(100 * time.Millisecond)
	}

	_ = m.Logout(t.Context())

	requireNoClientGoroutines(t)
}

// Every reconnect must replace the goroutines of the previous connection.
func TestReconnect(t *testing.T) {
	f := newFakeServer(t)
	m, cancel := login(t, f)

	defer cancel()

	time.Sleep(200 * time.Millisecond)

	connected := len(clientGoroutines())

	for i := range int32(3) {
		m.Reconnect(t.Context())
		waitFor(t, "the websocket to reconnect", func() bool { return f.wsCount.Load() == i+2 })
	}

	time.Sleep(500 * time.Millisecond)

	running := clientGoroutines()
	if len(running) != connected {
		t.Errorf("%d goroutines running after reconnecting, %d when first connected:\n%s",
			len(running), connected, strings.Join(running, "\n\n"))
	}

	_ = m.Logout(t.Context())

	requireNoClientGoroutines(t)
}
