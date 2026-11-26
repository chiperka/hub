package cloud

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// tunnel keeps a reverse SSH tunnel to one cloud server: the hub dials the
// server, asks it to listen on 127.0.0.1:tunnelPort, and forwards each
// connection to the hub's own HTTP listener. Nothing listens on the hub side,
// so the hub can stay behind a VPN with no inbound ports.
type tunnel struct {
	serverID string
	addr     string        // server public IP:22
	hostKey  ssh.PublicKey // pinned: the hub generated it and handed it over in user data
	signer   ssh.Signer
	hubAddr  string // where forwarded connections go, e.g. 127.0.0.1:8080

	// Callbacks; each is called from the tunnel goroutine.
	onBoot func()      // first successful SSH handshake: the server booted
	onUp   func(bool)  // tunnel listening; true on the first time, false when restored
	onDown func(error) // an established tunnel dropped
	logf   func(string, ...any)

	cancel context.CancelFunc
	done   chan struct{}
}

func (t *tunnel) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	t.done = make(chan struct{})
	go t.run(ctx)
}

func (t *tunnel) stop() {
	if t.cancel != nil {
		t.cancel()
		<-t.done
	}
}

func (t *tunnel) run(ctx context.Context) {
	defer close(t.done)
	booted, upOnce := false, false
	for ctx.Err() == nil {
		client, err := t.dial(ctx)
		if err != nil {
			// Expected while the server boots; keep trying quietly.
			sleep(ctx, 3*time.Second)
			continue
		}
		if !booted {
			booted = true
			t.onBoot()
		}
		ln, err := client.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tunnelPort))
		if err != nil {
			t.logf("tunnel listen on %s failed: %v", t.serverID, err)
			client.Close()
			sleep(ctx, 3*time.Second)
			continue
		}
		t.onUp(!upOnce)
		upOnce = true
		err = t.serve(ctx, client, ln)
		ln.Close()
		client.Close()
		if ctx.Err() == nil {
			t.onDown(err)
			sleep(ctx, time.Second)
		}
	}
}

func (t *tunnel) dial(ctx context.Context) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:              tunnelUser,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(t.signer)},
		HostKeyCallback:   ssh.FixedHostKey(t.hostKey),
		HostKeyAlgorithms: []string{t.hostKey.Type()},
		Timeout:           10 * time.Second,
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, err
	}
	// ClientConfig.Timeout covers only ssh.Dial; bound the handshake ourselves
	// so a silent peer cannot block stop().
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, t.addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// serve forwards connections until the SSH connection dies or ctx ends.
func (t *tunnel) serve(ctx context.Context, client *ssh.Client, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx) // stops the keepalive when serve returns
	defer cancel()
	errc := make(chan error, 3) // one slot per sender: none of them may block
	go func() { errc <- client.Wait() }()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				// Detect a dead peer quickly so the worker barely notices an outage.
				done := make(chan error, 1)
				go func() { _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); done <- err }()
				select {
				case err := <-done:
					if err != nil {
						errc <- err
						return
					}
				case <-time.After(15 * time.Second):
					errc <- fmt.Errorf("keepalive timed out")
					client.Close()
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		var wg sync.WaitGroup
		for {
			remote, err := ln.Accept()
			if err != nil {
				errc <- err
				wg.Wait()
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				t.forward(remote)
			}()
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func (t *tunnel) forward(remote net.Conn) {
	defer remote.Close()
	local, err := net.DialTimeout("tcp", t.hubAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer local.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(local, remote); closeWrite(local); done <- struct{}{} }()
	go func() { io.Copy(remote, local); closeWrite(remote); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
