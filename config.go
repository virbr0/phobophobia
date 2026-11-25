package phobos

// config.go - phobos.cfg loader. The file is a list of directives, one per
// line, "Key: Value" or "Key = Value". Lines whose first non-space character
// is '#' are comments; blank lines are ignored. Keys are case-insensitive.
//
//	PhobosName:         Phobos
//	ControllerName:     Player
//	ControllerSequence: ROCKET3,JUMP,ROCKET2
//	CmdMethod:          1
//
// Anything not in the file keeps the default below.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultConfig returns the built-in Phobos configuration that phobos.cfg
// overrides. It is a stats-tracking spectator that locks onto a controller.
func DefaultConfig() Config {
	seq, _ := ParseSequence("ROCKET3,JUMP,ROCKET2")
	return Config{
		Server:          "127.0.0.1",
		Name:            "Phobos",
		Skin:            "male/grunt",
		Controller:      "Player",
		ControllerCheck: 5 * time.Minute,
		AuthSequence:    seq,
		MessageFile:     "phobos.txt",
		CmdMethod:       1,
	}
}

// LoadConfigFile reads phobos.cfg at path and returns DefaultConfig with the
// file's directives applied.
func LoadConfigFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return DefaultConfig(), err
	}
	defer f.Close()
	return loadConfig(path, f)
}

// LoadConfigString parses phobos.cfg content from a string and returns
// DefaultConfig with its directives applied. It is used by a built Phobos
// binary to read the config embedded into it at build time.
func LoadConfigString(data string) (Config, error) {
	return loadConfig("<embedded>", strings.NewReader(data))
}

// loadConfig applies the directives read from r on top of DefaultConfig. name
// labels the source in error messages (a file path, or "<embedded>").
func loadConfig(name string, r io.Reader) (Config, error) {
	cfg := DefaultConfig()
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || s[0] == '#' {
			continue
		}
		sep := strings.IndexAny(s, ":=")
		if sep < 0 {
			return cfg, fmt.Errorf("%s:%d: no ':' or '=' in %q", name, line, s)
		}
		key := strings.ToLower(strings.TrimSpace(s[:sep]))
		val := strings.TrimSpace(s[sep+1:])
		if err := applyDirective(&cfg, key, val); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", name, line, err)
		}
	}
	return cfg, sc.Err()
}

func applyDirective(cfg *Config, key, val string) error {
	switch key {
	case "server":
		cfg.Server = val
	case "phobosname", "name":
		cfg.Name = val
	case "skin":
		cfg.Skin = val
	case "password":
		cfg.Password = val
	case "spectator":
		cfg.Spectator = val
	case "controllername", "controller":
		cfg.Controller = val
	case "controllersequence", "authsequence":
		seq, err := ParseSequence(val)
		if err != nil {
			return err
		}
		cfg.AuthSequence = seq
	case "messagefile":
		cfg.MessageFile = val
	case "controllercheck":
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("ControllerCheck: %w", err)
		}
		cfg.ControllerCheck = d
	case "cmdmethod":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("CmdMethod: %w", err)
		}
		cfg.CmdMethod = n
	case "verbose":
		cfg.Verbose = val == "1" || strings.EqualFold(val, "true")
	default:
		return fmt.Errorf("unknown directive %q", key)
	}
	return nil
}

// CmdMethodName labels the (reserved) command encoding.
func CmdMethodName(m int) string {
	switch m {
	case 1:
		return "morse"
	case 2:
		return "base4"
	}
	return "unknown"
}

// SampleConfig is a ready-to-edit phobos.cfg shown when none exists.
const SampleConfig = `PhobosName:         Phobos
ControllerName:     Player
# 3 rocket shots, 1 jump, then 2 rocket shots
ControllerSequence: ROCKET3,JUMP,ROCKET2
# 1 for morse, 2 for base4 (reserved for the command channel)
CmdMethod:          1
Server:             127.0.0.1
# decoded messages are appended here when the controller ends a transmission
MessageFile:        phobos.txt`

// writeFile is a tiny helper so tests need no extra imports.
func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }
