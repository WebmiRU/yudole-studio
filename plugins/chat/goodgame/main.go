// Command goodgame is the GoodGame chat plugin for studio.
//
// The kernel launches it and talks over stdin/stdout, one message per line.
// For now the plugin only announces itself; commands from the kernel are
// answered so the channel can be checked end to end.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	greeting  = "Hello from goodgame plugin"
	announce  = 3 * time.Second
	scanLimit = 64 * 1024
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands := commandsFrom(ctx, os.Stdin)
	ticker := time.NewTicker(announce)
	defer ticker.Stop()

	say(greeting)

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			say(greeting)

		case line, ok := <-commands:
			if !ok {
				// The kernel closed stdin: nothing left to talk to.
				return
			}
			respond(line)
		}
	}
}

// commandsFrom reads the kernel's commands, one per line. The channel is
// closed when stdin is closed or the context is cancelled.
func commandsFrom(ctx context.Context, in *os.File) <-chan string {
	out := make(chan string)

	go func() {
		defer close(out)

		lines := make(chan string)
		go func() {
			defer close(lines)

			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), scanLimit)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				select {
				case out <- line:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out
}

func respond(line string) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "ping":
		say("pong")
	case "quit", "exit":
		say("bye")
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "goodgame: unknown command %q\n", line)
	}
}

func say(message string) {
	fmt.Fprintln(os.Stdout, message)
}
