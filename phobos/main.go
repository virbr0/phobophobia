// phobos - a self-contained Phobos controller.
package main

import (
	_ "embed"
	"fmt"
	"os"

	phobos "phobophobia"
)

var embeddedConfig string

func main() {
	cfg, err := phobos.LoadConfigString(embeddedConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "phobos: embedded config:", err)
		os.Exit(2)
	}

	fmt.Printf("# Phobos (config embedded at build time)\n")
	fmt.Printf("#   name:       %s\n", cfg.Name)
	fmt.Printf("#   server:     %s\n", cfg.Server)
	fmt.Printf("#   controller: %s\n", cfg.Controller)
	fmt.Printf("#   sequence:   %s\n", phobos.SequenceString(cfg.AuthSequence))
	fmt.Printf("#   cmd method: %d (%s)\n", cfg.CmdMethod, phobos.CmdMethodName(cfg.CmdMethod))
	if cfg.MessageFile != "" {
		fmt.Printf("#   messages:   %s\n", cfg.MessageFile)
	}

	c := phobos.NewClient(cfg, os.Stdout)
	if err := phobos.RunInteractive(c); err != nil {
		fmt.Fprintln(os.Stderr, "phobos:", err)
		os.Exit(1)
	}
}
