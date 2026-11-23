package phobos

// morse.go - decode player movement into Morse code text. When CmdMethod is 1
// and a controller is set, the decoder tracks the controller entity's XY
// position. A short burst of movement is a dot, a longer one is a dash.
// Decoded characters accumulate into words, printed when the player pauses.

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

var morseToChar = map[string]byte{
	".-": 'a', "-...": 'b', "-.-.": 'c', "-..": 'd',
	".": 'e', "..-.": 'f', "--.": 'g', "....": 'h',
	"..": 'i', ".---": 'j', "-.-": 'k', ".-..": 'l',
	"--": 'm', "-.": 'n', "---": 'o', ".--.": 'p',
	"--.-": 'q', ".-.": 'r', "...": 's', "-": 't',
	"..-": 'u', "...-": 'v', ".--": 'w', "-..-": 'x',
	"-.--": 'y', "--..": 'z',
	"-----": '0', ".----": '1', "..---": '2', "...--": '3',
	"....-": '4', ".....": '5', "-....": '6', "--...": '7',
	"---..": '8', "----.": '9',
	".-.-.-": '.', "--..--": ',', "..--..": '?', "-..-.": '/',
}

// Morse timing.
// Maybe set dynamically depending on latency conditions, especially if onions get involved later..
// Measured burst and pause lengths. Wide margins between a dot and a dash, and
// between the element/letter/word gaps, keep that jitter from tipping a symbol
// into the wrong bucket. The controller moves at a matching, unhurried cadence
// (see the Morse sender): a quick tap for a dot, a long hold for a dash, with
// clear pauses between letters and words.
const (
	morseMoveThreshold = 10.0                    // XY displacement per update to count as moving
	morseDashMin       = 700 * time.Millisecond  // burst >= this is a dash; shorter is a dot
	morseLetterGap     = 1200 * time.Millisecond // pause this long ends a letter
	morseWordGap       = 2500 * time.Millisecond // pause this long ends a word
	morseFlushGap      = 8 * time.Second         // flush whatever is pending after silence

	// A burst only ends once movement has stayed below the threshold for this
	// long. Entity positions arrive at the server's ~10 Hz, and under latency
	// several samples in a row can read as stopped even mid-stride (repeated
	// positions from delayed input, a lag spike, or a wall graze). Bridging
	// gaps shorter than this keeps one burst from fragmenting into several,
	// while real element gaps (half a second or more) are untouched. Must stay
	// below the shortest real gap between two elements of a letter.
	morseStopDebounce = 250 * time.Millisecond
)

type morseDecoder struct {
	lastX, lastY float64
	hasLast      bool

	moving    bool
	moveStart time.Time
	moveStop  time.Time
	lowSince  time.Time // first sub-threshold sample of a pending stop (zero = none)

	symbol  strings.Builder
	word    strings.Builder
	message strings.Builder // decoded words so far, awaiting end-of-message

	letterDone bool
	wordDone   bool

	out     io.Writer
	verbose bool

	// onMessage, if set, is called with the full decoded message when the
	// controller ends a transmission (a post-auth jump). It is how the
	// completed message is persisted (e.g. appended to phobos.txt).
	onMessage func(now time.Time, msg string)
}

func (m *morseDecoder) reset() {
	out, v, cb := m.out, m.verbose, m.onMessage
	*m = morseDecoder{out: out, verbose: v, onMessage: cb}
}

func (m *morseDecoder) feedXY(x, y float64, hasX, hasY bool, now time.Time) {
	if !hasX && !hasY {
		return
	}
	if !m.hasLast {
		if hasX {
			m.lastX = x
		}
		if hasY {
			m.lastY = y
		}
		m.hasLast = true
		return
	}

	cx, cy := m.lastX, m.lastY
	if hasX {
		cx = x
	}
	if hasY {
		cy = y
	}
	dx := cx - m.lastX
	dy := cy - m.lastY
	dist := math.Sqrt(dx*dx + dy*dy)
	m.lastX, m.lastY = cx, cy

	if dist > morseMoveThreshold {
		if m.moving && !m.lowSince.IsZero() {
			// movement resumed after a brief dip
			if now.Sub(m.lowSince) >= morseStopDebounce {
				m.commitBurst() // the dip was a real stop; close the burst
			} else {
				m.lowSince = time.Time{} // just a glitch; keep the burst going
			}
		}
		if !m.moving {
			m.moving = true
			m.moveStart = now
			m.flushOnGap(now)
		}
	} else if m.moving {
		if m.lowSince.IsZero() {
			m.lowSince = now
		}
		if now.Sub(m.lowSince) >= morseStopDebounce {
			m.commitBurst()
		}
	}
	if !m.moving {
		m.flushOnGap(now)
	}
}

// commitBurst closes the current movement burst at the moment it first dropped
// below the threshold (m.lowSince), recording a dot or dash from its length.
func (m *morseDecoder) commitBurst() {
	m.moving = false
	m.moveStop = m.lowSince
	dur := m.moveStop.Sub(m.moveStart)
	if dur < morseDashMin {
		m.symbol.WriteByte('.')
	} else {
		m.symbol.WriteByte('-')
	}
	m.lowSince = time.Time{}
	m.letterDone = false
	m.wordDone = false
	if m.verbose {
		fmt.Fprintf(m.out, "[morse] element: %c (%.0fms)\n",
			m.symbol.String()[m.symbol.Len()-1], dur.Seconds()*1000)
	}
}

func (m *morseDecoder) tick(now time.Time) {
	if m.moving {
		if !m.lowSince.IsZero() && now.Sub(m.lowSince) >= morseStopDebounce {
			m.commitBurst()
		} else {
			return
		}
	}
	m.flushOnGap(now)
}

func (m *morseDecoder) flushOnGap(now time.Time) {
	if m.moveStop.IsZero() {
		return
	}
	gap := now.Sub(m.moveStop)
	if gap >= morseLetterGap && m.symbol.Len() > 0 && !m.letterDone {
		m.decodeLetter()
	}
	if gap >= morseWordGap && m.word.Len() > 0 && !m.wordDone {
		m.emitWord(now)
	}
	if gap >= morseFlushGap && m.word.Len() > 0 && !m.wordDone {
		m.emitWord(now)
	}
}

func (m *morseDecoder) decodeLetter() {
	code := m.symbol.String()
	m.symbol.Reset()
	m.letterDone = true
	ch, ok := morseToChar[code]
	if ok {
		m.word.WriteByte(ch)
		if m.verbose {
			fmt.Fprintf(m.out, "[morse] letter: %s -> %c\n", code, ch)
		}
	} else {
		m.word.WriteByte('?')
		fmt.Fprintf(m.out, "[morse] unknown symbol: %s\n", code)
	}
}

func (m *morseDecoder) emitWord(now time.Time) {
	if m.word.Len() == 0 {
		return
	}
	w := m.word.String()
	fmt.Fprintf(m.out, "%s [MORSE] %s\n", now.Format("15:04:05"), w)
	// accumulate into the current message (space-separated words)
	if m.message.Len() > 0 {
		m.message.WriteByte(' ')
	}
	m.message.WriteString(w)
	m.word.Reset()
	m.wordDone = true
}

// closeBurst closes any in-progress movement burst, recording a dot or dash.
func (m *morseDecoder) closeBurst(now time.Time) {
	if !m.moving {
		return
	}
	dur := now.Sub(m.moveStart)
	if dur < morseDashMin {
		m.symbol.WriteByte('.')
	} else {
		m.symbol.WriteByte('-')
	}
	m.moving = false
	m.moveStop = now
	m.lowSince = time.Time{}
	m.letterDone = false
	m.wordDone = false
	if m.verbose {
		fmt.Fprintf(m.out, "[morse] element: %c (%.0fms)\n",
			m.symbol.String()[m.symbol.Len()-1], dur.Seconds()*1000)
	}
}

// onControllerJump handles a post-auth jump with cascading priority:
//  1. Pending dots/dashes → decode letter (letter break)
//  2. Pending word letters → emit word (word break / space)
//  3. Accumulated message → end of message
func (m *morseDecoder) onControllerJump(now time.Time) {
	m.closeBurst(now)

	if m.symbol.Len() > 0 {
		m.decodeLetter()
		m.letterDone = true
		m.moveStop = time.Time{}
		return
	}

	if m.word.Len() > 0 {
		m.emitWord(now)
		m.wordDone = true
		m.moveStop = time.Time{}
		return
	}

	msg := m.message.String()
	fmt.Fprintf(m.out, "%s [MORSE] end of message: %q\n", now.Format("15:04:05"), msg)
	if msg != "" && m.onMessage != nil {
		m.onMessage(now, msg)
	}
	m.message.Reset()
	m.symbol.Reset()
	m.word.Reset()
	m.letterDone = true
	m.wordDone = true
	m.moveStop = time.Time{}
}

// endMessage finalises the current transmission by flushing everything at once.
// Used by the timing-based flush (morseFlushGap) and tests.
func (m *morseDecoder) endMessage(now time.Time) {
	m.closeBurst(now)
	if m.symbol.Len() > 0 {
		m.decodeLetter()
	}
	if m.word.Len() > 0 {
		m.emitWord(now)
	}
	msg := m.message.String()
	fmt.Fprintf(m.out, "%s [MORSE] end of message: %q\n", now.Format("15:04:05"), msg)
	if msg != "" && m.onMessage != nil {
		m.onMessage(now, msg)
	}
	m.message.Reset()
	m.symbol.Reset()
	m.word.Reset()
	m.letterDone = true
	m.wordDone = true
	m.moveStop = time.Time{}
}
