package vmodem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrConfigRequired         = errors.New("config required")
	ErrModemBusy              = errors.New("modem busy")
	ErrInvalidStateTransition = errors.New("invalid state transition")
	ErrNoCarrier              = errors.New("no carrier")
)

// ModemStatus represents the status of the modem
type ModemStatus int

const (
	StatusIdle ModemStatus = iota // Initial state
	StatusDialing
	StatusConnected
	StatusConnectedCmd
	StatusRinging
	StatusClosed // Terminal state, dead modem
)

func (ms ModemStatus) String() string {
	switch ms {
	case StatusIdle:
		return "Idle"
	case StatusDialing:
		return "Dialing"
	case StatusConnected:
		return "Connected"
	case StatusConnectedCmd:
		return "ConnectedCmd"
	case StatusRinging:
		return "Ringing"
	case StatusClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

type CmdReturn int

const (
	RetCodeOk CmdReturn = iota
	RetCodeError
	RetCodeSilent
	RetCodeConnect
	RetCodeNoCarrier
	RetCodeNoDialtone
	RetCodeBusy
	RetCodeNoAnswer
	RetCodeSkip
)

type Modem struct {
	sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	st           ModemStatus
	stCtx        context.Context
	stCtxCancel  context.CancelFunc
	tty          io.ReadWriteCloser
	conn         io.ReadWriteCloser
	outgoingCall OutgoingCallType
	commandHook  CommandHookType
	connectStr   string
	sregs        map[byte]byte
	echo         bool
	shortForm    bool
}

type OutgoingCallType func(m *Modem, number string) (io.ReadWriteCloser, error)
type CommandHookType func(m *Modem, cmdChar string, cmdNum string, cmdAssign bool, cmdQuery bool, cmdAssignVal string) CmdReturn

type ModemConfig struct {
	OutgoingCall OutgoingCallType
	CommandHook  CommandHookType
	TTY          io.ReadWriteCloser
	ConnectStr   string
}

func checkValidCmdChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func checkValidNumChar(b byte) bool {
	return (b >= '0' && b <= '9')
}

func (m *Modem) checkLock() {
	if m.TryLock() {
		panic("Modem lock not held")
	}
}

func (m *Modem) ttyWriteStr(s string) {
	fmt.Fprint(m.tty, s)
}

func (m *Modem) TtyWriteStr(s string) {
	m.checkLock()
	m.ttyWriteStr(s)
}

func (m *Modem) TtyWriteStrSync(s string) {
	m.Lock()
	defer m.Unlock()
	m.ttyWriteStr(s)
}

func (m *Modem) cr() string {
	if m.shortForm {
		return "\r"
	} else {
		return "\r\n"
	}
}

func (m *Modem) Cr() string {
	m.checkLock()
	return m.cr()
}

func (m *Modem) CrSync() string {
	m.Lock()
	defer m.Unlock()
	return m.cr()
}

func (m *Modem) printRetCode(ret CmdReturn) {
	if ret == RetCodeSilent {
		return
	}
	retStr := ""
	if m.shortForm {
		switch ret {
		case RetCodeOk:
			retStr = "0"
		case RetCodeError:
			retStr = "4"
		case RetCodeConnect:
			retStr = "1"
		case RetCodeNoCarrier:
			retStr = "3"
		case RetCodeNoDialtone:
			retStr = "6"
		case RetCodeBusy:
			retStr = "7"
		case RetCodeNoAnswer:
			retStr = "8"
		}
	} else {
		switch ret {
		case RetCodeOk:
			retStr = "OK"
		case RetCodeError:
			retStr = "ERROR"
		case RetCodeConnect:
			retStr = m.connectStr
		case RetCodeNoCarrier:
			retStr = "NO CARRIER"
		case RetCodeNoDialtone:
			retStr = "NO DIALTONE"
		case RetCodeBusy:
			retStr = "BUSY"
		case RetCodeNoAnswer:
			retStr = "NO ANSWER"
		}
	}
	m.ttyWriteStr(m.cr() + retStr + m.cr())
}

func (m *Modem) setStatus(status ModemStatus) {
	prevStatus := m.st
	if prevStatus == StatusClosed {
		panic(ErrInvalidStateTransition)
	}
	switch status {
	case StatusIdle:
		if prevStatus == StatusConnected || prevStatus == StatusConnectedCmd || prevStatus == StatusDialing {
			m.printRetCode(RetCodeNoCarrier)
		}
		if prevStatus == StatusConnected || prevStatus == StatusConnectedCmd || prevStatus == StatusRinging {
			m.conn.Close()
			m.conn = nil
		}

	case StatusConnected:
		if prevStatus != StatusDialing && prevStatus != StatusRinging && prevStatus != StatusConnectedCmd {
			panic(ErrInvalidStateTransition)
		}
		m.printRetCode(RetCodeConnect)
	case StatusConnectedCmd:
		if prevStatus != StatusConnected {
			panic(ErrInvalidStateTransition)
		}
		m.printRetCode(RetCodeOk)
	case StatusDialing:
		if prevStatus != StatusIdle {
			panic(ErrInvalidStateTransition)
		}
	case StatusRinging:
		if prevStatus != StatusIdle {
			panic(ErrInvalidStateTransition)
		}
	}
	m.stCtxCancel()
	m.stCtx, m.stCtxCancel = context.WithCancel(m.ctx)
	m.st = status
	fmt.Printf("Modem status transition: %v -> %v\n", prevStatus, status)
}

func (m *Modem) status() ModemStatus {
	return m.st
}

// Status returns the current status of the modem. Modem lock must be held.
func (m *Modem) Status() ModemStatus {
	m.checkLock()
	return m.status()
}

// StatusSync returns the current status of the modem. Modem lock is acquired and released.
func (m *Modem) StatusSync() ModemStatus {
	m.Lock()
	defer m.Unlock()
	return m.status()
}

func (m *Modem) close() {
	m.setStatus(StatusClosed)
	m.cancel()
	m.tty.Close()
}

// Close closes the modem. Modem lock must be held.
func (m *Modem) Close() {
	m.checkLock()
	m.close()
}

// CloseSync closes the modem. Modem lock is acquired and released.
func (m *Modem) CloseSync() {
	m.Lock()
	defer m.Unlock()
	m.close()
}

func (m *Modem) incomingCall(conn io.ReadWriteCloser) error {
	if m.status() != StatusIdle {
		return ErrModemBusy
	}
	m.setStatus(StatusRinging)
	m.conn = conn
	return nil
}

// IncomingCall simulates an incoming call. Modem lock must be held.
func (m *Modem) IncomingCall(conn io.ReadWriteCloser) error {
	m.checkLock()
	return m.incomingCall(conn)
}

// IncomingCallSync simulates an incoming call. Modem lock is acquired and released.
func (m *Modem) IncomingCallSync(conn io.ReadWriteCloser) error {
	m.Lock()
	defer m.Unlock()
	return m.incomingCall(conn)
}

func (m *Modem) processDialing(ctx context.Context, number string) {
	if ctx.Err() != nil {
		return
	}
	conn, err := m.outgoingCall(m, number)
	m.Lock()
	defer m.Unlock()
	if ctx.Err() != nil {
		if err == nil {
			conn.Close()
		}
		return
	}
	if err != nil {
		m.setStatus(StatusIdle)
		return
	}
	m.conn = conn
	m.setStatus(StatusConnected)
}

func (m *Modem) processCommand(cmdChar string, cmdNum string, cmdAssign bool, cmdQuery bool, cmdAssignVal string) CmdReturn {
	if m.commandHook != nil {
		r := m.commandHook(m, cmdChar, cmdNum, cmdAssign, cmdQuery, cmdAssignVal)
		if r != RetCodeSkip {
			return r
		}
	}
	switch cmdChar {
	case "S":
		r, _ := strconv.Atoi(cmdNum)
		if r < 0 || r > 255 {
			return RetCodeError
		}
		if cmdAssign {
			v, _ := strconv.Atoi(cmdAssignVal)
			if v < 0 || v > 255 {
				return RetCodeError
			}
			m.sregs[byte(r)] = byte(v)
			return RetCodeOk
		}
		if cmdQuery {
			v := m.sregs[byte(r)]
			m.ttyWriteStr(fmt.Sprintf(m.cr()+"%03d\r\n", v))
			return RetCodeOk
		}
	case "E":
		n, _ := strconv.Atoi(cmdNum)
		switch n {
		case 0:
			m.echo = false
		case 1:
			m.echo = true
		default:
			return RetCodeError
		}
	case "V":
		n, _ := strconv.Atoi(cmdNum)
		switch n {
		case 0:
			m.shortForm = true
		case 1:
			m.shortForm = false
		default:
			return RetCodeError
		}
	case "D":
		if m.status() != StatusIdle {
			return RetCodeError
		}
		if m.outgoingCall != nil {
			m.setStatus(StatusDialing)
			go m.processDialing(m.stCtx, cmdAssignVal)
			return RetCodeSilent
		}
		return RetCodeNoCarrier
	case "A":
		if m.status() == StatusIdle {
			return RetCodeNoCarrier
		}
		if m.status() != StatusRinging {
			return RetCodeError
		}
		m.setStatus(StatusConnected)
		return RetCodeSilent
	case "H":
		if m.status() == StatusConnected || m.status() == StatusConnectedCmd {
			m.setStatus(StatusIdle)
			return RetCodeSilent
		}
	case "O":
		if m.status() != StatusConnectedCmd {
			return RetCodeError
		}
		m.setStatus(StatusConnected)
		return RetCodeSilent
	}
	return RetCodeOk
}

func (m *Modem) processAtCommand(cmd string) CmdReturn {
	if m.status() != StatusIdle && m.status() != StatusConnectedCmd {
		return RetCodeError
	}
	cmdBuf := bytes.NewBufferString(cmd)
	cmdRet := RetCodeOk
	e := false
	for cmdBuf.Len() > 0 && !e {
		cmdChar := ""
		cmdNum := ""
		cmdLong := false
		cmdAssign := false
		cmdQuery := false
		cmdAssignVal := ""

		for cmdBuf.Len() > 0 && !e {
			b, err := cmdBuf.ReadByte()
			if err != nil {
				e = true
				break
			}

			if b == '?' {
				if cmdChar != "" {
					cmdQuery = true
					break
				} else {
					e = true
					break
				}
			}

			if cmdAssign {
				if !cmdLong && !checkValidNumChar(b) { // short command only accepts numbers
					cmdBuf.UnreadByte()
					break
				}
				cmdAssignVal += string(b)
				continue
			}

			if b == '+' || b == '#' {
				if cmdChar == "" {
					cmdLong = true
					cmdChar += string(b)
					continue
				} else {
					e = true
					break
				}
			}

			if b == '=' {
				if cmdChar != "" {
					cmdAssign = true
					continue
				} else {
					e = true
					break
				}
			}

			if cmdLong {
				if checkValidCmdChar(b) {
					cmdChar += string(b)
					continue
				} else {
					e = true
					break
				}
			}

			if cmdChar == "" || cmdChar == "&" {
				if b == '&' && cmdChar == "" && cmdBuf.Len() > 0 {
					cmdChar += string(b)
					continue
				}
				if checkValidCmdChar(b) {
					cmdChar += string(b)
					if cmdChar == "d" || cmdChar == "D" {
						cmdLong = true
						cmdAssign = true
					}
				} else {
					e = true
					break
				}
			} else {
				if checkValidNumChar(b) {
					cmdNum += string(b)
				} else {
					cmdBuf.UnreadByte()
					break
				}
			}
		}
		if !e {
			cmdRet = m.processCommand(strings.ToUpper(cmdChar), cmdNum, cmdAssign, cmdQuery, cmdAssignVal)
			if cmdRet == RetCodeError {
				break
			}
		}
		if cmdLong {
			break // long commands don't support chaining
		}
	}

	if e {
		cmdRet = RetCodeError
	}
	return cmdRet
}

func (m *Modem) ProcessAtCommand(cmd string) CmdReturn {
	m.checkLock()
	return m.processAtCommand(cmd)
}

func (m *Modem) ProcessAtCommandSync(cmd string) CmdReturn {
	m.Lock()
	defer m.Unlock()
	return m.processAtCommand(cmd)
}

func (m *Modem) ttyReadTask() {
	aFlag := false
	atFlag := false
	buffer := *bytes.NewBuffer(nil)
	byteBuff := make([]byte, 1)
	lastCmd := ""
	m.Lock()
	for {
		if m.ctx.Err() != nil {
			break
		}
		m.Unlock()
		n, err := m.tty.Read(byteBuff)
		m.Lock()
		if err != nil || n == 0 {
			break
		}

		if m.status() == StatusConnected { // online mode pass-through
			if m.conn != nil {
				m.conn.Write(byteBuff)
			}
			continue
		}

		if m.status() == StatusDialing {
			m.setStatus(StatusIdle)
			continue
		}

		if !atFlag {
			if m.echo {
				m.tty.Write(byteBuff)
			}
			if bytes.ToUpper(byteBuff)[0] == 'A' {
				aFlag = true
				continue
			}
			if aFlag && byteBuff[0] == '/' {
				aFlag = false
				m.ttyWriteStr("\r")
				r := m.processAtCommand(lastCmd)
				m.printRetCode(r)
				continue
			}
			if aFlag && bytes.ToUpper(byteBuff)[0] == 'T' {
				atFlag = true
				aFlag = false
				continue
			}
			aFlag = false
		} else {
			if byteBuff[0] == 0x7f {
				if buffer.Len() > 0 {
					buffer.Truncate(buffer.Len() - 1)
					m.ttyWriteStr("\x1b[D \x1b[D")
				}
				continue
			}
			if byteBuff[0] == '\r' {
				atFlag = false
				lastCmd = buffer.String()
				m.ttyWriteStr("\r")
				r := m.processAtCommand(lastCmd)
				m.printRetCode(r)
				buffer.Reset()
				continue
			}
			if buffer.Len() < 100 && strconv.IsPrint(rune(byteBuff[0])) {
				buffer.Write(byteBuff)
				if m.echo {
					m.tty.Write(byteBuff)
				}
			}
		}
	}
	m.Unlock()
}

func NewModem(ctx context.Context, config *ModemConfig) (*Modem, error) {
	if config == nil {
		return nil, ErrConfigRequired
	}

	if config.TTY == nil {
		return nil, ErrConfigRequired
	}

	modemContext, modemCancel := context.WithCancel(ctx)
	m := &Modem{
		ctx:          modemContext,
		cancel:       modemCancel,
		st:           StatusIdle,
		outgoingCall: config.OutgoingCall,
		commandHook:  config.CommandHook,
		tty:          config.TTY,
		connectStr:   config.ConnectStr,
		echo:         true,
		sregs:        make(map[byte]byte),
	}

	m.stCtx, m.stCtxCancel = context.WithCancel(ctx)

	if m.connectStr == "" {
		m.connectStr = "CONNECT"
	}

	go m.ttyReadTask()
	return m, nil
}
