package phobos

// client.go - connection state machine, modelled on client/cl_main.c.
//
//  1. OOB  "getchallenge"                      -> "challenge <n>"
//  2. OOB  "connect 34 <qport> <n> "<userinfo>"" -> "client_connect"
//  3. netchan up; reliable stringcmd "new"
//  4. server: svc_serverdata + stufftext "cmd configstrings <spawncount> 0"
//  5. we forward "cmd ..." lines back as stringcmds (configstrings, baselines)
//  6. server: stufftext "precache <spawncount>"  -> we send "begin <spawncount>"
//  7. server starts sending svc_frame: we are in the game and may "say".

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type connState int

const (
	stateChallenging connState = iota
	stateConnecting
	stateConnected // netchan established, loading the level
	stateActive    // received a frame after "begin": in game
)

func (s connState) String() string {
	return [...]string{"challenging", "connecting", "connected", "active"}[s]
}

type Config struct {
	Server   string
	Name     string
	Skin     string
	Password string // server "password" cvar, if any
	Verbose  bool
	// Spectator, if set, is sent as the "spectator" userinfo key: "1", or the
	// server's spectator_password if it has one.
	Spectator string
	// SendMoves makes the client send clc_move input like a real player,
	// with the buttons set by SetButtons. Off by default: a chat client or
	// spectator has no need to press anything.
	SendMoves bool
	// Chase (spectators only) attaches Phobos's camera to a player, so it
	// receives the events around them (muzzle flashes are only sent to
	// clients that can see the shooter). ChaseCycle > 0 rotates to the next
	// player at that interval.
	Chase      bool
	ChaseCycle time.Duration
	// Controller (implies Chase) is the name of the player Phobos should
	// follow whenever they are in the game. ControllerCheck is how often to
	// verify that (and log the status); 0 disables the periodic check.
	Controller      string
	ControllerCheck time.Duration
	// AuthSequence is the pattern of in-game actions (built with ParseSequence)
	// the controller must perform to authenticate. When it matches, the client
	// writes AuthFile. Both only apply when Controller is set.
	AuthSequence []Action
	AuthFile     string
	// MessageFile is where a decoded command-channel message is appended when
	// the authenticated controller ends a transmission (a post-auth jump).
	MessageFile string
	// CmdMethod selects how future command bits are encoded (1=morse,
	// 2=base4). Parsed and reported, but the command channel is not built yet;
	// authentication uses AuthSequence directly.
	CmdMethod int
	Timeout   time.Duration
}

type Client struct {
	cfg Config

	// player input, only sent when cfg.SendMoves is set
	buttons  atomic.Uint32
	move     atomic.Uint32 // forward<<16 | side, as int16 pairs
	cmds     [3]usercmd
	lastMove time.Time

	// chase cam state (cfg.Chase)
	chaseSlot   int // client slot being watched, -1 = none
	jumpPending atomic.Bool
	lastJump    time.Time
	lastSwitch  time.Time

	// controller lock-on (cfg.Controller)
	acquiring    bool
	acquireTries int
	lastStep     time.Time
	lastCheck    time.Time

	// controller authentication (cfg.AuthSequence)
	auth     authMatcher
	jumps    jumpDetector
	authSlot int  // slot the jump detector is currently tracking
	authed   bool // the controller has completed the auth sequence
	morse    *morseDecoder
	out      io.Writer
	conn     *net.UDPConn
	addr     *net.UDPAddr

	qport     uint16
	challenge int
	state     connState
	nc        *netchan

	lastResend time.Time
	tries      int

	levelName    string
	gamedir      string
	playerNum    int
	configs      map[int]string
	outbox       []string // stringcmds waiting for stateActive
	activeCh     chan struct{}
	activeOnce   sync.Once
	beginSent    bool
	disconnected bool
	quitErr      error
}

func NewClient(cfg Config, out io.Writer) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Controller != "" {
		cfg.Chase = true
	}
	if cfg.Chase {
		cfg.SendMoves = true // chase targets are picked with the jump key
		if cfg.Spectator == "" {
			cfg.Spectator = "1" // only spectators have a chase cam
		}
	}
	c := &Client{
		cfg:       cfg,
		out:       out,
		qport:     uint16(rand.Intn(65536)),
		configs:   map[int]string{},
		chaseSlot: -1,
		authSlot:  -1,
		auth:      authMatcher{seq: cfg.AuthSequence, window: 20 * time.Second},
		activeCh:  make(chan struct{}),
	}
	if cfg.CmdMethod == 1 && cfg.Controller != "" {
		c.morse = &morseDecoder{out: out, verbose: cfg.Verbose}
		c.morse.onMessage = c.logMessage
	}
	return c
}

func (c *Client) logf(format string, a ...any) {
	fmt.Fprintf(c.out, format+"\n", a...)
}

func (c *Client) debugf(format string, a ...any) {
	if c.cfg.Verbose {
		fmt.Fprintf(c.out, "[debug] "+format+"\n", a...)
	}
}

// userinfo builds the \key\value string sent in "connect".
func (c *Client) userinfo() string {
	clean := func(s string) string {
		return strings.NewReplacer(`\`, "", `"`, "", ";", "").Replace(s)
	}
	info := fmt.Sprintf(`\name\%s\skin\%s\rate\25000\msg\0\hand\2\fov\90`,
		clean(c.cfg.Name), clean(c.cfg.Skin))
	if c.cfg.Password != "" {
		info += `\password\` + clean(c.cfg.Password)
	}
	if c.cfg.Spectator != "" {
		info += `\spectator\` + clean(c.cfg.Spectator)
	}
	return info
}

// Active is closed once we are in the game.
func (c *Client) Active() <-chan struct{} { return c.activeCh }

// Say queues a chat message ("say" is handled by the game DLL: Cmd_Say_f).
func (c *Client) Say(text string) { c.Command("say " + text) }

// Command queues an arbitrary console command for the server, like typing
// it into the Q2 console (e.g. "say_team hi", "players", "score").
func (c *Client) Command(cmd string) {
	c.outbox = append(c.outbox, cmd)
}

func (c *Client) stringCmd(s string) {
	var w msgWriter
	w.Byte(clcStringCmd)
	w.String(s)
	if err := c.nc.QueueReliable(w.buf); err != nil {
		c.logf("! %v (dropped %q)", err, s)
	}
	c.debugf("-> stringcmd %q", strings.TrimRight(s, "\n"))
}

// ---------------------------------------------------------------- run loop

type packet struct {
	data []byte
	err  error
}

// Run connects and pumps the network until ctx-like 'stop' is closed or the
// server drops us. 'input' delivers lines from the user (may be nil).
func (c *Client) Run(stop <-chan struct{}, input <-chan string) error {
	var err error
	c.addr, err = net.ResolveUDPAddr("udp4", withDefaultPort(c.cfg.Server))
	if err != nil {
		return err
	}
	c.conn, err = net.DialUDP("udp4", nil, c.addr)
	if err != nil {
		return err
	}
	defer c.conn.Close()
	c.logf("* connecting to %s as %q (qport %d)", c.addr, c.cfg.Name, c.qport)

	pkts := make(chan packet, 64)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := c.conn.Read(buf)
			if err != nil {
				pkts <- packet{err: err}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			pkts <- packet{data: append([]byte(nil), buf[:n]...)}
		}
	}()

	c.checkResend(true)
	// A chat/spectator client only needs to nudge the channel at ~10 Hz, but
	// a client that sends player input (SendMoves) should send about as often
	// as a real client: at 10 Hz some server frames get no fresh usercmd and
	// the player stalls for a frame, which breaks up continuous movement (and
	// the Morse channel reads off that movement). ~25 Hz keeps every server
	// frame fed.
	moveInterval := 100 * time.Millisecond
	tickRate := 50 * time.Millisecond
	if c.cfg.SendMoves {
		moveInterval = 40 * time.Millisecond
		tickRate = 20 * time.Millisecond
	}
	tick := time.NewTicker(tickRate)
	defer tick.Stop()

	for {
		select {
		case <-stop:
			c.disconnect()
			return nil

		case line, ok := <-input:
			if !ok {
				input = nil
				continue
			}
			c.handleInput(line)

		case p := <-pkts:
			if p.err != nil {
				// On Windows/Linux an ICMP port-unreachable surfaces here
				// as "connection refused": the server isn't listening.
				c.debugf("read error: %v", p.err)
				continue
			}
			c.handlePacket(p.data)

		case <-tick.C:
		}

		if c.disconnected {
			return c.quitErr
		}

		if c.state < stateConnected {
			if err := c.checkResend(false); err != nil {
				return err
			}
			continue
		}

		if time.Since(c.nc.lastReceived) > c.cfg.Timeout {
			return fmt.Errorf("server timed out")
		}

		if c.state == stateActive {
			for _, cmd := range c.outbox {
				c.stringCmd(cmd)
			}
			c.outbox = c.outbox[:0]
		}

		// Send at ~10 Hz, or immediately if there is new reliable data.
		c.updateChase()
		if c.morse != nil {
			c.morse.tick(time.Now())
		}
		if time.Since(c.nc.lastSent) >= moveInterval || c.nc.needReliable() {
			c.nc.Transmit(c.unreliable())
		}
	}
}

func withDefaultPort(s string) string {
	if _, _, err := net.SplitHostPort(s); err != nil {
		return net.JoinHostPort(s, "27910")
	}
	return s
}

// checkResend mirrors CL_CheckForResend: retry the handshake every 3s.
func (c *Client) checkResend(force bool) error {
	if !force && time.Since(c.lastResend) < 3*time.Second {
		return nil
	}
	if c.tries >= 10 {
		return fmt.Errorf("no response from %s after %d attempts", c.addr, c.tries)
	}
	c.tries++
	c.lastResend = time.Now()
	switch c.state {
	case stateChallenging:
		c.debugf("-> getchallenge (try %d)", c.tries)
		return sendOOB(c.conn, "getchallenge\n")
	case stateConnecting:
		msg := fmt.Sprintf("connect %d %d %d \"%s\"\n",
			protocolVersion, c.qport, c.challenge, c.userinfo())
		c.debugf("-> %s", strings.TrimSpace(msg))
		return sendOOB(c.conn, msg)
	}
	return nil
}

func (c *Client) handleInput(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	switch {
	case line == "/quit" || line == "/exit":
		c.disconnect()
	case line == "/who":
		c.printPlayers()
	case strings.HasPrefix(line, "/"):
		c.Command(line[1:])
	default:
		c.Say(line)
	}
	if c.state != stateActive && !c.disconnected {
		c.logf("* (queued until we are in the game)")
	}
}

func (c *Client) disconnect() {
	if c.nc != nil && !c.disconnected {
		// CL_Disconnect sends this unreliably three times
		var w msgWriter
		w.Byte(clcStringCmd)
		w.String("disconnect")
		for i := 0; i < 3; i++ {
			c.nc.Transmit(w.buf)
		}
		c.logf("* disconnected")
	}
	c.disconnected = true
}

// ---------------------------------------------------------------- packets

func (c *Client) handlePacket(data []byte) {
	if len(data) >= 4 && data[0] == 0xff && data[1] == 0xff && data[2] == 0xff && data[3] == 0xff {
		c.handleOOB(string(data[4:]))
		return
	}
	if c.nc == nil {
		return
	}
	payload, ok := c.nc.Process(data)
	if !ok {
		return
	}
	if err := parseServerMessage(newReader(payload), c); err != nil {
		// The reliable part always comes first in a packet, so whatever we
		// care about (prints, stufftext) has already been handled.
		c.debugf("parse: %v", err)
	}
}

// handleOOB mirrors CL_ConnectionlessPacket.
func (c *Client) handleOOB(s string) {
	cmd, rest, _ := strings.Cut(s, "\n")
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return
	}
	c.debugf("<- oob %q", cmd)

	switch fields[0] {
	case "challenge":
		if c.state != stateChallenging || len(fields) < 2 {
			return
		}
		c.challenge, _ = strconv.Atoi(fields[1])
		c.state = stateConnecting
		c.tries = 0
		c.checkResend(true)

	case "client_connect":
		if c.state != stateConnecting {
			return
		}
		c.nc = newNetchan(c.conn, c.qport)
		c.state = stateConnected
		c.logf("* connected, loading level...")
		c.stringCmd("new")
		c.nc.Transmit(nil)

	case "print":
		// e.g. "Server is full.", "Bad challenge.", "Server is version 3.19"
		msg := strings.TrimSpace(q2Text(rest))
		c.logf("[server] %s", msg)
		if c.state < stateConnected {
			c.quitErr = fmt.Errorf("connection refused: %s", msg)
			c.disconnected = true
		}

	case "echo":
		if len(fields) > 1 {
			sendOOB(c.conn, fields[1])
		}
	}
}

// ---------------------------------------------------------------- serverHandler

func (c *Client) onPrint(level int, text string) {
	if c.cfg.Controller != "" && level == printHigh {
		// p_client.c ClientBeginDeathmatch: "%s entered the game\n"
		if name, ok := strings.CutSuffix(q2Text(text), " entered the game\n"); ok &&
			strings.EqualFold(name, c.cfg.Controller) && c.state == stateActive {
			defer func() {
				c.logf("* controller %s joined - locking on", c.cfg.Controller)
				c.startAcquire()
			}()
		}
	}
	c.debugf("<- print level=%d raw=%q", level, text)
	text = strings.TrimRight(q2Text(text), "\n")
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if level == printChat {
			c.logf("%s %s", time.Now().Format("15:04:05"), line)
		} else {
			c.logf("[server] %s", line)
		}
	}
}

func (c *Client) onLayout(text string) {
	// the TAB scoreboard (and other HUD layouts) as a layout program
	c.debugf("<- layout %q", text)
}

// onMuzzleFlash reports rocket launcher shots. Player entities are numbered
// 1..maxclients, one per client slot, so entity-1 indexes CS_PLAYERSKINS.
func (c *Client) onMuzzleFlash(ent, weapon int) {
	if weapon&^mzSilenced != mzRocket || ent < 1 || ent > maxClients {
		return
	}
	slot := ent - 1
	name := c.playerName(slot)
	if name == "" {
		name = fmt.Sprintf("client %d", slot)
	}
	silenced := ""
	if weapon&mzSilenced != 0 {
		silenced = " (silenced)"
	}
	c.logf("%s ROCKET fired by %s%s", time.Now().Format("15:04:05"), name, silenced)
	if slot == c.controllerSlot() {
		c.feedAuth(ActRocket)
	}
}

func (c *Client) onEntity(number int, x, y, z float64, hasX, hasY, hasZ bool) {
	slot := c.controllerSlot()
	ent := slot + 1

	if c.authArmed() && hasZ {
		if slot != c.authSlot {
			c.authSlot = slot
			c.jumps.reset()
		}
		if slot >= 0 && number == ent {
			if c.jumps.feed(z, time.Now()) {
				c.logf("%s %s jumped", time.Now().Format("15:04:05"), c.playerName(slot))
				c.debugf("controller jump detected (z=%.0f)", z)
				if c.authed {
					// After authentication a jump is a morse signal:
					// letter break, word break, or end of message,
					// depending on what's pending.
					if c.morse != nil {
						c.morse.onControllerJump(time.Now())
					}
				} else {
					c.feedAuth(ActJump)
				}
			}
		}
	}

	if c.morse != nil && slot >= 0 && number == ent {
		c.morse.feedXY(x, y, hasX, hasY, time.Now())
	}
}

// authArmed reports whether controller authentication is configured.
func (c *Client) authArmed() bool {
	return c.cfg.Controller != "" && len(c.cfg.AuthSequence) > 0
}

// feedAuth advances the authentication matcher with one controller action.
// Once authenticated it stops feeding: further rockets or jumps belong to the
// command channel (a post-auth jump ends a message), not to re-authentication.
func (c *Client) feedAuth(a Action) {
	if !c.authArmed() || c.authed {
		return
	}
	c.debugf("auth action from controller: %s", a)
	if c.auth.feed(a, time.Now()) {
		c.onAuthSuccess()
	}
}

// logMessage appends a decoded command-channel message to MessageFile. It is
// the morse decoder's onMessage hook, fired when the controller ends a
// transmission with a jump.
func (c *Client) logMessage(now time.Time, msg string) {
	if c.cfg.MessageFile == "" {
		return
	}
	line := fmt.Sprintf("%s %s\n", now.Format("2006-01-02 15:04:05"), msg)
	f, err := os.OpenFile(c.cfg.MessageFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		c.logf("! could not write message log %s: %v", c.cfg.MessageFile, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		c.logf("! could not write message log %s: %v", c.cfg.MessageFile, err)
		return
	}
	c.logf("* message logged to %s: %q", c.cfg.MessageFile, msg)
}

func (c *Client) onAuthSuccess() {
	c.authed = true
	// Start the command channel from a clean slate so the first transmitted
	// message carries no movement decoded during the auth sequence.
	if c.morse != nil {
		c.morse.reset()
	}
	c.logf("* CONTROLLER AUTHENTICATED: %s completed [%s]",
		c.cfg.Controller, SequenceString(c.cfg.AuthSequence))
	if c.cfg.AuthFile == "" {
		return
	}
	const msg = "SUCCESFULL AUTH!! WOO HOO!!\n"
	if err := os.WriteFile(c.cfg.AuthFile, []byte(msg), 0o600); err != nil {
		c.logf("! could not write auth file %s: %v", c.cfg.AuthFile, err)
		return
	}
	c.logf("* wrote %s", c.cfg.AuthFile)
}

// playerName returns the name from a client slot's "name\skin" configstring.
func (c *Client) playerName(slot int) string {
	name, _, _ := strings.Cut(c.configs[csPlayerSkins+slot], `\`)
	return q2Text(name)
}

func (c *Client) onCenterPrint(text string) {
	// g_chase.c answers every chase attempt with this while we are alone;
	// the "not watching anyone" line already says so
	if c.cfg.Chase && strings.TrimSpace(text) == "No other players to chase." {
		c.debugf("<- centerprint %q", text)
		return
	}
	if t := strings.TrimSpace(q2Text(text)); t != "" {
		c.logf("[center] %s", strings.ReplaceAll(t, "\n", " / "))
	}
}

func (c *Client) onServerData(proto, count int32, gamedir string, pnum int, level string) {
	if proto != protocolVersion {
		c.quitErr = fmt.Errorf("server returned protocol %d, not %d", proto, protocolVersion)
		c.disconnected = true
		return
	}
	c.gamedir, c.playerNum, c.levelName = gamedir, pnum, level
	if gamedir == "" {
		gamedir = "baseq2"
	}
	c.logf("* level: %q  game: %s  you are client %d", level, gamedir, pnum)
}

func (c *Client) onConfigString(idx int, val string) {
	if idx < 0 || idx >= maxConfigStrings {
		return
	}
	c.configs[idx] = val
	if idx == csMapChecksum {
		c.debugf("map checksum %s", val)
	}
}

func (c *Client) onFrame(int32) {
	if c.state == stateConnected && c.beginSent {
		c.state = stateActive
		c.logf("* in game on map %s - type to chat, /cmd for commands, /who, /quit",
			c.mapName())
		c.lastCheck = time.Time{} // check for the controller straight away
		c.activeOnce.Do(func() { close(c.activeCh) })
	}
}

func (c *Client) onDisconnect() {
	c.logf("* server disconnected us")
	c.disconnected = true
	c.quitErr = fmt.Errorf("disconnected by server")
}

func (c *Client) onReconnect() {
	// svc_reconnect: the server restarted; redo the whole handshake
	c.logf("* server is restarting, reconnecting...")
	c.resetForReconnect()
	c.nc = nil
	c.state = stateChallenging
	c.tries = 0
	c.lastResend = time.Time{}
}

func (c *Client) resetForReconnect() {
	c.beginSent = false
	if c.state == stateActive {
		c.state = stateConnected
	}
	if c.morse != nil {
		c.morse.reset()
	}
}

// onStuffText handles text the server pushes into our console buffer.
// A real client would execute it; we only honour the handful of commands
// that drive the connection forward.
func (c *Client) onStuffText(text string) {
	for _, cmd := range splitCommands(text) {
		args := strings.Fields(cmd)
		if len(args) == 0 {
			continue
		}
		c.debugf("<- stufftext %q", cmd)
		switch args[0] {
		case "cmd":
			// Cmd_ForwardToServer: send everything after "cmd "
			c.stringCmd(strings.TrimSpace(strings.TrimPrefix(cmd, "cmd")))
		case "precache":
			// CL_Precache_f -> (no downloads needed) -> "begin <spawncount>"
			if len(args) >= 2 {
				c.stringCmd("begin " + args[1] + "\n")
				c.beginSent = true
			}
		case "changing":
			c.logf("* server is changing map...")
			c.resetForReconnect()
		case "reconnect":
			// CL_Reconnect_f while connected: just ask for the new level
			c.resetForReconnect()
			c.state = stateConnected
			c.stringCmd("new")
		case "disconnect":
			c.onDisconnect()
		}
	}
}

// splitCommands splits a console buffer on newlines and on semicolons that
// are not inside quotes (like Cbuf_Execute).
func splitCommands(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == '\n' || (r == ';' && !quoted):
			out = append(out, cur.String())
			cur.Reset()
			quoted = false
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ---------------------------------------------------------------- helpers

func (c *Client) mapName() string {
	m := c.configs[csModels+1] // "maps/q2dm1.bsp"
	m = strings.TrimSuffix(strings.TrimPrefix(m, "maps/"), ".bsp")
	if m == "" {
		return c.levelName
	}
	return m
}

func (c *Client) printPlayers() {
	n := 0
	for i := 0; i < maxClients; i++ {
		v, ok := c.configs[csPlayerSkins+i]
		if !ok || v == "" {
			continue
		}
		name, _, _ := strings.Cut(v, `\`)
		me := ""
		if i == c.playerNum {
			me = "  (you)"
		}
		c.logf("  %2d  %s%s", i, q2Text(name), me)
		n++
	}
	c.logf("* %d player(s)", n)
}

// SetButtons sets the buttons held down (buttonAttack, ...); it takes effect
// when cfg.SendMoves is enabled. Safe to call from any goroutine.
func (c *Client) SetButtons(b byte) { c.buttons.Store(uint32(b)) }

// Jump queues one jump (upmove) on the next clc_move; needs cfg.SendMoves.
// Used by tests to make a bot jump.
func (c *Client) Jump() { c.jumpPending.Store(true) }

// SetMove sets the walk velocity sent on each clc_move: forward is +ahead
// /-back, side is +right/-left, in Quake 2 speed units (~400 is a full run).
// Takes effect when cfg.SendMoves is enabled. Used to drive a controller
// bot so it physically walks - the Morse channel reads its XY displacement.
// Safe to call from any goroutine.
func (c *Client) SetMove(forward, side int16) {
	c.move.Store(uint32(uint16(forward))<<16 | uint32(uint16(side)))
}

// unreliable returns the unreliable part of the next packet: a clc_move
// when we are sending input, else a clc_nop to keep the channel alive.
func (c *Client) unreliable() []byte {
	if !c.cfg.SendMoves || c.state != stateActive {
		return []byte{clcNop}
	}
	msec := time.Since(c.lastMove).Milliseconds()
	if c.lastMove.IsZero() || msec > 250 {
		msec = 100 // CL_CreateCmd clamps to 250
	}
	c.lastMove = time.Now()
	cmd := usercmd{msec: byte(msec), buttons: byte(c.buttons.Load())}
	mv := c.move.Load()
	cmd.forward = int16(uint16(mv >> 16))
	cmd.side = int16(uint16(mv))
	if c.jumpPending.Load() {
		cmd.up = 200 // one frame of jump; the game acts on the press edge
		c.jumpPending.Store(false)
	}
	c.cmds = [3]usercmd{c.cmds[1], c.cmds[2], cmd}
	// the checksum is keyed to the sequence this packet is about to use
	return buildMove(c.nc.outgoingSequence, -1, c.cmds)
}

// updateChase presses jump when we need a chase target: spectators use jump
// for GetChaseTarget / ChaseNext (p_client.c ClientThink).
func (c *Client) updateChase() {
	if !c.cfg.Chase || c.state != stateActive {
		return
	}
	if c.cfg.Controller != "" && c.updateController() {
		return
	}
	if time.Since(c.lastJump) < time.Second {
		return
	}
	cycle := c.cfg.ChaseCycle > 0 && time.Since(c.lastSwitch) >= c.cfg.ChaseCycle
	if c.chaseSlot < 0 || cycle {
		c.jumpPending.Store(true)
		c.lastJump = time.Now()
		if cycle {
			c.lastSwitch = time.Now()
		}
	}
}

// onPlayerStats tracks STAT_CHASE. We never ask for delta-compressed frames
// (lastframe -1), so a stat that isn't sent is zero.
func (c *Client) onPlayerStats(bits uint32, stats [32]int16) {
	slot := -1
	if bits&(1<<statChase) != 0 && int(stats[statChase]) >= csPlayerSkins {
		slot = int(stats[statChase]) - csPlayerSkins
	}
	if slot == c.chaseSlot {
		return
	}
	c.chaseSlot = slot
	c.lastSwitch = time.Now()
	if !c.cfg.Chase {
		return
	}
	if slot < 0 {
		c.logf("* not watching anyone (waiting for a player to chase)")
	} else {
		c.logf("* now watching %s", c.playerName(slot))
	}
}

// controllerSlot finds the controller's client slot by name, or -1.
func (c *Client) controllerSlot() int {
	for slot := 0; slot < maxClients; slot++ {
		if slot != c.playerNum && strings.EqualFold(c.playerName(slot), c.cfg.Controller) {
			return slot
		}
	}
	return -1
}

func (c *Client) startAcquire() {
	n, err := strconv.Atoi(c.configs[csMaxClients])
	if err != nil || n <= 0 {
		n = maxClients
	}
	c.acquiring = true
	c.acquireTries = n + 1 // one full lap of ChaseNext, plus the first lock
	c.lastStep = time.Time{}
}

// controllerCheck logs whether the controller is here and being followed,
// and starts a lock-on if they are here but not followed.
func (c *Client) controllerCheck() {
	slot := c.controllerSlot()
	switch {
	case slot < 0:
		c.logf("* controller check: %s is not on the server", c.cfg.Controller)
	case slot == c.chaseSlot:
		c.logf("* controller check: following %s", c.playerName(slot))
	default:
		c.logf("* controller check: %s is here but not followed - locking on", c.playerName(slot))
		c.startAcquire()
	}
}

// updateController drives the lock-on. It returns true while the controller
// owns the chase cam, so the generic chase logic (follow anyone, cycle)
// stays out of the way.
func (c *Client) updateController() bool {
	now := time.Now()
	if c.cfg.ControllerCheck > 0 && now.Sub(c.lastCheck) >= c.cfg.ControllerCheck ||
		c.lastCheck.IsZero() {
		c.lastCheck = now
		c.controllerCheck()
	}
	slot := c.controllerSlot()
	if !c.acquiring {
		return slot >= 0 && slot == c.chaseSlot
	}
	switch {
	case slot < 0:
		c.acquiring = false
		return false
	case slot == c.chaseSlot:
		c.acquiring = false
		c.logf("* following controller %s", c.playerName(slot))
		return true
	case now.Sub(c.lastStep) < 300*time.Millisecond:
		return true // give the server a frame or two to update STAT_CHASE
	case c.acquireTries == 0:
		c.acquiring = false
		c.logf("* couldn't lock on to %s (spectating?) - will retry at the next check", c.playerName(slot))
		return false
	}
	c.acquireTries--
	c.lastStep = now
	if c.chaseSlot < 0 {
		c.jumpPending.Store(true) // GetChaseTarget: first player in slot order
		c.lastJump = now
	} else {
		// SelectNextItem -> ChaseNext for a chasing spectator (g_cmds.c)
		c.stringCmd("invnext")
	}
	return true
}
