// phobosbuilda - build a self-contained Phobos controller from a phobos.cfg.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	phobos "phobophobia"
)

func main() {
	fs := flag.NewFlagSet("phobosbuilda", flag.ExitOnError)
	out := fs.String("o", "phobos", "output binary path")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: phobosbuilda [options] [phobos.cfg]

Build a self-contained Phobos controller binary with the given phobos.cfg
embedded in it. Defaults to ./phobos.cfg -> ./phobos.

options:
`)
		fs.PrintDefaults()
	}

	// Allow flags and the positional config path to be interspersed, so both
	// "phobosbuilda -o bot my.cfg" and "phobosbuilda my.cfg -o bot" work.
	cfgPath := ""
	args := os.Args[1:]
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		if fs.NArg() == 0 {
			break
		}
		if cfgPath == "" {
			cfgPath = fs.Arg(0)
		}
		args = fs.Args()[1:]
	}
	if cfgPath == "" {
		cfgPath = "phobos.cfg"
	}

	if err := run(cfgPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "phobosbuilda:", err)
		os.Exit(1)
	}
}

func run(cfgPath, out string) error {
	// 1. Read the config the user wants embedded.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s not found. Create one, e.g.:\n\n%s", cfgPath, phobos.SampleConfig)
		}
		return err
	}

	// 2. Validate it now, so build failures are about code, not config.
	cfg, err := phobos.LoadConfigString(string(data))
	if err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// 3. Resolve the output path against the current directory before we move
	//    into the module root to run the compiler.
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return err
	}

	// 4. Locate the module root and the phobos command's embed slot.
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	embedPath := filepath.Join(root, "cmd", "phobos", "phobos.cfg")
	if _, statErr := os.Stat(filepath.Join(root, "cmd", "phobos", "main.go")); statErr != nil {
		return fmt.Errorf("cmd/phobos not found under %s: %w", root, statErr)
	}

	// 5. Swap in the user's config, build, and always restore the default.
	original, err := os.ReadFile(embedPath)
	if err != nil {
		return fmt.Errorf("reading embed slot %s: %w", embedPath, err)
	}
	if err := os.WriteFile(embedPath, data, 0o644); err != nil {
		return fmt.Errorf("writing embed slot: %w", err)
	}
	defer os.WriteFile(embedPath, original, 0o644)

	build := exec.Command("go", "build", "-o", outAbs, "./cmd/phobos")
	build.Dir = root
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	// 6. Report what was baked in.
	fmt.Printf("built %s from %s\n", outAbs, cfgPath)
	fmt.Printf("  name:       %s\n", cfg.Name)
	fmt.Printf("  server:     %s\n", cfg.Server)
	fmt.Printf("  controller: %s\n", cfg.Controller)
	fmt.Printf("  sequence:   %s\n", phobos.SequenceString(cfg.AuthSequence))
	fmt.Printf("  cmd method: %d (%s)\n", cfg.CmdMethod, phobos.CmdMethodName(cfg.CmdMethod))
	if cfg.MessageFile != "" {
		fmt.Printf("  messages:   %s\n", cfg.MessageFile)
	}
	fmt.Printf("run it with: %s\n", out)
	return nil
}

// moduleRoot walks up from the working directory to the nearest go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s - run phobosbuilda inside the q2chat module", mustWd())
		}
		dir = parent
	}
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}
