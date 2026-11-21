package phobos

// auth.go - controller authentication by a sequence of observable in-game
// actions. Rocket shots are reported explicitly by the engine (an
// svc_muzzleflash with MZ_ROCKET), so they are exact. A jump has no event
// in the protocol, so it is inferred from the controller entity's Z origin
// rising while Phobos is chasing them (see jumpDetector).

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Action is one unit of an authentication sequence.
type Action int

const (
	ActRocket Action = iota // one rocket launcher shot
	ActJump                 // one jump
)

func (a Action) String() string {
	switch a {
	case ActRocket:
		return "ROCKET"
	case ActJump:
		return "JUMP"
	}
	return "?"
}

// ParseSequence expands a directive like "ROCKET3,JUMP,ROCKET2" into the
// flat action list [ROCKET ROCKET ROCKET JUMP ROCKET ROCKET]. A bare token
// ("ROCKET", "JUMP") counts as one.
func ParseSequence(s string) ([]Action, error) {
	var out []Action
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		i := 0
		for i < len(tok) && (tok[i] < '0' || tok[i] > '9') {
			i++
		}
		word, numStr := strings.ToUpper(tok[:i]), tok[i:]
		var act Action
		switch word {
		case "ROCKET", "ROCKETS":
			act = ActRocket
		case "JUMP", "JUMPS":
			act = ActJump
		default:
			return nil, fmt.Errorf("unknown action %q (want ROCKET or JUMP)", tok)
		}
		n := 1
		if numStr != "" {
			var err error
			if n, err = strconv.Atoi(numStr); err != nil || n < 1 {
				return nil, fmt.Errorf("bad count in %q", tok)
			}
		}
		for j := 0; j < n; j++ {
			out = append(out, act)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty sequence")
	}
	return out, nil
}

func SequenceString(seq []Action) string {
	parts := make([]string, len(seq))
	for i, a := range seq {
		parts[i] = a.String()
	}
	return strings.Join(parts, " ")
}

// authMatcher matches a stream of actions against seq in order. A wrong
// action resets progress but is reconsidered as a possible fresh start, and
// too long a gap between actions resets it (so a half-finished attempt does
// not linger forever).
type authMatcher struct {
	seq    []Action
	window time.Duration
	i      int
	last   time.Time
}

// feed advances the matcher and returns true on the action that completes
// the whole sequence.
func (m *authMatcher) feed(a Action, now time.Time) bool {
	if len(m.seq) == 0 {
		return false
	}
	if m.i > 0 && m.window > 0 && now.Sub(m.last) > m.window {
		m.i = 0
	}
	m.last = now
	if a == m.seq[m.i] {
		m.i++
	} else {
		m.i = 0
		if a == m.seq[0] {
			m.i = 1
		}
	}
	if m.i == len(m.seq) {
		m.i = 0
		return true
	}
	return false
}

// jumpDetector infers jumps from a player entity's Z origin. A jump is an arc:
// Z rises at least jumpRise above the floor and then starts falling within
// jumpMaxRise, and the detector fires at that peak. Stairs, ramps, lifts and
// respawns also raise Z but never fall back that quickly, so they only move
// the floor. Every phase has a timeout, so the detector can't get stuck
// waiting for a landing that never comes.
type jumpDetector struct {
	started bool
	phase   jumpPhase
	floor   float64   // ground level the current rise is measured from
	peak    float64   // highest Z since leaving the floor
	entry   float64   // Z when it first cleared floor+jumpRise; a real arc keeps rising past it
	upAt    time.Time // when Z first cleared floor+jumpRise
	launch  float64   // Z of the latest sample that wasn't rising
	base    float64   // launch when the current rise cleared jumpRise
	last    float64   // previous sample
}

type jumpPhase int

const (
	jumpGround jumpPhase = iota
	jumpUp               // risen past jumpRise, waiting for the arc to turn over
	jumpDown             // jump reported, waiting to land
)

const (
	jumpRise     = 18.0 // a stair step is at most 18
	jumpMinPeak  = 24.0 // jump height depends on client fps: ~33u at 10, ~39u at 20, ~45u at 60+
	jumpFall     = 4.0  // drop below the peak that confirms the arc turned over
	jumpLand     = 8.0  // back within this of the floor = landed
	jumpTeleport = 64.0 // a bigger one-sample Z change is a respawn/teleport/long fall

	jumpMaxRise = 700 * time.Millisecond  // a jump turns over ~340ms after takeoff
	jumpMaxAir  = 1500 * time.Millisecond // landed somewhere other than the floor
)

func (d *jumpDetector) reset() { *d = jumpDetector{} }

// feed returns true once per jump, at the top of its arc.
func (d *jumpDetector) feed(z float64, now time.Time) bool {
	if !d.started {
		d.started, d.floor, d.last, d.launch = true, z, z, z
		return false
	}
	prev := d.last
	d.last = z

	if z-prev > jumpTeleport || prev-z > jumpTeleport {
		d.phase, d.floor, d.launch = jumpGround, z, z
		return false
	}
	if z <= prev {
		d.launch = z
	}

	switch d.phase {
	case jumpUp:
		if z > d.peak {
			d.peak = z
		}
		switch {
		case z <= d.peak-jumpFall && d.peak-d.base >= jumpMinPeak && d.peak > d.entry:
			d.phase = jumpDown
			return true
		case z <= d.peak-jumpFall:
			// Over a bump or a couple of stairs, or a respawn settling onto
			// the floor (one step up, then down): not an arc.
			d.phase, d.floor = jumpGround, z
		case now.Sub(d.upAt) <= jumpMaxRise:
			return false
		default:
			d.phase, d.floor = jumpGround, prev // rose and stayed up: stairs, ramp, lift
		}
	case jumpDown:
		switch {
		case z <= d.floor+jumpLand:
			d.phase, d.floor = jumpGround, z
			return false
		case z > prev || now.Sub(d.upAt) > jumpMaxAir:
			// Rising again means we landed and took off; otherwise we've
			// been up too long and landed off the old floor (a ledge).
			d.phase, d.floor = jumpGround, prev
		default:
			return false
		}
	}

	// jumpGround: drops are followed at once, small rises slowly, so a
	// jump's first samples stand well clear of the floor.
	switch {
	case z <= d.floor:
		d.floor = z
	case z >= d.floor+jumpRise:
		d.phase, d.peak, d.entry, d.upAt, d.base = jumpUp, z, z, now, d.launch
	default:
		d.floor += (z - d.floor) * 0.5
	}
	return false
}
