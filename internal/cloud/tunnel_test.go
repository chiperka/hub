package cloud

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSHServer plays the cloud server's sshd: it accepts only the hub key,
// serves tcpip-forward, and exposes the forwarded port on a local listener.
type fakeSSHServer struct {
	ln        net.Listener
	forwardLn chan net.Listener // the "127.0.0.1:18080" the worker would call
	conns     atomic.Int32
}

func startFakeSSHServer(t *testing.T, hostSigner ssh.Signer, hubKey ssh.PublicKey) *fakeSSHServer {
	t.Helper()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == tunnelUser && string(k.Marshal()) == string(hubKey.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSSHServer{ln: ln, forwardLn: make(chan net.Listener, 4)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, cfg)
		}
	}()
	return f
}

func (f *fakeSSHServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	f.conns.Add(1)
	go ssh.DiscardRequests(nil)
	go func() {
		for ch := range chans {
			ch.Reject(ssh.Prohibited, "no sessions")
		}
	}()
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			var p struct {
				Addr string
				Port uint32
			}
			ssh.Unmarshal(req.Payload, &p)
			fl, _ := net.Listen("tcp", "127.0.0.1:0")
			req.Reply(true, nil)
			f.forwardLn <- fl
			go func() {
				for {
					in, err := fl.Accept()
					if err != nil {
						return
					}
					origin := in.RemoteAddr().(*net.TCPAddr)
					payload := ssh.Marshal(struct {
						Addr       string
						Port       uint32
						OriginAddr string
						OriginPort uint32
					}{p.Addr, p.Port, origin.IP.String(), uint32(origin.Port)})
					ch, chReqs, err := sc.OpenChannel("forwarded-tcpip", payload)
					if err != nil {
						in.Close()
						continue
					}
					go ssh.DiscardRequests(chReqs)
					go func() { io.Copy(ch, in); ch.CloseWrite() }()
					go func() { io.Copy(in, ch); in.Close() }()
				}
			}()
		case "keepalive@openssh.com":
			req.Reply(true, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
	sc.Close()
}

func TestTunnelForwardsWorkerTrafficToHub(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hub:%s", r.URL.Path)
	}))
	defer hub.Close()

	_, _, hubSigner, _ := newKey("hub")
	_, hostPub, hostSigner, _ := newKey("host")
	hostKey, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(hostPub))
	srv := startFakeSSHServer(t, hostSigner, hubSigner.PublicKey())

	var booted, up atomic.Int32
	tn := &tunnel{
		serverID: "ci-test", addr: srv.ln.Addr().String(), hostKey: hostKey, signer: hubSigner,
		hubAddr: hub.Listener.Addr().String(), logf: t.Logf,
		onBoot: func() { booted.Add(1) }, onUp: func(bool) { up.Add(1) }, onDown: func(error) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tn.start(ctx)
	defer tn.stop()

	var fl net.Listener
	select {
	case fl = <-srv.forwardLn:
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel never asked the server to listen")
	}
	// The worker on the cloud server calls its loopback; it must land in the hub.
	res, err := http.Get("http://" + fl.Addr().String() + "/api/workers/register")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "hub:/api/workers/register" {
		t.Fatalf("got %q", body)
	}
	if booted.Load() != 1 || up.Load() != 1 {
		t.Fatalf("callbacks: booted=%d up=%d", booted.Load(), up.Load())
	}
}

func TestTunnelRejectsUnexpectedHostKey(t *testing.T) {
	_, _, hubSigner, _ := newKey("hub")
	_, _, realHost, _ := newKey("host")
	_, otherPub, _, _ := newKey("someone-else")
	pinned, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(otherPub))
	srv := startFakeSSHServer(t, realHost, hubSigner.PublicKey())

	tn := &tunnel{addr: srv.ln.Addr().String(), hostKey: pinned, signer: hubSigner}
	if _, err := tn.dial(context.Background()); err == nil {
		t.Fatal("must refuse a server whose host key differs from the pinned one")
	}
}
