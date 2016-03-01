// Package sshd implements an SSH server.
//
// See https://tools.ietf.org/html/rfc4254
//
// This was copied over (and effectively forked from) cookoo-ssh. Mainly this
// differs from the cookoo-ssh version in that this does not act like a
// stand-alone SSH server.
package sshd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Masterminds/cookoo"
	"github.com/Masterminds/cookoo/log"
	"github.com/Masterminds/cookoo/safely"
	"github.com/deis/builder/pkg/cleaner"
	"golang.org/x/crypto/ssh"
)

const (
	// HostKeys is the context key for Host Keys list.
	HostKeys string = "ssh.HostKeys"
	// Address is the context key for SSH address.
	Address string = "ssh.Address"
	// ServerConfig is the context key for ServerConfig object.
	ServerConfig string = "ssh.ServerConfig"

	multiplePush string = "Another git push is ongoing"

	inProgressDelete string = "This app was deleted and is being cleaned up. Please re-create it with 'deis create your_app'"
)

// Serve starts a native SSH server.
//
// The general design of the server is that it acts as a main server for
// a Cookoo app. It assumes that certain things have been configured for it,
// like an ssh.ServerConfig. Once it runs, it will block until the main
// process terminates. If you want to stop it prior to that, you can grab
// the closer ("sshd.Closer") out of the context and send it a signal.
//
// Currently, the service is not generic. It only runs git hooks.
//
// This expects the following Context variables.
// 	- ssh.Hostkeys ([]ssh.Signer): Host key, as an unparsed byte slice.
// 	- ssh.Address (string): Address/port
// 	- ssh.ServerConfig (*ssh.ServerConfig): The server config to use.
//
// This puts the following variables into the context:
// 	- ssh.Closer (chan interface{}): Send a message to this to shutdown the server.
func Serve(
	reg *cookoo.Registry,
	router *cookoo.Router,
	serverCircuit *Circuit,
	gitHomeDir string,
	concurrentPushLock RepositoryLock,
	cleanerRef cleaner.Ref,
	c cookoo.Context) cookoo.Interrupt {

	hostkeys := c.Get(HostKeys, []ssh.Signer{}).([]ssh.Signer)
	addr := c.Get(Address, "0.0.0.0:2223").(string)
	cfg := c.Get(ServerConfig, &ssh.ServerConfig{}).(*ssh.ServerConfig)

	for _, hk := range hostkeys {
		cfg.AddHostKey(hk)
		log.Infof(c, "Added hostkey.")
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	srv := &server{
		c:          c,
		gitHome:    gitHomeDir,
		pushLock:   concurrentPushLock,
		cleanerRef: cleanerRef,
	}

	closer := make(chan interface{}, 1)
	c.Put("sshd.Closer", closer)

	log.Infof(c, "Listening on %s", addr)
	serverCircuit.Close()
	srv.listen(listener, cfg, closer)

	return nil
}

// server is the struct that encapsulates the SSH server.
type server struct {
	c          cookoo.Context
	gitHome    string
	pushLock   RepositoryLock
	cleanerRef cleaner.Ref
}

// listen handles accepting and managing connections. However, since closer
// is len(1), it will not block the sender.
func (s *server) listen(l net.Listener, conf *ssh.ServerConfig, closer chan interface{}) error {
	cxt := s.c
	log.Info(cxt, "Accepting new connections.")
	defer l.Close()

	// FIXME: Since Accept blocks, closer may not be checked often enough.
	for {
		log.Info(cxt, "Checking closer.")
		if len(closer) > 0 {
			<-closer
			log.Info(cxt, "Shutting down SSHD listener.")
			return nil
		}
		conn, err := l.Accept()
		if err != nil {
			log.Warnf(cxt, "Error during Accept: %s", err)
			// We shouldn't kill the listener because of an error.
			return err
		}
		safely.GoDo(cxt, func() {
			s.handleConn(conn, conf)
		})
	}
}

// handleConn handles an individual client connection.
//
// It manages the connection, but passes channels on to `answer()`.
func (s *server) handleConn(conn net.Conn, conf *ssh.ServerConfig) {
	defer conn.Close()
	log.Info(s.c, "Accepted connection.")
	_, chans, reqs, err := ssh.NewServerConn(conn, conf)
	if err != nil {
		// Handshake failure.
		log.Debugf(s.c, "Failed handshake: %s", err)
		return
	}

	// Discard global requests. We're only concerned with channels.
	safely.GoDo(s.c, func() { ssh.DiscardRequests(reqs) })

	condata := sshConnection(conn)

	// Now we handle the channels.
	for incoming := range chans {
		log.Infof(s.c, "Channel type: %s\n", incoming.ChannelType())
		if incoming.ChannelType() != "session" {
			incoming.Reject(ssh.UnknownChannelType, "Unknown channel type")
		}

		channel, req, err := incoming.Accept()
		if err != nil {
			// Should close request and move on.
			panic(err)
		}
		safely.GoDo(s.c, func() { s.answer(channel, req, condata) })
	}
	conn.Close()
}

// sshConnection generates the SSH_CONNECTION environment variable.
//
// This is untested on UNIX sockets.
func sshConnection(conn net.Conn) string {
	remote := conn.RemoteAddr().String()
	local := conn.LocalAddr().String()
	rhost, rport, _ := net.SplitHostPort(remote)
	lhost, lport, _ := net.SplitHostPort(local)

	return fmt.Sprintf("%s %s %s %s", rhost, rport, lhost, lport)
}

func sendExitStatus(status uint32, channel ssh.Channel) error {
	exit := struct{ Status uint32 }{uint32(0)}
	_, err := channel.SendRequest("exit-status", false, ssh.Marshal(exit))
	return err
}

func (s *server) withCleanerLock(f func() error) error {
	s.cleanerRef.Lock()
	defer s.cleanerRef.Unlock()
	return f()
}

// answer handles answering requests and channel requests
//
// Currently, an exec must be either "ping", "git-receive-pack" or
// "git-upload-pack". Anything else will result in a failure response. Right
// now, we leave the channel open on failure because it is unclear what the
// correct behavior for a failed exec is.
//
// Support for setting environment variables via `env` has been disabled.
func (s *server) answer(channel ssh.Channel, requests <-chan *ssh.Request, sshConn string) error {
	defer channel.Close()

	// Answer all the requests on this connection.
	for req := range requests {
		ok := false

		// I think that ideally what we want to do here is pass this on to
		// the Cookoo router and let it handle each Type on its own.
		switch req.Type {
		case "env":
			o := &EnvVar{}
			ssh.Unmarshal(req.Payload, o)
			fmt.Printf("Key='%s', Value='%s'\n", o.Name, o.Value)
			req.Reply(true, nil)
		case "exec":
			clean := cleanExec(req.Payload)
			parts := strings.SplitN(clean, " ", 2)

			router := s.c.Get("cookoo.Router", nil).(*cookoo.Router)

			// TODO: Should we unset the context value 'cookoo.Router'?
			// We need a shallow copy of the context to avoid race conditions.
			cxt := s.c.Copy()
			cxt.Put("SSH_CONNECTION", sshConn)

			// Only allow commands that we know about.
			switch parts[0] {
			case "ping":
				cxt.Put("channel", channel)
				cxt.Put("request", req)
				sshPing := cxt.Get("route.sshd.sshPing", "sshPing").(string)
				err := router.HandleRequest(sshPing, cxt, true)
				if err != nil {
					log.Warnf(s.c, "Error pinging: %s", err)
				}
				return err
			case "git-receive-pack", "git-upload-pack":
				if len(parts) < 2 {
					log.Warn(s.c, "Expected two-part command.\n")
					req.Reply(ok, nil)
					break
				}

				repoName := parts[1]
				errConcurrentPush := errors.New("concurrent push")
				err := s.withCleanerLock(func() error {
					if err := s.pushLock.Lock(repoName, time.Duration(0)); err != nil {
						return errConcurrentPush
					}

					req.Reply(true, nil) // We processed. Yay.

					cxt.Put("channel", channel)
					cxt.Put("request", req)
					cxt.Put("operation", parts[0])
					cxt.Put("repository", parts[1])
					sshGitReceive := cxt.Get("route.sshd.sshGitReceive", "sshGitReceive").(string)
					err := router.HandleRequest(sshGitReceive, cxt, true)
					if err := s.pushLock.Unlock(repoName, time.Duration(0)); err != nil {
						log.Errf(s.c, "unable to unlock repository lock for %s (%s)", repoName, err)
						// TODO: this is an important error case that needs to be covered
						// Probably the best solution is to change the lock into a lease so that even on unlock failures, RepositoryLock will eventually yield
					}
					return err
				})

				if err == errConcurrentPush {
					log.Errf(s.c, multiplePush)
					// The error must be in git format
					if err := gitPktLine(channel, fmt.Sprintf("ERR %v\n", multiplePush)); err != nil {
						log.Errf(s.c, "Failed to write to channel: %s", err)
					}
					sendExitStatus(1, channel)
					req.Reply(false, nil)
					return nil
				}

				var xs uint32
				if err != nil {
					log.Errf(s.c, "Failed git receive: %v", err)
					xs = 1
				}
				sendExitStatus(xs, channel)
				return nil
			default:
				log.Warnf(s.c, "Illegal command is '%s'\n", clean)
				req.Reply(false, nil)
				return nil
			}

			if err := sendExitStatus(0, channel); err != nil {
				log.Errf(s.c, "Failed to write exit status: %s", err)
			}
			return nil
		default:
			// We simply ignore all of the other cases and leave the
			// channel open to take additional requests.
			log.Infof(s.c, "Received request of type %s\n", req.Type)
			req.Reply(false, nil)
		}
	}

	return nil
}

// ExecCmd is an SSH exec request
type ExecCmd struct {
	Value string
}

// EnvVar is an SSH env request
type EnvVar struct {
	Name  string
	Value string
}

// GenericMessage describes a simple string message, which is common in SSH.
type GenericMessage struct {
	Value string
}

// cleanExec cleans the exec string.
func cleanExec(pay []byte) string {
	e := &ExecCmd{}
	ssh.Unmarshal(pay, e)
	// TODO: Minimal escaping of values in command. There is probably a better
	// way of doing this.
	r := strings.NewReplacer("$", "", "`", "'")
	return r.Replace(e.Value)
}

// Ping handles a simple test SSH exec.
//
// Returns the string PONG and exit status 0.
//
// Params:
// 	- channel (ssh.Channel): The channel to respond on.
// 	- request (*ssh.Request): The request.
//
func Ping(c cookoo.Context, p *cookoo.Params) (interface{}, cookoo.Interrupt) {
	channel := p.Get("channel", nil).(ssh.Channel)
	req := p.Get("request", nil).(*ssh.Request)
	log.Info(c, "PING\n")
	if _, err := channel.Write([]byte("pong")); err != nil {
		log.Errf(c, "Failed to write to channel: %s", err)
	}
	sendExitStatus(0, channel)
	req.Reply(true, nil)
	return nil, nil
}

// gitPktLine writes a line following the pkt-line git protocol. See https://github.com/git/git/blob/master/Documentation/technical/protocol-common.txt for the protocol and https://github.com/git/git/blob/master/Documentation/technical/pack-protocol.txt for its usage.
func gitPktLine(w io.Writer, s string) error {
	_, err := fmt.Fprintf(w, "%04x%s", len(s)+4, s)
	return err
}
