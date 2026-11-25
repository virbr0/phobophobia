package phobos

// run.go - the interactive front-end shared by the q2chat and phobosbuilda
// commands: feed stdin lines to the client and stop cleanly on Ctrl-C.

import (
	"bufio"
	"os"
	"os/signal"
	"syscall"
)

// RunInteractive connects c and relays stdin to it until the server drops us,
// stdin closes, or the process is interrupted.
func RunInteractive(c *Client) error {
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()

	input := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			input <- sc.Text()
		}
		close(input)
	}()
	return c.Run(stop, input)
}
