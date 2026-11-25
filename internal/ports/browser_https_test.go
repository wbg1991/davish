package ports

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type oneClient struct{ c *ssh.Client }

func (o oneClient) Client(string) (*ssh.Client, error) { return o.c, nil }

// loopbackSSH is an SSH server inside the test that only forwards
// connections, and an SSH client logged in to it.
func loopbackSSH(t *testing.T) *ssh.Client {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	conf := &ssh.ServerConfig{NoClientAuth: true}
	conf.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		_, chans, reqs, err := ssh.NewServerConn(nc, conf)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nch := range chans {
			var to struct {
				Host     string
				Port     uint32
				From     string
				FromPort uint32
			}
			if nch.ChannelType() != "direct-tcpip" || ssh.Unmarshal(nch.ExtraData(), &to) != nil {
				_ = nch.Reject(ssh.UnknownChannelType, "forwarding only")
				continue
			}
			rc, err := net.Dial("tcp", net.JoinHostPort(to.Host, strconv.Itoa(int(to.Port))))
			if err != nil {
				_ = nch.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, chReqs, err := nch.Accept()
			if err != nil {
				_ = rc.Close()
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go func() { _, _ = io.Copy(rc, ch); _ = rc.Close() }()
			go func() { _, _ = io.Copy(ch, rc); _ = ch.Close() }()
		}
	}()
	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func serverPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

type page struct {
	status int
	header http.Header
	body   string
}

// get loads path through the site's listener the way the frame does.
func get(t *testing.T, s Site, path string) page {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(s.LocalPort)+path+"?"+tokenParam+"="+s.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return page{resp.StatusCode, resp.Header, string(body)}
}

// A server's app that only speaks HTTPS, with a certificate nothing trusts,
// opens through a secure site: pages, redirects and Secure cookies.
func TestBrowserHTTPS(t *testing.T) {
	var port int
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{
				Name: "session", Value: "s1", Path: "/",
				Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "https://localhost:"+strconv.Itoa(port)+"/home", http.StatusFound)
		default:
			cookie := ""
			if c, err := r.Cookie("session"); err == nil {
				cookie = c.Value
			}
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Strict-Transport-Security", "max-age=60")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; upgrade-insecure-requests; frame-ancestors 'none'")
			_, _ = io.WriteString(w, "<head></head>host="+r.Host+" cookie="+cookie) //nolint:gosec // the test's own page
		}
	}))
	t.Cleanup(app.Close)
	port = serverPort(t, app.URL)

	b := NewBrowser(oneClient{loopbackSSH(t)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(b.Close)
	site, err := b.Open("s", port, true)
	if err != nil {
		t.Fatal(err)
	}
	if !site.Secure {
		t.Error("site is not marked secure")
	}

	login := get(t, site, "/login")
	want := "http://127.0.0.1:" + strconv.Itoa(site.LocalPort) + "/home?" + tokenParam + "=" + site.Token
	if got := login.header.Get("Location"); got != want {
		t.Errorf("Location = %s, want %s", got, want)
	}
	if login.header.Get("Set-Cookie") != "" {
		t.Error("the cookie reached the frame")
	}

	home := get(t, site, "/home")
	if home.status != http.StatusOK {
		t.Fatalf("status %d: %s", home.status, home.body)
	}
	for _, part := range []string{"host=localhost:" + strconv.Itoa(port), "cookie=s1", navPath} {
		if !strings.Contains(home.body, part) {
			t.Errorf("body %q lacks %q", home.body, part)
		}
	}
	if got := home.header.Get("Content-Security-Policy"); got != "default-src 'self';" {
		t.Errorf("CSP = %q", got)
	}
	if home.header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS was passed on")
	}

	// The same port over HTTP is a listener of its own, and is refused by an
	// HTTPS-only app.
	plain, err := b.Open("s", port, false)
	if err != nil {
		t.Fatal(err)
	}
	if plain.LocalPort == site.LocalPort {
		t.Error("HTTP and HTTPS share a listener")
	}
	if got := get(t, plain, "/home"); got.status == http.StatusOK {
		t.Error("plain HTTP reached the HTTPS app")
	}
}

// An HTTP app that sends the visitor to HTTPS gets a secure listener.
func TestBrowserRedirectToHTTPS(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure page")
	}))
	t.Cleanup(secure.Close)
	plain := httptest.NewServer(http.RedirectHandler(
		"https://127.0.0.1:"+strconv.Itoa(serverPort(t, secure.URL))+"/", http.StatusMovedPermanently))
	t.Cleanup(plain.Close)

	b := NewBrowser(oneClient{loopbackSSH(t)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(b.Close)
	site, err := b.Open("s", serverPort(t, plain.URL), false)
	if err != nil {
		t.Fatal(err)
	}
	next, err := url.Parse(get(t, site, "/").header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range b.Sites() {
		if strconv.Itoa(s.LocalPort) != next.Port() {
			continue
		}
		if !s.Secure || s.RemotePort != serverPort(t, secure.URL) {
			t.Fatalf("redirected to %+v", s)
		}
		if got := get(t, s, "/"); got.body != "secure page" {
			t.Errorf("body = %q", got.body)
		}
		return
	}
	t.Fatalf("no listener for %s", next)
}

// A websocket (hot reload) is upgraded over TLS and carries bytes both ways.
func TestBrowserHTTPSUpgrade(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nc, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer nc.Close() //nolint:errcheck // test
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = rw.WriteString("echo " + line)
		_ = rw.Flush()
	}))
	t.Cleanup(app.Close)

	b := NewBrowser(oneClient{loopbackSSH(t)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(b.Close)
	site, err := b.Open("s", serverPort(t, app.URL), true)
	if err != nil {
		t.Fatal(err)
	}
	self := "127.0.0.1:" + strconv.Itoa(site.LocalPort)
	nc, err := net.Dial("tcp", self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	if err := nc.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(nc, "GET /ws HTTP/1.1\r\nHost: "+self+"\r\nOrigin: http://"+self+
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, err := io.WriteString(nc, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := br.ReadString('\n'); err != nil || got != "echo hello\n" {
		t.Errorf("got %q, %v", got, err)
	}
}
