// Command fsnotify watches one or more paths and prints file system
// events to stdout, or runs a shell command on each event.
//
// Usage:
//
//	fsnotify [flags] PATH [PATH...]
//
// Flags:
//
//	-r          watch each PATH recursively
//	-V          verbose log to stderr (registrations, signals, exec exits)
//	-e CMD      run CMD via the platform shell on every event
//	            (sh -c on Unix, cmd /C on Windows). The child receives
//	            FSNOTIFY_PATH and FSNOTIFY_OP in its environment.
//	-json       emit each event as one NDJSON object on stdout instead
//	            of the default "OP\tPATH" line. Mutually exclusive with -e.
//
// Default output is one line per event: "OP\tPATH". Watcher errors are
// always written to stderr.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/gofsnotify/fsnotify"
)

var (
	verbose   = flag.Bool("V", false, "verbose log to stderr")
	recursive = flag.Bool("r", false, "watch each PATH recursively")
	execCmd   = flag.String("e", "", "shell command to run on each event")
	jsonOut   = flag.Bool("json", false, "emit each event as a JSON object on stdout")
)

func main() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage: %s [flags] PATH [PATH...]\n\n", filepathBase(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	log.SetFlags(0)
	log.SetPrefix("fsnotify: ")
	log.SetOutput(os.Stderr)

	if *execCmd != "" && *jsonOut {
		log.Fatal("-e and -json are mutually exclusive")
	}
	jsonEnc := json.NewEncoder(os.Stdout)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	add := w.Add
	if *recursive {
		add = w.AddRecursive
	}
	for _, p := range paths {
		if err := add(p, fsnotify.All); err != nil {
			log.Fatalf("add %s: %v", p, err)
		}
		if *verbose {
			log.Printf("watching %s (recursive=%v)", p, *recursive)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if *verbose {
				log.Printf("event %s %s", ev.Op, ev.Name)
			}
			switch {
			case *execCmd != "":
				runCommand(*execCmd, ev)
			case *jsonOut:
				if err := jsonEnc.Encode(jsonEvent{Op: ev.Op.String(), Path: ev.Name}); err != nil {
					log.Printf("json encode: %v", err)
				}
			default:
				fmt.Printf("%s\t%s\n", ev.Op, ev.Name)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("error: %v", err)
		case s := <-sigCh:
			if *verbose {
				log.Printf("signal %v, exiting", s)
			}
			return
		}
	}
}

// jsonEvent is the wire shape emitted by -json. Op is the same
// pipe-joined form as the default text output ("CREATE|WRITE") so
// downstream consumers parse one stable representation across modes.
type jsonEvent struct {
	Op   string `json:"op"`
	Path string `json:"path"`
}

// runCommand executes cmd through the platform's shell so users can
// pipe, redirect, and use environment variables naturally. The event
// is exposed via FSNOTIFY_PATH and FSNOTIFY_OP so the command does not
// need to parse anything.
func runCommand(cmd string, ev fsnotify.Event) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/C", cmd)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = append(os.Environ(),
		"FSNOTIFY_PATH="+ev.Name,
		"FSNOTIFY_OP="+ev.Op.String(),
	)
	if err := c.Run(); err != nil {
		if *verbose {
			log.Printf("exec %q: %v", cmd, err)
		}
	}
}

// filepathBase trims the directory portion of a program path without
// pulling in path/filepath just for the usage line.
func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
