package sshell

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/awgh/sshell/commands"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func init() {
	commands.RegisterCommand("test", cmdTest, nil)
	commands.RegisterCommand("exit", cmdExit, nil)
}

var errExitApp = errors.New("exiting")

// SSHell settings struct
type SSHell struct {
	User, Password string
	Port           int
	Running        bool
	Prompt         string
}

// NewSSHell - create a SSHell with default settings
func NewSSHell() *SSHell {
	s := new(SSHell)
	return s
}

// Listen starts the server
func (s *SSHell) Listen() {

	portString := strconv.Itoa(s.Port)
	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if subtle.ConstantTimeCompare([]byte(c.User()), []byte(s.User)) == 1 &&
				subtle.ConstantTimeCompare(pass, []byte(s.Password)) == 1 {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected for %q", c.User())
		},
	}
	_, privateBytes, err := GetKeyPair("id_rsa")
	if err != nil {
		log.Fatal("Failed to load private key (./id_rsa)")
	}
	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		log.Fatal("Failed to parse private key")
	}
	config.AddHostKey(private)
	listener, err := net.Listen("tcp", "0.0.0.0:"+portString)
	if err != nil {
		log.Fatalf("Failed to listen on %s (%s)", portString, err)
	}
	log.Printf("Listening on %s...\n", portString)
	s.Running = true
	for {
		if !s.Running {
			break
		}
		tcpConn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept incoming connection (%s)", err)
			continue
		}
		sshConn, chans, reqs, err := ssh.NewServerConn(tcpConn, config)
		if err != nil {
			log.Printf("Failed to handshake (%s)", err)
			continue
		}

		log.Printf("New SSH connection from %s (%s)\n", sshConn.RemoteAddr(), sshConn.ClientVersion())
		go ssh.DiscardRequests(reqs)
		go s.handleChannels(chans)
	}
}

func (s *SSHell) handleChannels(chans <-chan ssh.NewChannel) {
	for newChannel := range chans {
		go s.handleChannel(newChannel)
	}
}

func (s *SSHell) handleChannel(newChannel ssh.NewChannel) {
	if t := newChannel.ChannelType(); t != "session" {
		newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", t))
		return
	}
	connection, requests, err := newChannel.Accept()
	if err != nil {
		log.Printf("Could not accept channel (%s)", err)
		return
	}

	c := make(chan *ssh.Request, 2)
	for req := range requests {
		switch req.Type {
		case "shell":
			if len(req.Payload) == 0 {
				req.Reply(true, nil)
				go func() {
					defer connection.Close()
					s.serveTerminal(connection, c)
				}()
			}
		case "subsystem":
			if string(req.Payload[4:]) == "sftp" {
				req.Reply(true, nil)
				go func() {
					defer connection.Close()
					defer connection.CloseWrite()
					s.serveSFTP(connection)
				}()
			}
		case "pty-req":
			c <- req // we have not created the pty yet, pass along

		case "window-change":
			c <- req // we have not created the pty yet, pass along
		}
	}
}

func (s *SSHell) serveTerminal(connection ssh.Channel, oldrequests <-chan *ssh.Request) {

	term := terminal.NewTerminal(connection, s.Prompt)
	term.AutoCompleteCallback = s.autoCompleteCallback

	go func() { // OOB requests
		for req := range oldrequests {
			switch req.Type {
			case "pty-req":
				termLen := req.Payload[3]
				w, h := parseDims(req.Payload[termLen+4:])
				term.SetSize(int(w), int(h))
				req.Reply(true, nil)
			case "window-change":
				w, h := parseDims(req.Payload)
				term.SetSize(int(w), int(h))
			}
		}
	}()

	for {
		line, err := term.ReadLine()
		if err != nil {
			log.Printf("channel read error (%s)", err)
			break
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		cmd, args := f[0], f[1:]
		if _, c, ok := commands.LookupCommand(cmd); ok {
			err = c.Run(term, args)
			if err == errExitApp {
				term.Write([]byte("Exiting." + "\n"))
				return
			}
		} else {
			term.Write([]byte("Unknown command: " + line + "\n"))
		}
	}
}

func (s *SSHell) serveSFTP(channel ssh.Channel) {

	serverOptions := []sftp.ServerOption{}

	server, err := sftp.NewServer(
		channel,
		serverOptions...,
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := server.Serve(); err == io.EOF {
		server.Close()
		log.Print("sftp client exited session.")
	} else if err != nil {
		log.Fatal("sftp server completed with error:", err)
	}
}

func (s *SSHell) autoCompleteCallback(line string, pos int, key rune) (newLine string, newPos int, ok bool) {
	if key != '\t' || pos != len(line) {
		return
	}
	lastWord := regexp.MustCompile(`.+\W(\w+)$`)
	// Auto-complete for the command itself.
	if !strings.Contains(line, " ") {
		var name string
		name, _, ok = commands.LookupCommand(line)
		if !ok {
			return
		}
		return name, len(name), true
	}
	_, c, ok := commands.LookupCommand(line[:strings.IndexByte(line, ' ')])
	if !ok || c.Complete == nil {
		return
	}
	if strings.HasSuffix(line, " ") {
		return line, pos, true
	}
	m := lastWord.FindStringSubmatch(line)
	if m == nil {
		return line, len(line), true
	}
	soFar := m[1]
	var match []string
	for _, cand := range c.Complete() {
		if len(soFar) > len(cand) || !strings.EqualFold(cand[:len(soFar)], soFar) {
			continue
		}
		match = append(match, cand)
	}
	if len(match) == 0 {
		return
	}
	if len(match) > 1 {
		return line, pos, true
	}
	newLine = line[:len(line)-len(soFar)] + match[0]
	return newLine, len(newLine), true
}

func cmdTest(term io.Writer, args []string) error {
	msg := fmt.Sprintf("Test: %+v", args)
	term.Write([]byte(msg + "\n"))
	return nil
}

func cmdExit(term io.Writer, args []string) error {
	return errExitApp
}

func parseDims(b []byte) (uint32, uint32) {
	w := binary.BigEndian.Uint32(b)
	h := binary.BigEndian.Uint32(b[4:])
	return w, h
}
