package daemonupdate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ListenerFD = 3
	LockFD     = 4
	ControlFD  = 5
)

type Message string

const (
	MessagePrepared Message = "prepared"
	MessageActivate Message = "activate"
	MessageReady    Message = "ready"
	MessageCommit   Message = "commit"
)

type controlMessage struct {
	Message Message `json:"message"`
	Error   string  `json:"error,omitempty"`
}

type Control struct {
	conn    net.Conn
	decoder *json.Decoder
	encoder *json.Encoder
}

func NewInheritedControl(file *os.File) (*Control, error) {
	if file == nil {
		return nil, errors.New("inherited update control file is required")
	}
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("adopt update control connection: %w", err)
	}
	return newControl(conn), nil
}

func newControl(conn net.Conn) *Control {
	return &Control{conn: conn, decoder: json.NewDecoder(bufio.NewReader(conn)), encoder: json.NewEncoder(conn)}
}

func (c *Control) Send(message Message, failure error) error {
	payload := controlMessage{Message: message}
	if failure != nil {
		payload.Error = failure.Error()
	}
	if err := c.encoder.Encode(payload); err != nil {
		return fmt.Errorf("send update handoff %s: %w", message, err)
	}
	return nil
}

func (c *Control) Expect(message Message, timeout time.Duration) error {
	if timeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
		defer c.conn.SetReadDeadline(time.Time{})
	}
	var payload controlMessage
	if err := c.decoder.Decode(&payload); err != nil {
		return fmt.Errorf("wait for update handoff %s: %w", message, err)
	}
	if payload.Message != message {
		return fmt.Errorf("wait for update handoff %s: received %s", message, payload.Message)
	}
	if payload.Error != "" {
		return fmt.Errorf("successor rejected handoff: %s", payload.Error)
	}
	return nil
}

func (c *Control) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

type Child struct {
	Command *exec.Cmd
	Control *Control
}

func Spawn(candidatePath string, daemonArgs []string, listenerFile, lockFile *os.File, stderr anyWriter) (*Child, error) {
	if candidatePath == "" || listenerFile == nil || lockFile == nil {
		return nil, errors.New("candidate, listener, and lock are required")
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create update control socket: %w", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "pika-update-parent")
	childFile := os.NewFile(uintptr(fds[1]), "pika-update-child")
	parentConn, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = childFile.Close()
		return nil, fmt.Errorf("open update control socket: %w", err)
	}
	args := append([]string{"daemon"}, daemonArgs...)
	args = append(args,
		"--handoff-listener-fd", fmt.Sprint(ListenerFD),
		"--handoff-lock-fd", fmt.Sprint(LockFD),
		"--handoff-control-fd", fmt.Sprint(ControlFD),
	)
	command := exec.Command(candidatePath, args...)
	command.ExtraFiles = []*os.File{listenerFile, lockFile, childFile}
	command.Stdout = os.Stdout
	if stderr != nil {
		command.Stderr = stderr
	} else {
		command.Stderr = os.Stderr
	}
	if err := command.Start(); err != nil {
		_ = parentConn.Close()
		_ = childFile.Close()
		return nil, fmt.Errorf("start successor daemon: %w", err)
	}
	_ = childFile.Close()
	return &Child{Command: command, Control: newControl(parentConn)}, nil
}

type anyWriter interface {
	Write([]byte) (int, error)
}

func DuplicateFile(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, errors.New("file to duplicate is required")
	}
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate inherited file: %w", err)
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}
